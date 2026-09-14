package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
)

var (
	traceMaxSteps  int
	traceVerbosity int
)

var traceCmd = &cobra.Command{
	Use:   "trace [<agent_id> <commit> | <trace_id>]",
	Short: "Causal graph trace across multi-agent commit histories",
	Long: `Traces backward step-by-step through multi-agent commit history according to D36.

Usage Modes:
  wackypub trace <agent_id> <commit>  Trace backward starting from <commit> in <agent_id>'s repository.
  wackypub trace <trace_id>           Search across all agent repositories for <trace_id> and trace backward.

Flags:
  -n, --max-steps <int>   Maximum trace steps to traverse (default 20).
  -v, --verbosity <int>   Verbosity level 0..4 (default 1):
                            0: Minimal (event types, function call names, user prompt text)
                            1: Compact Default (event type, tool names, user text, assistant text)
                            2: Clean Full (complete text, stripped of thinking blocks & signatures)
                            3: Full with Thinking (includes thinking blocks, stripped of signatures)
                            4: Raw JSONL (dumps raw commit messages & A2A payloads as-is)`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := refuseIfAgentContext("wackypub trace"); err != nil {
			return err
		}
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}

		sdk := adkAgent.NewSDK(wsDir)
		opts := adkAgent.TraceOptions{
			MaxSteps:  traceMaxSteps,
			Verbosity: traceVerbosity,
		}

		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}

		var resp *agentv1.TraceResponse

		if len(args) == 1 {
			// Single argument: trace_id
			traceID := args[0]
			resp, err = sdk.Trace(ctx, &agentv1.TraceRequest{
				Target:    &agentv1.TraceRequest_TraceId{TraceId: traceID},
				MaxSteps:  int32(traceMaxSteps),
				Verbosity: int32(traceVerbosity),
			})
			if err != nil {
				return err
			}
		} else {
			// Two arguments: agent_id, commit
			agentID := args[0]
			commitSpec := args[1]
			resp, err = sdk.Trace(ctx, &agentv1.TraceRequest{
				AgentId:   agentID,
				Target:    &agentv1.TraceRequest_CommitSpec{CommitSpec: commitSpec},
				MaxSteps:  int32(traceMaxSteps),
				Verbosity: int32(traceVerbosity),
			})
			if err != nil {
				return err
			}
		}

		res := adkAgent.TraceProtoToResult(resp)
		output := adkAgent.FormatTraceResult(wsDir, res, opts)
		fmt.Print(output)
		return nil
	},
}

func init() {
	traceCmd.Flags().IntVarP(&traceMaxSteps, "max-steps", "n", 20, "Maximum trace steps to traverse")
	traceCmd.Flags().IntVarP(&traceVerbosity, "verbosity", "v", 1, "Verbosity level 0..4 (0: minimal, 1: compact, 2: clean full, 3: full+thinking, 4: raw)")
	RootCmd.AddCommand(traceCmd)
}
