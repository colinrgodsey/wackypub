package cmd

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

// Write-age thresholds for `workspace locks`. The SDK reports lock and activity facts;
// deciding what counts as stuck is a presentation concern, so it lives here. The task
// card leaves tuning these behind flags as a follow-up.
const (
	locksActiveWrite = 5 * time.Minute
	locksWedgeWrite  = 10 * time.Minute
	locksIdleWrite   = 30 * time.Minute
	locksCmdWidth    = 22
)

// classifyLock maps one SDK observation onto the STATUS and VERDICT columns.
//
// A held lock whose session write falls between locksActiveWrite and
// locksWedgeWrite stays OK: the card escalates only past 10 minutes so that a single
// long-running tool command does not read as a wedge. A dead holder PID is STALE
// unless the session was written within locksActiveWrite, because releasing a lock
// leaves the file behind, so a lock file naming an exited PID is the normal state
// after any finished run.
func classifyLock(obs adkAgent.AgentLockObservation, now time.Time) (string, string) {
	if !obs.LockExists {
		return "FREE", unwedgedVerdict(obs, now)
	}
	if !obs.HolderPIDValid {
		return "UNKNOWN", unwedgedVerdict(obs, now)
	}
	if !obs.HolderAlive {
		if obs.SessionExists && now.Sub(obs.LastWrite) <= locksActiveWrite {
			return "STALE", "OK"
		}
		return "STALE", "STALE"
	}
	if !obs.SessionExists {
		return "HELD", "OK"
	}
	if now.Sub(obs.LastWrite) > locksWedgeWrite {
		return "HELD", "WEDGED"
	}
	return "HELD", "OK"
}

func unwedgedVerdict(obs adkAgent.AgentLockObservation, now time.Time) string {
	if !obs.SessionExists {
		return "IDLE"
	}
	if now.Sub(obs.LastWrite) > locksIdleWrite {
		return "IDLE"
	}
	return "OK"
}

func locksAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

var workspaceLocksCmd = &cobra.Command{
	Use:   "locks",
	Short: "Show per-agent session lock status and session activity (read-only)",
	Long: `Lists every agent directory in the workspace with the state of its session lock and
how recently its session.jsonl was written, so a stuck turn can be told apart from a
busy one. Backed by adkAgent.InspectAgentLocks, which reports the facts; the verdict
thresholds below are this command's interpretation of them.

STATUS is read from <agent_dir>/session.lock:
  HELD     lock file present and the PID recorded inside it is alive
  FREE     no lock file
  STALE    lock file present but the recorded PID is gone
  UNKNOWN  lock file present but the PID inside it could not be read

VERDICT combines that with session.jsonl write age:
  OK       written within the last 5 minutes
  WEDGED   lock held and nothing written for over 10 minutes - alive but producing nothing
  IDLE     no lock and nothing written for over 30 minutes
  STALE    recorded PID is gone and the session has been quiet for over 5 minutes

A live PID holding a lock is not the same as progress: the incident this command was
written for had several agents whose wrappers held their session locks while the model
call behind them never returned. HELD plus a quiet session.jsonl is that shape. Releasing
a lock leaves the lock file in place, so a stale lock file naming an exited PID is what
finished runs look like, not evidence of a bug.

Holder liveness uses kill(pid,0) and holder commands are read from /proc/<pid>/cmdline,
so both are Linux-specific; elsewhere every holder reports as unreadable while the
STATUS and VERDICT columns stay correct. Values of flags naming a token, key, secret, or
password are replaced with [redacted], because those often arrive as argv.

This command never creates, truncates, locks, or deletes a lock file.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		observations, err := adkAgent.InspectAgentLocks(wsDir)
		if err != nil {
			return err
		}
		if len(observations) == 0 {
			fmt.Println("No agent directories found.")
			return nil
		}

		now := time.Now()
		out := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(out, "AGENT\tSTATUS\tHOLDER_PID\tHOLDER_CMD\tHELD_SINCE\tLAST_WRITE\tVERDICT"); err != nil {
			return fmt.Errorf("failed to write lock table header: %w", err)
		}
		for _, obs := range observations {
			status, verdict := classifyLock(obs, now)

			holderPID, holderCmd, heldSince, lastWrite := "-", "-", "-", "-"
			if obs.HolderPIDValid {
				holderPID = fmt.Sprintf("%d", obs.HolderPID)
				holderCmd = obs.HolderCommand
				if holderCmd == "" {
					// A dead PID has no /proc entry, which is the expected reading for a
					// leftover lock file; a live PID with no readable cmdline is a genuine
					// observability gap and must not be reported as gone.
					if obs.HolderAlive {
						holderCmd = "(unreadable)"
					} else {
						holderCmd = "(process gone)"
					}
				} else if len(holderCmd) > locksCmdWidth {
					holderCmd = holderCmd[:locksCmdWidth]
				}
			}
			if obs.LockExists {
				heldSince = obs.LockHeldSince.Format("2006-01-02 15:04:05")
			}
			if obs.SessionExists {
				lastWrite = fmt.Sprintf("%s (%s)", obs.LastWrite.Format("15:04:05"), locksAge(now.Sub(obs.LastWrite)))
			}
			if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", obs.AgentID, status, holderPID, holderCmd, heldSince, lastWrite, verdict); err != nil {
				return fmt.Errorf("failed to write lock row for agent %q: %w", obs.AgentID, err)
			}
		}
		if err := out.Flush(); err != nil {
			return fmt.Errorf("failed to flush lock table: %w", err)
		}
		return nil
	},
}

func init() {
	workspaceCmd.AddCommand(workspaceLocksCmd)
}
