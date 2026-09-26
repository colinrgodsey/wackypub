package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"github.com/colinrgodsey/wackypub/pkg/stdio"
)

// stdioServeCmd serves the AgentService protocol over stdin/stdout with the
// CLI lifecycle: start the process, serve the endpoint, finish when the client
// closes - then exit. It is NOT a daemon; every caller spawns and reaps its
// own process (wackydiscord does exactly this from the workspace root).
//
// Workspace identity is CWD-based and identical to the rest of the CLI:
// ResolveWorkspaceDir walks up from CWD looking for the RootMarkerFile, so
// spawning wackypub in the workspace root makes the served SDK serve exactly
// that workspace - no new identity or authorization surface.
//
// stdout is the PROTOCOL channel. Every diagnostic in this command must go to
// stderr; a stray line on stdout corrupts the gRPC framing. Writers of this
// file, keep it that way.
var stdioServeCmd = &cobra.Command{
	Use:   "stdio-serve",
	Short: "Serve the AgentService protocol over stdio (CLI lifecycle, per-call process)",
	Long: `Serves the AgentService protocol over stdin/stdout with the CLI lifecycle.

Start -> serve -> client closes -> process exits. The workspace is resolved
from the current directory the same way every other wackypub command resolves
it (walk up for WACKYPUB_ROOT), so run this from the workspace root.

stdout carries gRPC only; all diagnostics go to stderr.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return fmt.Errorf("resolve workspace: %w", err)
		}

		sdk := adkAgent.NewSDK(wsDir)
		sdk.MaxToolTurns = GetMaxToolTurns()
		sdk.CommandTimeoutSeconds = GetCommandTimeoutSeconds()

		grpcServer := grpc.NewServer()
		agentv1.RegisterAgentServiceServer(grpcServer, sdk)

		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		conn := stdio.NewConn(os.Stdin, os.Stdout)
		if err := stdio.ServeContext(ctx, grpcServer, conn); err != nil {
			// Diagnostics to stderr only: stdout must stay pure protocol.
			fmt.Fprintf(os.Stderr, "wackypub stdio-serve: %v\n", err)
			return err
		}
		fmt.Fprintln(os.Stderr, "wackypub stdio-serve: client closed, exiting")
		return nil
	},
}
