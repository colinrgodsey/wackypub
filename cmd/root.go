package cmd

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
	"github.com/colinrgodsey/wackypub/pkg/config"
)

var (
	cfgFile               string
	workspaceDir          string
	modelName             string
	apiKey                string
	maxToolTurns          int
	commandTimeoutSeconds int
	cfg                   *config.Config

	// BundledA2ASkill holds embedded skills/wackypub-a2a/SKILL.md text passed from main.go (D34).
	BundledA2ASkill string
	// BundledWSSkill holds embedded skills/wackypub-ws/SKILL.md text passed from main.go (D34).
	BundledWSSkill string
)

// bundledSkill pairs the short name accepted by "wackypub skill <name>" with the
// parsed skill (frontmatter Name/Description, not necessarily the same short form).
type bundledSkill struct {
	ShortName string
	Skill     *adkAgent.Skill
}

// bundledSkills returns the bundled skills in a fixed order, each parsed for its own
// description from frontmatter rather than a separately hardcoded copy (D40). ShortName
// is the form GetSkillContent actually accepts, kept in sync with the Use: line below.
func bundledSkills() ([]bundledSkill, error) {
	a2a, err := adkAgent.ParseSkillContent(BundledA2ASkill, "wackypub-a2a")
	if err != nil {
		return nil, fmt.Errorf("failed to parse bundled a2a skill: %w", err)
	}
	ws, err := adkAgent.ParseSkillContent(BundledWSSkill, "wackypub-ws")
	if err != nil {
		return nil, fmt.Errorf("failed to parse bundled ws skill: %w", err)
	}
	return []bundledSkill{
		{ShortName: "a2a", Skill: a2a},
		{ShortName: "ws", Skill: ws},
	}, nil
}

// GetSkillContent resolves and returns skill guidance by name (a2a, ws).
func GetSkillContent(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "a2a", "wackypub-a2a":
		return BundledA2ASkill, nil
	case "ws", "workspace", "wackypub-ws":
		return BundledWSSkill, nil
	default:
		return "", fmt.Errorf("unknown skill %q. Available skills: a2a, ws", name)
	}
}

var skillCmd = &cobra.Command{
	Use:   "skill [a2a|ws]",
	Short: "List bundled WackyPub skills, or print one - if you're an agent, you'll want to load one of these",
	Long: `With no argument, lists the bundled WackyPub skills (name and description) and exits.
With a name (a2a or ws), prints that skill's full guidance (skills/wackypub-a2a/SKILL.md or
skills/wackypub-ws/SKILL.md) directly to stdout.

If you're an agent driving this CLI cold, "wackypub skill" is a reasonable first move.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			skills, err := bundledSkills()
			if err != nil {
				return err
			}
			fmt.Println("Available skills:")
			for _, sk := range skills {
				fmt.Printf("  %-4s  %s\n", sk.ShortName, sk.Skill.Description)
			}
			fmt.Println(`Run "wackypub skill <name>" to print one.`)
			return nil
		}
		content, err := GetSkillContent(args[0])
		if err != nil {
			return err
		}
		fmt.Print(content)
		return nil
	},
}

// RootCmd represents the base command when called without any subcommands.
var RootCmd = &cobra.Command{
	Use:   "wackypub",
	Short: "WackyPub - Folder-based Agent Management CLI powered by Google ADK",
	Long: `WackyPub is a command-line tool and Go SDK for managing folder-based AI agents.

Built on top of Google Agent Development Kit (ADK) in Go, WackyPub supports workspace agent directories,
OpenAI-compatible model adapters, macro prompt inclusion (@<FILE_PATH>), and auto-compaction.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		var err error
		cfgPath := cfgFile
		if cfgPath == "" {
			cfgPath = "wackypub.yaml"
		}

		cfg, err = config.LoadConfig(cfgPath)
		if err != nil {
			// Ignore if wackypub.yaml doesn't exist for agent CLI operations
			cfg = config.DefaultConfig()
		}

		if modelName != "" {
			cfg.DefaultModel = modelName
		}
		if apiKey != "" {
			cfg.APIKey = apiKey
		}

		return nil
	},
}

// GetWorkspaceDir returns the resolved workspace directory according to D15.
func GetWorkspaceDir() (string, error) {
	isExplicit := RootCmd.PersistentFlags().Changed("ws")
	return adkAgent.ResolveWorkspaceDir(workspaceDir, isExplicit)
}

// GetMaxToolTurns returns the configured max tool turns limit.
func GetMaxToolTurns() int {
	return maxToolTurns
}

// GetCommandTimeoutSeconds returns the configured command execution timeout according to D52 precedence:
// 1. Explicit CLI flag (--command-timeout-seconds)
// 2. WACKYPUB_COMMAND_TIMEOUT_SECONDS environment variable
// 3. DefaultCommandTimeoutSeconds (900)
func GetCommandTimeoutSeconds() int {
	if RootCmd.PersistentFlags().Changed("command-timeout-seconds") {
		return commandTimeoutSeconds
	}
	if envVal := os.Getenv(adkAgent.EnvCommandTimeoutSeconds); envVal != "" {
		if val, err := strconv.Atoi(envVal); err == nil {
			return val
		}
	}
	return adkAgent.DefaultCommandTimeoutSeconds
}

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() {
	if err := RootCmd.Execute(); err != nil {
		fmt.Println(err)
		// A named scratchpad entry that does not exist is a usage error, so it exits 2 the way
		// diff does. Everything else keeps the previous generic exit 1.
		var entryMissing *adkAgent.ScratchpadEntryNotFoundError
		if errors.As(err, &entryMissing) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func init() {
	RootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "wackypub.yaml", "config file path")
	RootCmd.PersistentFlags().StringVar(&workspaceDir, "ws", ".", "workspace directory path (defaults to current working directory)")
	RootCmd.PersistentFlags().StringVarP(&modelName, "model", "m", "", "Gemini model override (e.g. gemini-2.5-flash)")
	RootCmd.PersistentFlags().StringVar(&apiKey, "api-key", "", "Gemini API key override (or GEMINI_API_KEY env var)")
	RootCmd.PersistentFlags().IntVar(&maxToolTurns, "max-tool-turns", adkAgent.DefaultMaxToolTurns, "Maximum consecutive tool-call turns allowed per generation")
	RootCmd.PersistentFlags().IntVar(&commandTimeoutSeconds, "command-timeout-seconds", adkAgent.DefaultCommandTimeoutSeconds, "Maximum execution timeout in seconds for tool commands (-1 to disable)")
	RootCmd.AddCommand(skillCmd)
}
