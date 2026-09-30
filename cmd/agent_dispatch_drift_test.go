package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

// TestAgentFirstDispatch_CoverageAgainstCobraTable satisfies the acceptance criteria for
// tasks/wackypub/agent-first-dispatch-drift:
// "A test asserting every cobra-registered subcommand is either handled by the agent-first
// chain or explicitly excluded (the two sources of truth can no longer diverge silently)."
// "Unknown/handled classification documented per subcommand."
func TestAgentFirstDispatch_CoverageAgainstCobraTable(t *testing.T) {
	commands := agentCmd.Commands()
	if len(commands) == 0 {
		t.Fatal("expected agentCmd to have registered subcommands")
	}

	seenInCobra := make(map[string]bool)

	for _, c := range commands {
		name := c.Name()
		seenInCobra[name] = true

		classification, ok := agentFirstClassifications[name]
		if !ok {
			t.Errorf("registered subcommand %q is missing from agentFirstClassifications table; "+
				"every cobra-registered subcommand must be classified as handled or explicitly excluded", name)
			continue
		}

		if classification.Status != AgentFirstHandled && classification.Status != AgentFirstExcluded {
			t.Errorf("subcommand %q has invalid classification status %q; must be %q or %q",
				name, classification.Status, AgentFirstHandled, AgentFirstExcluded)
		}

		if strings.TrimSpace(classification.Rationale) == "" {
			t.Errorf("subcommand %q must have a non-empty rationale documenting its classification", name)
		}
	}

	// Verify no phantom classifications exist that are not registered on agentCmd.
	for name := range agentFirstClassifications {
		if !seenInCobra[name] {
			t.Errorf("agentFirstClassifications contains %q, but it is not registered as a subcommand on agentCmd", name)
		}
	}
}

// TestAgentFirstDispatch_HandledCommandsReachDispatch ensures that for every command classified as
// AgentFirstHandled, executeAgentDispatcher routes to the command's RunE and does NOT return
// the unhandled-drift error or excluded error.
func TestAgentFirstDispatch_HandledCommandsReachDispatch(t *testing.T) {
	for name, classification := range agentFirstClassifications {
		if classification.Status != AgentFirstHandled {
			continue
		}

		t.Run(name, func(t *testing.T) {
			args := []string{"testagent", name}
			if name == "scratchpad" {
				args = append(args, "list")
			}

			_, err := captureStdout(t, func() error {
				return executeAgentDispatcher(agentCmd, args)
			})
			// The command will likely fail due to missing workspace or missing required arguments,
			// but it must NOT fail with the drift unhandled error or excluded error.
			if err != nil {
				if strings.Contains(err.Error(), "not handled by agent-first dispatch") {
					t.Fatalf("subcommand %q is classified as handled but executeAgentDispatcher reported not handled: %v", name, err)
				}
				if strings.Contains(err.Error(), "not supported in agent-first syntax") {
					t.Fatalf("subcommand %q is classified as handled but executeAgentDispatcher reported not supported: %v", name, err)
				}
			}
		})
	}
}

// TestAgentFirstDispatch_UnhandledRegisteredSubcommandFailsLoud tests Option 2 fail-loud:
// if a subcommand is registered in Cobra on agentCmd but absent from executeAgentDispatcher,
// invoking it via agent-first syntax ("wackypub agent <id> <subcommand>") must return a
// NON-ZERO exit error with an explicit message, rather than silently printing help and exiting 0.
func TestAgentFirstDispatch_UnhandledRegisteredSubcommandFailsLoud(t *testing.T) {
	dummyName := "test-unhandled-drift-cmd"
	dummyCmd := &cobra.Command{
		Use:   dummyName,
		Short: "Synthetic subcommand to test fail-loud drift detection",
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
	agentCmd.AddCommand(dummyCmd)
	defer agentCmd.RemoveCommand(dummyCmd)

	// Direct call to executeAgentDispatcher
	err := executeAgentDispatcher(agentCmd, []string{"testagent", dummyName})
	if err == nil {
		t.Fatal("expected executeAgentDispatcher to return non-zero error for unhandled registered subcommand, got nil (silent exit 0)")
	}

	expectedSubstr := fmt.Sprintf("subcommand %q is registered on 'agent' but not handled by agent-first dispatch", dummyName)
	if !strings.Contains(err.Error(), expectedSubstr) {
		t.Fatalf("expected error containing %q, got: %v", expectedSubstr, err)
	}

	// Full CLI invocation via RootCmd.Execute()
	RootCmd.SetArgs([]string{"agent", "testagent", dummyName})
	buf := new(bytes.Buffer)
	RootCmd.SetOut(buf)
	RootCmd.SetErr(buf)
	defer func() {
		RootCmd.SetOut(nil)
		RootCmd.SetErr(nil)
	}()
	execErr := RootCmd.Execute()
	if execErr == nil {
		t.Fatal("expected RootCmd.Execute to return non-zero error for unhandled registered subcommand, got nil")
	}
	if !strings.Contains(execErr.Error(), expectedSubstr) {
		t.Fatalf("expected RootCmd.Execute error containing %q, got: %v", expectedSubstr, execErr)
	}
}

// TestAgentFirstDispatch_ExcludedSubcommandFailsLoud verifies that an explicitly excluded subcommand
// returns an explicit non-zero error with the documented rationale.
func TestAgentFirstDispatch_ExcludedSubcommandFailsLoud(t *testing.T) {
	dummyName := "test-excluded-cmd"
	dummyCmd := &cobra.Command{
		Use:   dummyName,
		Short: "Synthetic excluded subcommand",
	}
	agentCmd.AddCommand(dummyCmd)
	defer agentCmd.RemoveCommand(dummyCmd)

	agentFirstClassifications[dummyName] = AgentFirstClassification{
		Status:    AgentFirstExcluded,
		Rationale: "workspace-level inspection command, not per-agent",
	}
	defer delete(agentFirstClassifications, dummyName)

	err := executeAgentDispatcher(agentCmd, []string{"testagent", dummyName})
	if err == nil {
		t.Fatal("expected executeAgentDispatcher to return error for excluded subcommand, got nil")
	}

	expectedSubstr := fmt.Sprintf("subcommand %q is not supported in agent-first syntax: workspace-level inspection command, not per-agent", dummyName)
	if !strings.Contains(err.Error(), expectedSubstr) {
		t.Fatalf("expected error containing %q, got: %v", expectedSubstr, err)
	}
}

// TestAgentFirstDispatch_UnregisteredSubcommandFallsThroughToHelp verifies that unregistered
// subcommands / unknown arguments preserve the existing behavior (cmd.Help() returning nil),
// matching Colin's instruction to ship the safe minimum without changing unknown-arg exit contracts.
func TestAgentFirstDispatch_UnregisteredSubcommandFallsThroughToHelp(t *testing.T) {
	_, err := captureStdout(t, func() error {
		return executeAgentDispatcher(agentCmd, []string{"testagent", "completely-nonexistent-subcommand-12345"})
	})
	if err != nil {
		t.Fatalf("expected unregistered subcommand to fall through to help (nil error), got: %v", err)
	}

	// 1 argument (only agent ID) also falls through to help
	_, err = captureStdout(t, func() error {
		return executeAgentDispatcher(agentCmd, []string{"testagent"})
	})
	if err != nil {
		t.Fatalf("expected single argument to fall through to help (nil error), got: %v", err)
	}

	// 0 arguments falls through to help
	_, err = captureStdout(t, func() error {
		return executeAgentDispatcher(agentCmd, []string{})
	})
	if err != nil {
		t.Fatalf("expected empty args to fall through to help (nil error), got: %v", err)
	}
}

// TestAgentFirstDispatch_ScratchpadUnhandledSubcommandFailsLoud verifies that unhandled subcommands
// under "agent <id> scratchpad <action>" fail loud if registered on scratchpadCmd.
func TestAgentFirstDispatch_ScratchpadUnhandledSubcommandFailsLoud(t *testing.T) {
	actionName := "test-unhandled-scratch-action"
	dummyScratchCmd := &cobra.Command{
		Use:   actionName,
		Short: "Synthetic unhandled scratchpad subcommand",
	}
	scratchpadCmd.AddCommand(dummyScratchCmd)
	defer scratchpadCmd.RemoveCommand(dummyScratchCmd)

	err := executeAgentDispatcher(agentCmd, []string{"testagent", "scratchpad", actionName})
	if err == nil {
		t.Fatal("expected executeAgentDispatcher to return error for unhandled scratchpad subcommand, got nil")
	}

	expectedSubstr := fmt.Sprintf("scratchpad subcommand %q is registered but not supported in agent-first syntax", actionName)
	if !strings.Contains(err.Error(), expectedSubstr) {
		t.Fatalf("expected error containing %q, got: %v", expectedSubstr, err)
	}
}

// TestAgentFirstDispatch_Hooks verifies that "wackypub agent <id> hooks" works identically
// to "wackypub agent hooks <id>", rather than silently exiting 0 via help.
func TestAgentFirstDispatch_Hooks(t *testing.T) {
	wsDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("write root marker: %v", err)
	}

	origCwd, _ := os.Getwd()
	if err := os.Chdir(wsDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	// 1. Agent-first syntax: wackypub agent <id> hooks
	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "myagent", "hooks"})
	outAgentFirst, err := captureStdout(t, func() error {
		return RootCmd.Execute()
	})
	if err != nil {
		t.Fatalf("agent-first hooks execution failed: %v", err)
	}

	expectedMsg := "No hooks installed for agent \"myagent\"."
	if !strings.Contains(outAgentFirst, expectedMsg) {
		t.Errorf("expected agent-first hooks output to contain %q, got:\n%s", expectedMsg, outAgentFirst)
	}

	// 2. Subcommand-first syntax: wackypub agent hooks <id>
	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "hooks", "myagent"})
	outSubcmdFirst, err := captureStdout(t, func() error {
		return RootCmd.Execute()
	})
	if err != nil {
		t.Fatalf("subcommand-first hooks execution failed: %v", err)
	}

	if !strings.Contains(outSubcmdFirst, expectedMsg) {
		t.Errorf("expected subcommand-first hooks output to contain %q, got:\n%s", expectedMsg, outSubcmdFirst)
	}

	if strings.TrimSpace(outAgentFirst) != strings.TrimSpace(outSubcmdFirst) {
		t.Errorf("expected identical output between agent-first and subcommand-first hooks, got:\nagent-first: %q\nsubcmd-first: %q",
			outAgentFirst, outSubcmdFirst)
	}
}
