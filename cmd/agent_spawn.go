package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

var (
	agentSpawnParent  string
	agentSpawnRuntime string
	agentSpawnTier    string
	agentSpawnGrant   bool
)

var agentSpawnCmd = &cobra.Command{
	Use:   "spawn <child_agent_id>",
	Short: "Create a persistent helper agent for a parent (coordinator authority)",
	Long: `Create a thin child agent inside the workspace being operated on.

Spawn is a coordinator-grade command, not agent self-service: it creates an identity, so
the authority that multiplies capabilities stays with the operator rather than with
whatever agent is asking. pause and retire are coordinator-grade for the same reason.

--parent names who asked and is recorded in the child's ` + adkAgent.SpawnMetaFile + ` lineage.
The child's allowlist starts closed except for that parent; --grant appends the child to
the parent's allowlist as well, which rewrites another agent's roster and is therefore
opt-in rather than automatic.

The runtime comes from a named preset under <workspace>/` + adkAgent.RuntimesDirName + `/, never from
a free-text endpoint, because the preset is where the operator decides which endpoints a
child may spend against. Recursion is capped at depth ` + fmt.Sprint(adkAgent.MaxSpawnDepth) + `: a helper cannot spawn a
helper. A child that would carry its own ` + adkAgent.RootMarkerFile + ` marker is refused outright - a
nested workspace root outranks every roster in the tree.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}

		result, err := adkAgent.SpawnAgent(adkAgent.SpawnOptions{
			WorkspaceDir: wsDir,
			ChildID:      args[0],
			ParentID:     agentSpawnParent,
			Preset:       agentSpawnRuntime,
			Tier:         agentSpawnTier,
		})
		if err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "spawned %s for %s (depth %d, runtime preset %s)\n", args[0], agentSpawnParent, result.Depth, agentSpawnRuntime)
		fmt.Fprintf(cmd.OutOrStdout(), "  directory: %s\n", result.ChildDir)
		fmt.Fprintf(cmd.OutOrStdout(), "  written:   %s\n", strings.Join(result.FilesWritten, ", "))
		if !result.InheritedTools {
			fmt.Fprintf(cmd.OutOrStdout(), "  tools:     none inherited (no %s/%s/ in the workspace to link)\n", "toolsets", args[0])
		}

		if agentSpawnGrant {
			changed, err := adkAgent.GrantSpawnedChild(wsDir, agentSpawnParent, args[0])
			if err != nil {
				return fmt.Errorf("child was created at %s but granting the parent failed: %w", result.ChildDir, err)
			}
			if changed {
				fmt.Fprintf(cmd.OutOrStdout(), "  granted:   %s may now address %s\n", agentSpawnParent, args[0])
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "  granted:   already present in %s allowlist\n", agentSpawnParent)
			}
		}
		return nil
	},
}

var agentRuntimesCmd = &cobra.Command{
	Use:                   "runtimes",
	Short:                 "List the runtime presets a workspace allows children to be spawned with",
	Args:                  cobra.NoArgs,
	DisableFlagsInUseLine: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		presets, err := adkAgent.ListRuntimePresets(wsDir)
		if err != nil {
			return err
		}
		if len(presets) == 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "no runtime presets: create %s/<name>.json in %s to allow spawning\n", adkAgent.RuntimesDirName, wsDir)
			return nil
		}
		for _, name := range presets {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", name)
		}
		return nil
	},
}

func init() {
	agentSpawnCmd.Flags().StringVar(&agentSpawnRuntime, "runtime", "", "runtime preset name under <workspace>/"+adkAgent.RuntimesDirName+"/ (required)")
	agentSpawnCmd.Flags().StringVar(&agentSpawnParent, "parent", "", "agent id that asked for this child (required, recorded as lineage)")
	agentSpawnCmd.Flags().StringVar(&agentSpawnTier, "tier", "", "containment tier recorded in lineage; v1 records it and enforces nothing")
	agentSpawnCmd.Flags().BoolVar(&agentSpawnGrant, "grant", false, "also append the child to the parent's "+adkAgent.AllowedAgentsFile)
	_ = agentSpawnCmd.MarkFlagRequired("runtime")
	_ = agentSpawnCmd.MarkFlagRequired("parent")

	agentCmd.AddCommand(agentSpawnCmd)
	agentCmd.AddCommand(agentRuntimesCmd)
}
