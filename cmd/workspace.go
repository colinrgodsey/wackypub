package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

// wackypub workspace [agent_id]
var workspaceCmd = &cobra.Command{
	Use:   "workspace [agent_id]",
	Short: "Show workspace information, or diagnose a single agent's setup",
	Long: `With no argument, lists the agent directories found under the workspace directory
(--ws, defaults to the current directory) along with a one-line status for each: whether
runtime.json is present and valid, how many turns are in session.jsonl, and whether MEMORY.md
exists. A directory is recognized as an agent directory if it directly contains at least one of
AGENTS.md, runtime.json, or session.jsonl.

With an agent_id argument, reports the detailed on-disk state of that one agent: every expected
file's presence, runtime.json's resolved path (following a symlink) and whether it parses, and
session/memory stats - including anything that looks broken or incomplete. Works even if the
agent directory doesn't exist yet or is only partially set up; in that case it explains what's
missing rather than erroring, so this doubles as a guide for setting up a new agent correctly.

This command is read-only: it never creates or modifies any file.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := adkAgent.NewSDK(wsDir)

		if len(args) == 1 {
			return printAgentInspection(sdk, args[0])
		}
		return printWorkspaceOverview(sdk, wsDir)
	},
}

var initGitCmd = &cobra.Command{
	Use:   "init-git [agent_id]",
	Short: "Initialize git versioning for the workspace or a specific agent directory according to D35",
	Long:  "Initializes an isolated git repository in an agent directory (<ws_dir>/<agent_id>/.git) or the root workspace (<ws_dir>/.git).",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		if len(args) == 1 {
			agentID := args[0]
			if err := adkAgent.InitAgentGit(wsDir, agentID); err != nil {
				return err
			}
			fmt.Printf("Initialized per-agent git repository in %s/%s\n", wsDir, agentID)
			return nil
		}
		if err := adkAgent.InitWorkspaceGit(wsDir); err != nil {
			return err
		}
		fmt.Printf("Initialized workspace git repository in %s\n", wsDir)
		return nil
	},
}

func printWorkspaceOverview(sdk *adkAgent.AgentSDK, wsDir string) error {
	if selfID, ok := adkAgent.CurrentAgentIDFromCWD(); ok {
		fmt.Printf("You are agent %q.\n", selfID)
		if insp, err := sdk.InspectAgent(selfID); err == nil {
			if len(insp.AllowedAgents) > 0 {
				fmt.Printf("Agents you can talk to: %s\n", strings.Join(insp.AllowedAgents, ", "))
			} else {
				fmt.Println("Agents you can talk to: none (no WACKYPUB_ALLOWED_AGENTS file, or it's empty)")
			}
		}
		fmt.Println()
	}

	absDir, err := filepath.Abs(wsDir)
	if err != nil {
		absDir = wsDir
	}
	gitStatus := "disabled"
	if adkAgent.IsWorkspaceGitRepo(wsDir) {
		gitStatus = "enabled"
	}
	fmt.Printf("Workspace: %s (git: %s)\n", absDir, gitStatus)

	ids, err := sdk.ListAgents()
	if err != nil {
		return err
	}

	if len(ids) == 0 {
		fmt.Println("\nNo agent directories found.")
		fmt.Println("An agent directory needs at least one of AGENTS.md, runtime.json, or session.jsonl directly inside it to be recognized.")
		fmt.Println("Create one with, e.g.: wackypub agent <agent_id> add \"hello\"")
		return nil
	}

	fmt.Printf("\nAgents found: %d\n\n", len(ids))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "AGENT_ID\tRUNTIME.JSON\tSESSION TURNS\tMEMORY.MD\tTOOLS\tSKILLS\tALLOWED_AGENTS")
	for _, id := range ids {
		insp, err := sdk.InspectAgent(id)
		if err != nil {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", id, "error", "-", "-", "-", "-", "-")
			continue
		}

		runtimeStatus := "missing"
		if insp.RuntimeJSONExists {
			if insp.RuntimeJSONValid {
				runtimeStatus = "ok"
			} else {
				runtimeStatus = "invalid"
			}
		}

		turns := "-"
		if insp.SessionJSONLExists {
			turns = fmt.Sprintf("%d", insp.SessionTurnCount)
			if insp.SessionCorruptLines > 0 {
				turns += fmt.Sprintf(" (%d corrupt)", insp.SessionCorruptLines)
			}
		}

		memory := "no"
		if insp.MemoryMDExists {
			memory = "yes"
		}

		toolsStatus := "-"
		if insp.ToolsDirExists {
			toolsStatus = fmt.Sprintf("%d tool(s)", len(insp.DiscoveredTools))
			if len(insp.ShadowedTools) > 0 {
				toolsStatus += fmt.Sprintf(" (%d shadowed)", len(insp.ShadowedTools))
			}
		}

		skillsStatus := "-"
		if insp.SkillsDirExists {
			skillsStatus = fmt.Sprintf("%d skill(s)", len(insp.DiscoveredSkills))
			if len(insp.ShadowedSkills) > 0 {
				skillsStatus += fmt.Sprintf(" (%d shadowed)", len(insp.ShadowedSkills))
			}
		}

		allowedStatus := "deny-all"
		if insp.AllowedAgentsExists {
			allowedStatus = fmt.Sprintf("%d allowed", len(insp.AllowedAgents))
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", id, runtimeStatus, turns, memory, toolsStatus, skillsStatus, allowedStatus)
	}
	return w.Flush()
}

func printAgentInspection(sdk *adkAgent.AgentSDK, agentID string) error {
	insp, err := sdk.InspectAgent(agentID)
	if err != nil {
		return err
	}

	if !insp.AgentDirExists {
		fmt.Printf("Agent %q does not exist yet at %s.\n\n", agentID, insp.AgentDir)
		fmt.Println("To create it, add at minimum:")
		fmt.Printf("  %s/runtime.json   LLM endpoint/model config - see docs/agents.md §3\n", insp.AgentDir)
		fmt.Println("AGENTS.md is optional (falls back to a generic \"You are agent <id>.\" prompt if missing).")
		fmt.Printf("\nThen run: wackypub agent %s prompt \"...\"\n", agentID)
		return nil
	}

	fmt.Printf("Agent: %s\n", insp.AgentID)
	fmt.Printf("Directory: %s\n\n", insp.AgentDir)

	fmt.Println("Files:")
	fmt.Printf("  AGENTS.md                 %s\n", presence(insp.AgentsMDExists))
	fmt.Printf("  MEMORY.md                 %s\n", presence(insp.MemoryMDExists))
	if insp.DotEnvExists {
		fmt.Println("  .env                      present")
	}

	if insp.AllowedAgentsExists {
		fmt.Printf("  WACKYPUB_ALLOWED_AGENTS   present (%d allowed)\n", len(insp.AllowedAgents))
	} else {
		fmt.Println("  WACKYPUB_ALLOWED_AGENTS   missing (deny-all cross-agent access)")
	}

	if insp.ToolsDirExists {
		fmt.Printf("  tools/                    present (%d tool(s) discovered)\n", len(insp.DiscoveredTools))
	} else {
		fmt.Println("  tools/                    missing")
	}

	if insp.SkillsDirExists {
		fmt.Printf("  skills/                   present (%d skill(s) discovered)\n", len(insp.DiscoveredSkills))
	} else {
		fmt.Println("  skills/                   missing")
	}

	if !insp.RuntimeJSONExists {
		fmt.Println("  runtime.json              missing")
	} else {
		runtimeLine := "present"
		if insp.RuntimeJSONIsSymlink {
			if insp.RuntimeJSONResolved != "" {
				runtimeLine += fmt.Sprintf(" (symlink -> %s)", insp.RuntimeJSONResolved)
			} else {
				runtimeLine += " (symlink, broken - target does not resolve)"
			}
		}
		if insp.RuntimeJSONValid {
			runtimeLine += ", valid"
		} else {
			runtimeLine += ", INVALID"
		}
		fmt.Printf("  runtime.json              %s\n", runtimeLine)
	}

	if !insp.SessionJSONLExists {
		fmt.Println("  session.jsonl             missing (no turns yet)")
	} else {
		sessionLine := fmt.Sprintf("present, %d turn(s)", insp.SessionTurnCount)
		if insp.SessionCorruptLines > 0 {
			sessionLine += fmt.Sprintf(", %d line(s) failed to parse and were skipped", insp.SessionCorruptLines)
		}
		fmt.Printf("  session.jsonl             %s\n", sessionLine)
	}

	var issues []string
	if !insp.RuntimeJSONExists {
		issues = append(issues, "runtime.json is missing - generation will fall back to the bundled openrouter-auto default (requires OPENROUTER_API_KEY), or add your own (see docs/agents.md §3 for the schema).")
	} else if !insp.RuntimeJSONValid {
		issues = append(issues, fmt.Sprintf("runtime.json failed to parse: %s", insp.RuntimeJSONError))
	}
	if insp.RuntimeJSONIsSymlink && insp.RuntimeJSONResolved == "" {
		issues = append(issues, "runtime.json is a symlink that does not resolve to an existing file.")
	}
	if insp.SessionCorruptLines > 0 {
		issues = append(issues, fmt.Sprintf("session.jsonl has %d line(s) that don't parse as JSON turns - they're silently skipped on every read, which can cause the agent to lose context. See .agents/AGENTS.md's session.jsonl corruption gotcha.", insp.SessionCorruptLines))
	}
	for _, shadowMsg := range insp.ShadowedTools {
		issues = append(issues, shadowMsg)
	}
	for _, shadowMsg := range insp.ShadowedSkills {
		issues = append(issues, shadowMsg)
	}

	if len(issues) > 0 {
		fmt.Println("\nIssues:")
		for _, issue := range issues {
			fmt.Printf("  - %s\n", issue)
		}
	}

	return nil
}

// refuseIfAgentContext blocks an operator/diagnostic command from running when the
// current directory is an agent's own directory, according to D41. No override -
// run from the workspace root (or anywhere that isn't literally that agent's own
// directory) instead.
func refuseIfAgentContext(cmdName string) error {
	if id, ok := adkAgent.CurrentAgentIDFromCWD(); ok {
		return fmt.Errorf("%q is an operator/diagnostic command, not available to agents - refusing because the current directory is agent %q's own directory", cmdName, id)
	}
	return nil
}

var snapshotCmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Create or update MANIFEST.md snapshot of agent commit SHAs",
	Long: `Scans all agent directories in the workspace and creates or updates <ws_dir>/MANIFEST.md according to D35.

Behavior:
  - Lists every agent directory in the workspace.
  - Records each agent's active HEAD commit SHA in MANIFEST.md with a timestamped Markdown table.
  - If the workspace root repository (<ws_dir>/.git) exists, commits MANIFEST.md with message 'snapshot'.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := refuseIfAgentContext("wackypub workspace snapshot"); err != nil {
			return err
		}
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		manifestPath, err := adkAgent.CreateWorkspaceSnapshot(wsDir)
		if err != nil {
			return err
		}
		fmt.Printf("Updated workspace manifest snapshot: %s\n", manifestPath)
		return nil
	},
}

var tagCmd = &cobra.Command{
	Use:   "tag <name>",
	Short: "Tag the workspace root repo and each per-agent repository",
	Long: `Creates a git tag in the workspace root repository and each per-agent repository according to D35.

Arguments:
  <name>  The tag name to apply (e.g. 'v1.0.0').

Behavior:
  - Tags the workspace root repository (<ws_dir>/.git) with <name>.
  - Tags each per-agent repository (<ws_dir>/<agent_id>/.git) with 'tag-<agent_id>'.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := refuseIfAgentContext("wackypub workspace tag"); err != nil {
			return err
		}
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		tagName := args[0]
		if err := adkAgent.TagWorkspaceAndAgents(wsDir, tagName); err != nil {
			return err
		}
		fmt.Printf("Tagged workspace with %q and agent repositories with \"tag-<agent_id>\"\n", tagName)
		return nil
	},
}

var confirmPush bool

var pushCmd = &cobra.Command{
	Use:   "push <remote>",
	Short: "Push workspace root and per-agent repositories to a remote",
	Long: `Pushes the workspace root repository and each per-agent repository to <remote> according to D35.

Arguments:
  <remote>  The name of a remote (e.g. 'origin') configured on the root workspace repository, or a direct remote Git URL.

Behavior:
  - Reads the remote URL from <remote> in the root workspace repository (<ws_dir>/.git).
  - Pushes the root workspace repository (branches and tags) to <remote>.
  - For each valid agent directory (<ws_dir>/<agent_id>) containing a .git repository and runtime.json:
    - Automatically binds the root remote URL if the agent repository does not have <remote> configured.
    - Pushes the agent's active HEAD branch to a remote branch named <agent_id> (refs/heads/<agent_id>).
    - Pushes all per-agent git tags (e.g. tag-<agent_id>) to the remote.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := refuseIfAgentContext("wackypub workspace push"); err != nil {
			return err
		}
		if !confirmPush {
			return fmt.Errorf("WARNING: this will push full agent history including runtime.json/.env which may contain API keys — only push to a private, trusted remote. re-run this command with the --i-understand flag to proceed")
		}
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		remoteName := args[0]
		if err := adkAgent.PushWorkspaceAndAgents(wsDir, remoteName); err != nil {
			return err
		}
		fmt.Printf("Pushed workspace and agent repositories to remote %q\n", remoteName)
		return nil
	},
}

func presence(exists bool) string {
	if exists {
		return "present"
	}
	return "missing"
}

func init() {
	workspaceCmd.AddCommand(initGitCmd)
	workspaceCmd.AddCommand(snapshotCmd)
	workspaceCmd.AddCommand(tagCmd)
	pushCmd.Flags().BoolVar(&confirmPush, "i-understand", false, "")
	_ = pushCmd.Flags().MarkHidden("i-understand")
	workspaceCmd.AddCommand(pushCmd)
	RootCmd.AddCommand(workspaceCmd)
}
