package executor

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/auth"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/commandutil"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/operation"
	"github.com/buildbuddy-io/buildbuddy/server/environment"
	"github.com/buildbuddy-io/buildbuddy/server/interfaces"
	"github.com/buildbuddy-io/buildbuddy/server/metrics"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/cachetools"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/digest"
	"github.com/buildbuddy-io/buildbuddy/server/util/alert"
	"github.com/buildbuddy-io/buildbuddy/server/util/background"
	"github.com/buildbuddy-io/buildbuddy/server/util/disk"
	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/buildbuddy-io/buildbuddy/server/util/tracing"
	"github.com/buildbuddy-io/buildbuddy/server/util/uuid"
	"github.com/golang/protobuf/ptypes"
	"github.com/prometheus/client_golang/prometheus"

	espb "github.com/buildbuddy-io/buildbuddy/proto/execution_stats"
	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	durationpb "github.com/golang/protobuf/ptypes/duration"
	timestamppb "github.com/golang/protobuf/ptypes/timestamp"
	tspb "github.com/golang/protobuf/ptypes/timestamp"
	gcodes "google.golang.org/grpc/codes"
	gstatus "google.golang.org/grpc/status"
)

const (
	// Messages are typically sent back to the client on state changes.
	// During very long build steps (running large tests, linking large
	// objects, etc) no progress can be returned for a very long time.
	// To ensure we keep the connection alive, we start a timer and
	// just repeat the last state change message after every
	// execProgressCallbackPeriod. If this is set to 0, it is disabled.
	execProgressCallbackPeriod = 60 * time.Second

	// 7 days? Forever. This is the duration returned when no max duration
	// has been set in the config and no timeout was set in the client
	// request. It's basically the same as "no-timeout".
	infiniteDuration = time.Hour * 24 * 7
	// Allowed deadline extension for uploading action outputs.
	// The deadline of the original request may be extended by up to this amount
	// in order to give enough time to upload action outputs.
	uploadDeadlineExtension = time.Minute * 1
)

type Executor struct {
	env        environment.Env
	runnerPool interfaces.RunnerPool
	id         string
	hostID     string
}

type Options struct {
	// TESTING ONLY: allows the name of the executor to be manually specified instead of deriving it
	// from host information.
	NameOverride string
}

func NewExecutor(env environment.Env, id string, runnerPool interfaces.RunnerPool, options *Options) (*Executor, error) {
	executorConfig := env.GetConfigurator().GetExecutorConfig()
	if executorConfig == nil {
		return nil, status.FailedPreconditionError("No executor config found")
	}
	if err := disk.EnsureDirectoryExists(executorConfig.GetRootDirectory()); err != nil {
		return nil, err
	}
	hostID := options.NameOverride
	if hostID == "" {
		if h, err := uuid.GetHostID(); err == nil {
			hostID = h
		} else {
			log.Warningf("Unable to get stable BuildBuddy HostID. Falling back to failsafe ID. %s", err)
			hostID = uuid.GetFailsafeHostID()
		}
	}
	return &Executor{
		env:        env,
		id:         id,
		hostID:     hostID,
		runnerPool: runnerPool,
	}, nil
}

func (s *Executor) HostID() string {
	return s.hostID
}

func (s *Executor) Warmup() {
	s.runnerPool.Warmup(context.Background())
}

func diffTimestamps(startPb, endPb *tspb.Timestamp) time.Duration {
	start, _ := ptypes.Timestamp(startPb)
	end, _ := ptypes.Timestamp(endPb)
	return end.Sub(start)
}

func diffTimestampsToProto(startPb, endPb *tspb.Timestamp) *durationpb.Duration {
	return ptypes.DurationProto(diffTimestamps(startPb, endPb))
}

func logActionResult(taskID string, md *repb.ExecutedActionMetadata) {
	workTime := diffTimestamps(md.GetWorkerStartTimestamp(), md.GetWorkerCompletedTimestamp())
	fetchTime := diffTimestamps(md.GetInputFetchStartTimestamp(), md.GetInputFetchCompletedTimestamp())
	execTime := diffTimestamps(md.GetExecutionStartTimestamp(), md.GetExecutionCompletedTimestamp())
	uploadTime := diffTimestamps(md.GetOutputUploadStartTimestamp(), md.GetOutputUploadCompletedTimestamp())
	log.Debugf("%q completed action %q [work: %02dms, fetch: %02dms, exec: %02dms, upload: %02dms]",
		md.GetWorker(), taskID, workTime.Milliseconds(), fetchTime.Milliseconds(),
		execTime.Milliseconds(), uploadTime.Milliseconds())
}

func timevalDuration(tv syscall.Timeval) time.Duration {
	return time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
}

func parseTimeout(timeout *durationpb.Duration, maxDuration time.Duration) (time.Duration, error) {
	if timeout == nil {
		if maxDuration == 0 {
			return infiniteDuration, nil
		}
		return maxDuration, nil
	}
	requestDuration, err := ptypes.Duration(timeout)
	if err != nil {
		return 0, status.InvalidArgumentErrorf("Unparsable timeout: %s", err.Error())
	}
	if maxDuration != 0 && requestDuration > maxDuration {
		return 0, status.InvalidArgumentErrorf("Specified timeout (%s) longer than allowed maximum (%s).", requestDuration, maxDuration)
	}
	return requestDuration, nil
}

func isBazelRetryableError(taskError error) bool {
	if gstatus.Code(taskError) == gcodes.ResourceExhausted {
		return true
	}
	if gstatus.Code(taskError) == gcodes.FailedPrecondition {
		if len(gstatus.Convert(taskError).Details()) > 0 {
			return true
		}
	}
	return false
}

func shouldRetry(taskError error) bool {
	return !isBazelRetryableError(taskError)
}

func (s *Executor) ExecuteTaskAndStreamResults(ctx context.Context, task *repb.ExecutionTask, stream operation.StreamLike) (retry bool, err error) {
	// From here on in we use these liberally, so check that they are setup properly
	// in the environment.
	if s.env.GetActionCacheClient() == nil || s.env.GetByteStreamClient() == nil || s.env.GetContentAddressableStorageClient() == nil {
		return false, status.FailedPreconditionError("No connection to cache backend.")
	}

	ctx, span := tracing.StartSpan(ctx)
	defer span.End()

	metrics.RemoteExecutionTasksStartedCount.Inc()

	req := task.GetExecuteRequest()
	taskID := task.GetExecutionId()
	adInstanceDigest := digest.NewResourceName(req.GetActionDigest(), req.GetInstanceName())

	acClient := s.env.GetActionCacheClient()

	stateChangeFn := operation.GetStateChangeFunc(stream, taskID, adInstanceDigest)
	finishWithErrFn := func(finalErr error) (retry bool, err error) {
		if shouldRetry(finalErr) {
			return true, finalErr
		}
		if err := operation.PublishOperationDone(stream, taskID, adInstanceDigest, finalErr); err != nil {
			return true, err
		}
		return false, finalErr
	}

	md := &repb.ExecutedActionMetadata{
		Worker:               s.hostID,
		QueuedTimestamp:      task.QueuedTimestamp,
		WorkerStartTimestamp: ptypes.TimestampNow(),
		ExecutorId:           s.id,
	}

	if !req.GetSkipCacheLookup() {
		if err := stateChangeFn(repb.ExecutionStage_CACHE_CHECK, operation.InProgressExecuteResponse()); err != nil {
			return true, err
		}
		actionResult, err := cachetools.GetActionResult(ctx, acClient, adInstanceDigest)
		if err == nil {
			if err := stateChangeFn(repb.ExecutionStage_COMPLETED, operation.ExecuteResponseWithCachedResult(actionResult)); err != nil {
				return true, err
			}
			return false, nil
		}
	}

	r, err := s.runnerPool.Get(ctx, task)
	if err != nil {
		return finishWithErrFn(status.UnavailableErrorf("Error creating runner for command: %s", err.Error()))
	}
	if err := r.PrepareForTask(ctx); err != nil {
		return finishWithErrFn(err)
	}

	finishedCleanly := false
	defer func() {
		ctx := context.Background()
		go s.runnerPool.TryRecycle(ctx, r, finishedCleanly)
	}()

	md.InputFetchStartTimestamp = ptypes.TimestampNow()

	ioStats := &espb.IOStats{}
	if err := r.DownloadInputs(ctx, ioStats); err != nil {
		return finishWithErrFn(err)
	}

	md.InputFetchCompletedTimestamp = ptypes.TimestampNow()

	if err := stateChangeFn(repb.ExecutionStage_EXECUTING, operation.InProgressExecuteResponse()); err != nil {
		return true, err
	}
	md.ExecutionStartTimestamp = ptypes.TimestampNow()
	maxDuration := infiniteDuration
	if currentDeadline, ok := ctx.Deadline(); ok {
		maxDuration = currentDeadline.Sub(time.Now())
	}
	execDuration, err := parseTimeout(task.GetAction().Timeout, maxDuration)
	if err != nil {
		// These errors are failure-specific. Pass through unchanged.
		return finishWithErrFn(err)
	}
	ctx, cancel := context.WithTimeout(ctx, execDuration)
	defer cancel()

	cmdResultChan := make(chan *interfaces.CommandResult, 1)
	go func() {
		cmdResultChan <- r.Run(ctx)
	}()

	// Run a timer that periodically sends update messages back
	// to our caller while execution is ongoing.
	updateTicker := time.NewTicker(execProgressCallbackPeriod)
	var cmdResult *interfaces.CommandResult
	for cmdResult == nil {
		select {
		case cmdResult = <-cmdResultChan:
			updateTicker.Stop()
		case <-updateTicker.C:
			if err := stateChangeFn(repb.ExecutionStage_EXECUTING, operation.InProgressExecuteResponse()); err != nil {
				return true, status.UnavailableErrorf("could not publish periodic execution update for %q: %s", taskID, err)
			}
		}
	}

	if cmdResult.ExitCode != 0 {
		log.Debugf("%q finished with non-zero exit code (%d). Err: %s, Stdout: %s, Stderr: %s", taskID, cmdResult.ExitCode, cmdResult.Error, cmdResult.Stdout, cmdResult.Stderr)
	}
	// Exit codes < 0 mean that the command either never started or was killed.
	// Make sure we return an error in this case.
	if cmdResult.ExitCode < 0 {
		cmdResult.Error = incompleteExecutionError(cmdResult.ExitCode, cmdResult.Error)
	}
	// If we know that the command terminated due to receiving SIGKILL, make sure
	// to retry, since this is not considered a clean termination.
	//
	// Other termination signals such as SIGABRT are treated as clean exits for
	// now, since some tools intentionally raise SIGABRT to cause a fatal exit:
	// https://github.com/bazelbuild/bazel/pull/14399
	if cmdResult.Error == commandutil.ErrSIGKILL {
		return finishWithErrFn(cmdResult.Error)
	}
	if cmdResult.Error != nil {
		log.Warningf("Command execution returned non-retriable error: %s", cmdResult.Error)
	}

	ctx, cancel = background.ExtendContextForFinalization(ctx, uploadDeadlineExtension)
	defer cancel()

	md.ExecutionCompletedTimestamp = ptypes.TimestampNow()
	md.OutputUploadStartTimestamp = ptypes.TimestampNow()

	actionResult := &repb.ActionResult{}
	actionResult.ExitCode = int32(cmdResult.ExitCode)

	if err := r.UploadOutputs(ctx, ioStats, actionResult, cmdResult); err != nil {
		return finishWithErrFn(status.UnavailableErrorf("Error uploading outputs: %s", err.Error()))
	}
	md.OutputUploadCompletedTimestamp = ptypes.TimestampNow()
	md.WorkerCompletedTimestamp = ptypes.TimestampNow()
	actionResult.ExecutionMetadata = md

	// If the action failed or do_not_cache is set, upload information about the error via a failed
	// ActionResult under an invocation-specific digest, which will not ever be seen by bazel but
	// may be viewed via the Buildbuddy UI.
	if task.GetAction().GetDoNotCache() || cmdResult.Error != nil || cmdResult.ExitCode != 0 {
		resultDigest, err := digest.AddInvocationIDToDigest(req.GetActionDigest(), task.GetInvocationId())
		if err != nil {
			return finishWithErrFn(status.UnavailableErrorf("Error uploading action result: %s", err.Error()))
		}
		adInstanceDigest = digest.NewResourceName(resultDigest, req.GetInstanceName())
	}
	if err := cachetools.UploadActionResult(ctx, acClient, adInstanceDigest, actionResult); err != nil {
		return finishWithErrFn(status.UnavailableErrorf("Error uploading action result: %s", err.Error()))
	}

	metrics.RemoteExecutionCount.With(prometheus.Labels{
		metrics.ExitCodeLabel: fmt.Sprintf("%d", actionResult.ExitCode),
	}).Inc()
	metrics.FileDownloadCount.Observe(float64(ioStats.FileDownloadCount))
	metrics.FileDownloadSizeBytes.Observe(float64(ioStats.FileDownloadSizeBytes))
	metrics.FileDownloadDurationUsec.Observe(float64(ioStats.FileDownloadDurationUsec))
	metrics.FileUploadCount.Observe(float64(ioStats.FileUploadCount))
	metrics.FileUploadSizeBytes.Observe(float64(ioStats.FileUploadSizeBytes))
	metrics.FileUploadDurationUsec.Observe(float64(ioStats.FileUploadDurationUsec))

	groupID := interfaces.AuthAnonymousUser
	if u, err := auth.UserFromTrustedJWT(ctx); err == nil {
		groupID = u.GetGroupID()
	}

	observeStageDuration(groupID, "queued", md.GetQueuedTimestamp(), md.GetWorkerStartTimestamp())
	observeStageDuration(groupID, "input_fetch", md.GetInputFetchStartTimestamp(), md.GetInputFetchCompletedTimestamp())
	observeStageDuration(groupID, "execution", md.GetExecutionStartTimestamp(), md.GetExecutionCompletedTimestamp())
	observeStageDuration(groupID, "output_upload", md.GetOutputUploadStartTimestamp(), md.GetOutputUploadCompletedTimestamp())
	observeStageDuration(groupID, "worker", md.GetWorkerStartTimestamp(), md.GetWorkerCompletedTimestamp())

	execSummary := &espb.ExecutionSummary{
		IoStats:                ioStats,
		ExecutedActionMetadata: md,
	}
	if err := stateChangeFn(repb.ExecutionStage_COMPLETED, operation.ExecuteResponseWithResult(actionResult, execSummary, cmdResult.Error)); err != nil {
		logActionResult(taskID, md)
		return finishWithErrFn(err) // CHECK (these errors should not happen).
	}
	if cmdResult.Error == nil {
		finishedCleanly = true
	}
	return false, nil
}

func incompleteExecutionError(exitCode int, err error) error {
	if err == nil {
		alert.UnexpectedEvent("incomplete_command_with_nil_error")
		return status.UnknownErrorf("Command did not complete, for unknown reasons (internal status %d)", exitCode)
	}
	// Ensure that if the command was not found, we return FAILED_PRECONDITION,
	// per RBE protocol. This is done because container/command implementations
	// don't really have a good way to distinguish this error from other types of
	// errors anyway (this is true for at least the bare and docker
	// implementations at time of writing)
	msg := status.Message(err)
	if strings.Contains(msg, "no such file or directory") {
		return status.FailedPreconditionError(msg)
	}
	return err
}

func observeStageDuration(groupID string, stage string, start *timestamppb.Timestamp, end *timestamppb.Timestamp) {
	startTime, err := ptypes.Timestamp(start)
	if err != nil {
		log.Warningf("Could not parse timestamp for '%s' stage: %s", stage, err)
		return
	}
	if startTime.IsZero() {
		return
	}
	endTime, err := ptypes.Timestamp(end)
	if err != nil {
		log.Warningf("Could not parse timestamp for '%s' stage: %s", stage, err)
		return
	}
	if endTime.IsZero() {
		return
	}
	duration := endTime.Sub(startTime)
	metrics.RemoteExecutionExecutedActionMetadataDurationsUsec.With(prometheus.Labels{
		metrics.GroupID:                  groupID,
		metrics.ExecutedActionStageLabel: stage,
	}).Observe(float64(duration / time.Microsecond))
}
