package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

var agentDeleteConfirm bool
var agentOrphansApply bool

func childLifecycleError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("could not %s child: %w", action, err)
}

var agentPauseCmd = &cobra.Command{
	Use:   "pause <child_agent_id>",
	Short: "Make a spawned child unreachable and mark it paused (coordinator authority)",
	Long: `Remove a spawned child from its parent's allowlist and mark it paused.

Nothing is deleted: files, memory and session stay exactly where they are, because a pause
has to be reversible and must never be a way to lose work. resume puts the pair back. Like
spawn and retire, this is a coordinator command - an agent does not decide the lifetime of
another identity.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		meta, err := adkAgent.PauseSpawnedChild(wsDir, args[0])
		if err != nil {
			return childLifecycleError("pause", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "paused %s (parent %s no longer has it in %s; memory and session untouched)\n", args[0], meta.Parent, adkAgent.AllowedAgentsFile)
		return nil
	},
}

var agentResumeCmd = &cobra.Command{
	Use:   "resume <child_agent_id>",
	Short: "Reverse a pause by restoring the parent's allowlist entry",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		meta, err := adkAgent.ResumeSpawnedChild(wsDir, args[0])
		if err != nil {
			return childLifecycleError("resume", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "resumed %s (parent %s can address it again)\n", args[0], meta.Parent)
		return nil
	},
}

var agentRetireCmd = &cobra.Command{
	Use:   "retire <child_agent_id>",
	Short: "Archive a spawned child into the workspace archive and refresh the manifest",
	Long: `Move a spawned child's whole subtree into <workspace>/` + adkAgent.SpawnArchiveDirName + `/spawned/ and
regenerate MANIFEST.md.

The child stops being addressable the moment it leaves the workspace root, which is what
retirement means, and the history travels with it. A child's memory and session belong to
the workspace rather than to the parent that asked for it, so this is the normal end of a
child's life and delete is the rare exception that requires it to have happened first.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		archivePath, _, err := adkAgent.RetireSpawnedChild(wsDir, args[0])
		if err != nil {
			if archivePath != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "the child is archived at %s; finish the job manually if the follow-up below fails\n", archivePath)
			}
			return childLifecycleError("retire", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "retired %s into %s\n", args[0], archivePath)
		return nil
	},
}

var agentDeleteCmd = &cobra.Command{
	Use:   "delete <child_agent_id>",
	Short: "Remove an already-retired child's archived tree (privileged, archive-first)",
	Long: `Delete removes an archived copy of a retired child. It refuses while the child directory
still exists, and it refuses without --confirm-delete, so the only route to destruction
passes through an archive that exists on disk.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		removed, err := adkAgent.DeleteSpawnedChild(wsDir, args[0], agentDeleteConfirm)
		if err != nil {
			return childLifecycleError("delete", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "removed archived child at %s\n", removed)
		return nil
	},
}

var agentOrphansCmd = &cobra.Command{
	Use:   "orphans",
	Short: "List spawned children whose parent no longer exists (coordinator owns them)",
	Long: `A child outlives its parent on purpose: its memory is workspace history. This lists the
children whose parent directory is gone, and --mark records them as orphaned in their
` + adkAgent.SpawnMetaFile + ` so the manifest shows them. Nothing is deleted or reassigned.`,
	Args:                  cobra.NoArgs,
	DisableFlagsInUseLine: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		orphans, err := adkAgent.MarkOrphanedSpawnedChildren(wsDir, agentOrphansApply)
		if err != nil {
			return err
		}
		if len(orphans) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no orphaned children")
			return nil
		}
		verb := "would mark"
		if agentOrphansApply {
			verb = "marked"
		}
		for _, id := range orphans {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", verb, id)
		}
		return nil
	},
}

func init() {
	agentDeleteCmd.Flags().BoolVar(&agentDeleteConfirm, "confirm-delete", false, "confirm that removing an archived child is intended")
	agentOrphansCmd.Flags().BoolVar(&agentOrphansApply, "mark", false, "record the orphan marker in each child's "+adkAgent.SpawnMetaFile)

	agentCmd.AddCommand(agentPauseCmd)
	agentCmd.AddCommand(agentResumeCmd)
	agentCmd.AddCommand(agentRetireCmd)
	agentCmd.AddCommand(agentDeleteCmd)
	agentCmd.AddCommand(agentOrphansCmd)
}
