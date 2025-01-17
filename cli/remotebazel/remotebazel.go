package remotebazel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/cachetools"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/digest"
	"github.com/buildbuddy-io/buildbuddy/server/util/grpc_client"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/metadata"

	bblog "github.com/buildbuddy-io/buildbuddy/cli/logging"
	bespb "github.com/buildbuddy-io/buildbuddy/proto/build_event_stream"
	bbspb "github.com/buildbuddy-io/buildbuddy/proto/buildbuddy_service"
	elpb "github.com/buildbuddy-io/buildbuddy/proto/eventlog"
	inpb "github.com/buildbuddy-io/buildbuddy/proto/invocation"
	rnpb "github.com/buildbuddy-io/buildbuddy/proto/runner"
	bspb "google.golang.org/genproto/googleapis/bytestream"
)

const (
	buildBuddyArtifactDir = "bb-out"

	escapeSeq                  = "\u001B["
	gitConfigSection           = "buildbuddy"
	gitConfigRemoteBazelRemote = "remote-bazel-remote-name"
)

var (
	execOs            = flag.String("os", "linux", "If set, requests execution on a specific OS.")
	execArch          = flag.String("arch", "amd64", "If set, requests execution on a specific CPU architecture.")
	defaultBranchRefs = []string{"refs/heads/main", "refs/heads/master"}
)

func consoleCursorMoveUp(y int) {
	fmt.Print(escapeSeq + strconv.Itoa(y) + "A")
}

func consoleCursorMoveBeginningLine() {
	fmt.Print(escapeSeq + "1G")
}

func consoleDeleteLines(n int) {
	fmt.Print(escapeSeq + strconv.Itoa(n) + "M")
}

type RunOpts struct {
	Server            string
	APIKey            string
	Args              []string
	WorkspaceFilePath string
	SidecarSocket     string
}

type RepoConfig struct {
	Root      string
	URL       string
	CommitSHA string
	Patches   [][]byte
}

func determineRemote(repo *git.Repository) (*git.Remote, error) {
	remotes, err := repo.Remotes()
	if err != nil {
		return nil, err
	}

	if len(remotes) == 0 {
		return nil, status.FailedPreconditionError("the git repository must have a remote configured to use remote Bazel")
	}

	if len(remotes) == 1 {
		return remotes[0], nil
	}

	conf, err := repo.Config()
	if err != nil {
		return nil, err
	}
	confRemote := conf.Raw.Section(gitConfigSection).Option(gitConfigRemoteBazelRemote)
	if confRemote != "" {
		r, err := repo.Remote(confRemote)
		if err == nil {
			return r, nil
		}
		bblog.Printf("Could not find remote %q saved in config, ignoring", confRemote)
	}

	var remoteNames []string
	for _, r := range remotes {
		remoteNames = append(remoteNames, fmt.Sprintf("%s (%s)", r.Config().Name, r.Config().URLs[0]))
	}

	selectedRemoteAndURL := ""
	prompt := &survey.Select{
		Message: "Select the git remote that will be used by the remote Bazel instance to fetch your repo:",
		Options: remoteNames,
	}
	if err := survey.AskOne(prompt, &selectedRemoteAndURL); err != nil {
		return nil, err
	}

	selectedRemote := strings.Split(selectedRemoteAndURL, " (")[0]
	remote, err := repo.Remote(selectedRemote)
	if err != nil {
		return nil, err
	}

	conf.Raw.Section(gitConfigSection).SetOption(gitConfigRemoteBazelRemote, selectedRemote)
	if err := repo.SetConfig(conf); err != nil {
		return nil, err
	}

	return remote, nil
}

func determineDefaultBranch(repo *git.Repository) (string, error) {
	branches, err := repo.Branches()
	if err != nil {
		return "", status.UnknownErrorf("could not list branches: %s", err)
	}

	allBranches := make(map[string]struct{})
	err = branches.ForEach(func(branch *plumbing.Reference) error {
		allBranches[string(branch.Name())] = struct{}{}
		return nil
	})
	if err != nil {
		return "", status.UnknownErrorf("could not iterate over branches: %s", err)
	}

	for _, defaultBranch := range defaultBranchRefs {
		if _, ok := allBranches[defaultBranch]; ok {
			return defaultBranch, nil
		}
	}

	return "", status.NotFoundErrorf("could not determine default branch")
}

func runGit(args ...string) (string, error) {
	stdout := bytes.Buffer{}
	stderr := bytes.Buffer{}
	cmd := exec.Command("git", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return stdout.String(), err
		}
		return stdout.String(), status.UnknownErrorf("error running git %s: %s\n%s", args, err, stderr.String())
	}
	return stdout.String(), nil
}

func diffUntrackedFile(path string) (string, error) {
	patch, err := runGit("diff", "--no-index", "/dev/null", path)
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return patch, nil
		}
		return "", err
	}

	return patch, nil
}

func Config(path string) (*RepoConfig, error) {
	repo, err := git.PlainOpenWithOptions(path, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return nil, err
	}

	remote, err := determineRemote(repo)
	if err != nil {
		return nil, err
	}
	if len(remote.Config().URLs) == 0 {
		return nil, status.FailedPreconditionErrorf("remote %q does not have a fetch URL", remote.Config().Name)
	}
	fetchURL := remote.Config().URLs[0]

	bblog.Printf("Using fetch URL: %s", fetchURL)

	defaultBranchRef, err := determineDefaultBranch(repo)
	if err != nil {
		return nil, err
	}

	bblog.Printf("Using base branch: %s", defaultBranchRef)

	defaultBranchCommitHash, err := repo.ResolveRevision(plumbing.Revision(defaultBranchRef))
	if err != nil {
		return nil, status.UnknownErrorf("could not find commit hash for branch ref %q", defaultBranchRef)
	}

	bblog.Printf("Using base branch commit hash: %s", defaultBranchCommitHash)

	wt, err := repo.Worktree()
	if err != nil {
		return nil, status.UnknownErrorf("could not determine git repo root")
	}

	repoConfig := &RepoConfig{
		Root:      wt.Filesystem.Root(),
		URL:       fetchURL,
		CommitSHA: defaultBranchCommitHash.String(),
	}

	patch, err := runGit("diff", defaultBranchCommitHash.String())
	if err != nil {
		return nil, err
	}
	if patch != "" {
		repoConfig.Patches = append(repoConfig.Patches, []byte(patch))
	}

	untrackedFiles, err := runGit("ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	untrackedFiles = strings.Trim(untrackedFiles, "\n")
	if untrackedFiles != "" {
		for _, uf := range strings.Split(untrackedFiles, "\n") {
			if strings.HasPrefix(uf, buildBuddyArtifactDir+"/") {
				continue
			}
			patch, err := diffUntrackedFile(uf)
			if err != nil {
				return nil, err
			}
			repoConfig.Patches = append(repoConfig.Patches, []byte(patch))
		}
	}

	return repoConfig, nil
}

func getTermWidth() int {
	size, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 80
	}
	return int(size.Col)
}

func splitLogBuffer(buf []byte) []string {
	var lines []string

	termWidth := getTermWidth()
	for _, line := range strings.Split(string(buf), "\n") {
		for len(line) > termWidth {
			lines = append(lines, line[0:termWidth])
			line = line[termWidth:]
		}
		lines = append(lines, line)
	}
	return lines
}

func streamLogs(ctx context.Context, bbClient bbspb.BuildBuddyServiceClient, invocationID string) error {
	chunkID := ""
	moveBack := 0

	drawChunk := func(chunk *elpb.GetEventLogChunkResponse) {
		// Are we redrawing the current chunk?
		if moveBack > 0 {
			consoleCursorMoveUp(moveBack)
			consoleCursorMoveBeginningLine()
			consoleDeleteLines(moveBack)
		}

		logLines := splitLogBuffer(chunk.GetBuffer())
		if !chunk.GetLive() {
			moveBack = 0
		} else {
			moveBack = len(logLines)
		}

		for _, l := range logLines {
			_, _ = os.Stdout.Write([]byte(l))
			_, _ = os.Stdout.Write([]byte("\n"))
		}
	}

	var chunks []*elpb.GetEventLogChunkResponse
	wasLive := false
	for {
		l, err := bbClient.GetEventLogChunk(ctx, &elpb.GetEventLogChunkRequest{
			InvocationId: invocationID,
			ChunkId:      chunkID,
			MinLines:     100,
		})
		if err != nil {
			return status.UnknownErrorf("error streaming logs: %s", err)
		}

		chunks = append(chunks, l)
		// If the current chunk was live but is no longer then delay redraw
		// until the next chunk is retrieved. The "volatile" part of the
		// chunk moves to the next chunk when a chunk is finalized. Without
		// the delay, we would print the chunk without the volatile portion
		// which will look like a "flicker" once the volatile portion is
		// printed again.
		if !wasLive || l.GetLive() {
			for _, chunk := range chunks {
				drawChunk(chunk)
			}
			chunks = nil
		}
		wasLive = l.GetLive()

		if l.GetNextChunkId() == "" {
			break
		}

		if l.GetNextChunkId() == chunkID {
			time.Sleep(1 * time.Second)
		}
		chunkID = l.GetNextChunkId()
	}
	return nil
}

func downloadOutput(ctx context.Context, bsClient bspb.ByteStreamClient, resourceName string, outFile string) error {
	if err := os.MkdirAll(filepath.Dir(outFile), 0755); err != nil {
		return err
	}
	out, err := os.Create(outFile)
	if err != nil {
		return err
	}
	defer out.Close()
	rn, err := digest.ParseDownloadResourceName(resourceName)
	if err != nil {
		return err
	}
	if err := cachetools.GetBlob(ctx, bsClient, rn, out); err != nil {
		return err
	}
	return nil
}

// TODO(vadim): add interactive progress bar for downloads
// TODO(vadim): parallelize downloads
func downloadOutputs(ctx context.Context, bbClient bbspb.BuildBuddyServiceClient, bsClient bspb.ByteStreamClient, invocationID string, outputBaseDir string) ([]string, error) {
	childInRsp, err := bbClient.GetInvocation(ctx, &inpb.GetInvocationRequest{Lookup: &inpb.InvocationLookup{InvocationId: invocationID}})
	if err != nil {
		return nil, fmt.Errorf("could not retrieve child invocation: %s", err)
	}

	fileSets := make(map[string][]*bespb.File)
	fileSetsToFetch := make(map[string]struct{})
	for _, e := range childInRsp.GetInvocation()[0].GetEvent() {
		switch t := e.GetBuildEvent().GetPayload().(type) {
		case *bespb.BuildEvent_NamedSetOfFiles:
			fileSets[e.GetBuildEvent().GetId().GetNamedSet().GetId()] = t.NamedSetOfFiles.GetFiles()
		case *bespb.BuildEvent_Completed:
			for _, og := range t.Completed.GetOutputGroup() {
				for _, fs := range og.GetFileSets() {
					fileSetsToFetch[fs.GetId()] = struct{}{}
				}
			}
		}
	}

	var localArtifacts []string
	for fsID := range fileSetsToFetch {
		fs, ok := fileSets[fsID]
		if !ok {
			return nil, fmt.Errorf("could not find file set with ID %q while fetching outputs", fsID)
		}
		for _, f := range fs {
			u, err := url.Parse(f.GetUri())
			if err != nil {
				bblog.Printf("Invocation contains an output with an invalid URI %q", f.GetUri())
				continue
			}
			r := strings.TrimPrefix(u.RequestURI(), "/")
			outFile := filepath.Join(outputBaseDir, buildBuddyArtifactDir)
			for _, p := range f.GetPathPrefix() {
				outFile = filepath.Join(outFile, p)
			}
			outFile = filepath.Join(outFile, f.GetName())
			if err := downloadOutput(ctx, bsClient, r, outFile); err != nil {
				return nil, err
			}
			localArtifacts = append(localArtifacts, outFile)
		}
	}

	// Format as relative paths with indentation for human consumption.
	var relArtifacts []string
	for _, a := range localArtifacts {
		rp, err := filepath.Rel(outputBaseDir, a)
		if err != nil {
			return nil, err
		}
		relArtifacts = append(relArtifacts, "  "+rp)
	}
	fmt.Printf("Downloaded artifacts:\n%s\n", strings.Join(relArtifacts, "\n"))
	return localArtifacts, nil
}

func Run(ctx context.Context, opts RunOpts, repoConfig *RepoConfig) (int, error) {
	conn, err := grpc_client.DialTarget(opts.Server)
	if err != nil {
		return 0, status.UnavailableErrorf("could not connect to BuildBuddy remote bazel service %q: %s", opts.Server, err)
	}
	bbClient := bbspb.NewBuildBuddyServiceClient(conn)

	ctx = metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-api-key", opts.APIKey)

	bblog.Printf("Requesting command execution on remote Bazel instance.")

	instanceHash := sha256.New()
	instanceHash.Write(uuid.NodeID())
	instanceHash.Write([]byte(repoConfig.Root))

	reqOS := runtime.GOOS
	if *execOs != "" {
		reqOS = *execOs
	}
	reqArch := runtime.GOARCH
	if *execArch != "" {
		reqArch = *execArch
	}

	fetchOutputs := false
	if len(opts.Args) > 0 && (opts.Args[0] == "build" || opts.Args[0] == "run") {
		fetchOutputs = true
	}
	runOutput := false
	if len(opts.Args) > 0 && opts.Args[0] == "run" {
		opts.Args[0] = "build"
		runOutput = true
	}

	req := &rnpb.RunRequest{
		GitRepo: &rnpb.RunRequest_GitRepo{
			RepoUrl: repoConfig.URL,
		},
		RepoState: &rnpb.RunRequest_RepoState{
			CommitSha: repoConfig.CommitSHA,
		},
		SessionAffinityKey: fmt.Sprintf("%x", instanceHash.Sum(nil)),
		BazelCommand:       strings.Join(opts.Args, " "),
		Os:                 reqOS,
		Arch:               reqArch,
	}

	for _, patch := range repoConfig.Patches {
		req.GetRepoState().Patch = append(req.GetRepoState().Patch, patch)
	}

	rsp, err := bbClient.Run(ctx, req)
	if err != nil {
		return 0, status.UnknownErrorf("error running bazel: %s", err)
	}

	iid := rsp.GetInvocationId()

	bblog.Printf("Invocation ID: %s", iid)

	if err := streamLogs(ctx, bbClient, iid); err != nil {
		return 0, err
	}

	inRsp, err := bbClient.GetInvocation(ctx, &inpb.GetInvocationRequest{Lookup: &inpb.InvocationLookup{InvocationId: iid}})
	if err != nil {
		return 0, fmt.Errorf("could not retrieve invocation: %s", err)
	}
	if len(inRsp.GetInvocation()) == 0 {
		return 0, fmt.Errorf("invocation not found")
	}

	childIID := ""
	exitCode := -1
	for _, e := range inRsp.GetInvocation()[0].GetEvent() {
		if cic, ok := e.GetBuildEvent().GetPayload().(*bespb.BuildEvent_ChildInvocationCompleted); ok {
			childIID = e.GetBuildEvent().GetId().GetChildInvocationCompleted().GetInvocationId()
			exitCode = int(cic.ChildInvocationCompleted.ExitCode)
		}
	}

	if exitCode == -1 {
		return 0, fmt.Errorf("could not determine remote Bazel exit code")
	}

	if fetchOutputs {
		conn, err := grpc_client.DialTarget("unix://" + opts.SidecarSocket)
		if err != nil {
			return 0, fmt.Errorf("could not communicate with sidecar: %s", err)
		}
		bsClient := bspb.NewByteStreamClient(conn)
		ctx = metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-api-key", opts.APIKey)
		outputs, err := downloadOutputs(ctx, bbClient, bsClient, childIID, filepath.Dir(opts.WorkspaceFilePath))
		if err != nil {
			return 0, err
		}
		if runOutput {
			if len(outputs) > 1 {
				return 0, fmt.Errorf("run requested but target produced more than one artifact")
			}
			binPath := outputs[0]
			if err := os.Chmod(binPath, 0755); err != nil {
				return 0, fmt.Errorf("could not prepare binary %q for execution: %s", binPath, err)
			}
			bblog.Printf("Executing %q", binPath)
			cmd := exec.CommandContext(ctx, binPath)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err = cmd.Run()
			if e, ok := err.(*exec.ExitError); ok {
				return e.ExitCode(), nil
			}
			return 0, err
		}
	}

	return exitCode, nil
}
