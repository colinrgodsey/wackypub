package cmd

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

var agentHooksCmd = &cobra.Command{
	Use:   "hooks <agent_id>",
	Short: "List an agent's installed hooks and show their source (read-only)",
	Long: `Lists every hook installed under <agent_id>/hooks/<event>/, in the same
ascending numeric order RunHookChain executes them in, together with each
script's source. Backed by adkAgent.InspectAgentHooks.

Hooks are invisible from the outside otherwise - the only way to answer "what
will run against my next message, or another agent's" was raw filesystem
access. Works against any agent in the workspace, not just the caller's own,
for coordinators verifying hook installs across a swarm.

Only lists files DiscoverHooks would actually execute (regular, executable
files directly under an event directory) - a non-executable or stray file in
that directory is intentionally omitted, since listing it here would
misrepresent what fires.

Source content is capped at a fixed number of lines per script; a script
past that cap is shown with a "+N more lines" suffix rather than in full,
since hooks are expected to be small.

This command never creates, modifies, or executes a hook.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		agentID := args[0]
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}

		observations, err := adkAgent.InspectAgentHooks(wsDir, agentID)
		if err != nil {
			return err
		}
		if len(observations) == 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "No hooks installed for agent %q.\n", agentID)
			return nil
		}

		out := cmd.OutOrStdout()

		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(tw, "EVENT\tNAME\tPATH\tLINES"); err != nil {
			return fmt.Errorf("failed to write hook table header: %w", err)
		}
		for _, obs := range observations {
			if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", obs.Event, obs.Name, obs.Path, obs.TotalLines); err != nil {
				return fmt.Errorf("failed to write hook row for %s/%s: %w", obs.Event, obs.Name, err)
			}
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("failed to flush hook table: %w", err)
		}

		for _, obs := range observations {
			if _, err := fmt.Fprintf(out, "\n--- %s / %s ---\n", obs.Event, obs.Name); err != nil {
				return fmt.Errorf("failed to write hook content header for %s/%s: %w", obs.Event, obs.Name, err)
			}
			if _, err := fmt.Fprintln(out, obs.Content); err != nil {
				return fmt.Errorf("failed to write hook content for %s/%s: %w", obs.Event, obs.Name, err)
			}
			if obs.Truncated {
				if _, err := fmt.Fprintf(out, "... (+%d more lines)\n", obs.TotalLines-adkAgent.HookContentLineLimit); err != nil {
					return fmt.Errorf("failed to write truncation notice for %s/%s: %w", obs.Event, obs.Name, err)
				}
			}
		}

		return nil
	},
}

func init() {
	agentCmd.AddCommand(agentHooksCmd)
}
