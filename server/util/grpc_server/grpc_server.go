package grpc_server

import (
	"context"
	"flag"
	"net/http"
	"strings"
	"time"

	"github.com/buildbuddy-io/buildbuddy/server/environment"
	"github.com/buildbuddy-io/buildbuddy/server/rpc/filters"
	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
)

var (
	gRPCOverHTTPPortEnabled = flag.Bool("app.grpc_over_http_port_enabled", false, "Cloud-Only")
	// Support large BEP messages: https://github.com/bazelbuild/bazel/issues/12050
	gRPCMaxRecvMsgSizeBytes = flag.Int("app.grpc_max_recv_msg_size_bytes", 50000000, "Configures the max GRPC receive message size [bytes]")
)

func GRPCShutdown(ctx context.Context, grpcServer *grpc.Server) error {
	// Attempt to graceful stop this grpcServer. Graceful stop will
	// disallow new connections, but existing ones are allowed to
	// finish. To ensure this doesn't hang forever, we also kick off
	// a goroutine that will hard stop the server 100ms before the
	// shutdown function deadline.
	deadline, ok := ctx.Deadline()
	if !ok {
		grpcServer.Stop()
		return nil
	}
	delay := deadline.Sub(time.Now()) - (100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(ctx, delay)
	go func() {
		select {
		case <-ctx.Done():
			log.Infof("Graceful stop of GRPC server succeeded.")
			grpcServer.Stop()
		case <-time.After(delay):
			log.Warningf("Hard-stopping GRPC Server!")
			grpcServer.Stop()
		}
	}()
	grpcServer.GracefulStop()
	cancel()
	return nil
}

func GRPCShutdownFunc(grpcServer *grpc.Server) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		return GRPCShutdown(ctx, grpcServer)
	}
}

func CommonGRPCServerOptions(env environment.Env) []grpc.ServerOption {
	return []grpc.ServerOption{
		filters.GetUnaryInterceptor(env),
		filters.GetStreamInterceptor(env),
		grpc.StreamInterceptor(grpc_prometheus.StreamServerInterceptor),
		grpc.UnaryInterceptor(grpc_prometheus.UnaryServerInterceptor),
		grpc.MaxRecvMsgSize(*gRPCMaxRecvMsgSizeBytes),
		// Set to avoid errors: Bandwidth exhausted HTTP/2 error code: ENHANCE_YOUR_CALM Received Goaway too_many_pings
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second, // If a client pings more than once every 10 seconds, terminate the connection
			PermitWithoutStream: true,             // Allow pings even when there are no active streams
		}),
	}
}

func MaxRecvMsgSizeBytes() int {
	return *gRPCMaxRecvMsgSizeBytes
}

func ServeGRPCOverHTTPPort(grpcServer *grpc.Server, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *gRPCOverHTTPPortEnabled && r.ProtoMajor == 2 && strings.HasPrefix(
			r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
		} else {
			next.ServeHTTP(w, r)
		}
	})
}
