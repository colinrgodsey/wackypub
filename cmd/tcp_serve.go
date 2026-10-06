package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

// serveTokenEnv is the shared bearer secret for TCP serving. The protocol is
// unauthenticated at the gRPC layer (stdio never needed auth - the pipe is the
// boundary), so a TCP listener that is reachable by anything other than
// loopback REQUIRES this token. Loopback bindings may leave it unset and stay
// open to local processes - state the policy, don't surprise.
const serveTokenEnv = "WACKYPUB_SERVE_TOKEN"

// isLoopback reports whether addr binds a loopback interface (127.0.0.0/8 or ::1).
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsLoopback()
	}
	// Empty host (":8423") binds all interfaces - not loopback.
	return false
}

// tokenInterceptor enforces the bearer token for every RPC when one is set.
// When no token is configured (loopback default), no check is applied.
func tokenInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := checkToken(ctx, token); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// tokenStreamInterceptor enforces the bearer token for streaming RPCs.
func tokenStreamInterceptor(token string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := checkToken(ss.Context(), token); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func checkToken(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing bearer token")
	}
	vals := md.Get("authorization")
	for _, v := range vals {
		if v == "Bearer "+token {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "invalid bearer token")
}

// tcpServeCmd serves the AgentService protocol over a TCP listener (the
// network face of stdio-serve). The same RoutingServer registration carries
// over unchanged: multi-agent resolution (native vs bridged per agent) is the
// serve-side shape, identical to stdio-serve.
//
// This is the daemon deployment shape: systemd user unit, scripts, or remote
// clients sharing one runtime process. Session/lock semantics carry over from
// the per-process model: each agent turn takes the cross-process session flock
// (D117) under the SDK, so concurrent clients serialize per agent exactly as
// concurrent local processes do. Multiple clients sharing one server get the
// same guarantees as multiple processes sharing one workspace - documented,
// not invented here.
var tcpServeCmd = &cobra.Command{
	Use:   "tcp-serve [--listen :8423]",
	Short: "Serve the AgentService protocol over TCP (daemon lifecycle, network clients)",
	Long: `Serves the AgentService protocol over a TCP listener with the daemon lifecycle.

Start -\u003e serve -\u003e SIGTERM drains in-flight turns -\u003e exit. The workspace is
resolved from the current directory the same way every other wackypub command
resolves it (walk up for WACKYPUB_ROOT), so run this from the workspace root.

Auth: bind loopback by default and remain open to local processes. Binding a
non-loopback address REQUIRES WACKYPUB_SERVE_TOKEN; every RPC then carries
a Bearer token. For remote access prefer an SSH tunnel (ssh -L) so the bearer
token stays a fallback, not the perimeter.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return fmt.Errorf("resolve workspace: %w", err)
		}

		token := os.Getenv(serveTokenEnv)
		if !isLoopback(listenAddr) && token == "" {
			return fmt.Errorf("refusing to bind non-loopback %s without %s: remote serving requires a bearer token", listenAddr, serveTokenEnv)
		}

		sdk := adkAgent.NewSDK(wsDir)
		sdk.MaxToolTurns = GetMaxToolTurns()
		sdk.CommandTimeoutSeconds = GetCommandTimeoutSeconds()

		router := adkAgent.NewRoutingServer(sdk)
		grpcServer := grpc.NewServer(
			router.UnknownServiceHandler(),
			grpc.ChainUnaryInterceptor(tokenInterceptor(token)),
			grpc.ChainStreamInterceptor(tokenStreamInterceptor(token)),
		)

		lis, err := net.Listen("tcp", listenAddr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", listenAddr, err)
		}

		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		fmt.Fprintf(os.Stderr, "wackypub tcp-serve: listening on %s (workspace %s)\n", lis.Addr(), wsDir)

		serveErr := make(chan error, 1)
		go func() {
			serveErr <- grpcServer.Serve(lis)
		}()

		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "wackypub tcp-serve: SIGTERM received, draining in-flight turns")
			// Close the listener FIRST so Serve unblocks (GracefulStop alone does
			// not unblock Accept on a single-conn listener), then let in-flight
			// turns finish or cancel per the existing cancellation semantics.
			_ = lis.Close()
			grpcServer.GracefulStop()
			fmt.Fprintln(os.Stderr, "wackypub tcp-serve: drained, exiting")
			return nil
		case err := <-serveErr:
			return err
		}
	},
}

var listenAddr string

func init() {
	tcpServeCmd.Flags().StringVar(&listenAddr, "listen", "127.0.0.1:8423", "TCP listen address (loopback by default; non-loopback requires "+serveTokenEnv+")")
}
