package agent

import (
	"fmt"
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

type CreateScratchpadArgs struct {
	Text string `json:"text" jsonschema_description:"Text content to store in a persistent scratchpad entry"`
}

type CreateScratchpadResult struct {
	ID       string   `json:"id"`
	Size     int      `json:"size"`
	Warning  string   `json:"warning,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

type GetScratchpadArgs struct {
	ID        string `json:"id" jsonschema_description:"4-character ID of the scratchpad entry to read"`
	SkipLines *int   `json:"skip_lines,omitempty" jsonschema_description:"Optional number of lines to skip from the beginning"`
	NumLines  *int   `json:"num_lines,omitempty" jsonschema_description:"Optional maximum number of lines to retrieve"`
}

type GetScratchpadResult struct {
	Output       string `json:"output"`
	Deferred     bool   `json:"deferred,omitempty"`
	ScratchpadID string `json:"scratchpad_id,omitempty"`
}

type ListScratchpadsArgs struct{}

type ListScratchpadsResult struct {
	Entries []ScratchpadItem `json:"entries"`
	Count   int              `json:"count"`
	Cap     int              `json:"cap"`
}

type SearchScratchpadArgs struct {
	ID            string `json:"id" jsonschema_description:"Required scratchpad entry ID to search"`
	Query         string `json:"query" jsonschema_description:"Search query string"`
	CaseSensitive *bool  `json:"case_sensitive,omitempty" jsonschema_description:"Whether search is case-sensitive (default: true)"`
	Regex         bool   `json:"regex,omitempty" jsonschema_description:"Opt-in to treat query as a regular expression (default: false)"`
	MaxResults    int    `json:"max_results,omitempty" jsonschema_description:"Maximum number of matching lines to return (default: 50)"`
}

type DeleteScratchpadArgs struct {
	ID string `json:"id" jsonschema_description:"4-character ID of the scratchpad entry to delete"`
}

type DeleteScratchpadResult struct {
	Status string `json:"status"`
}

// DiffScratchpadArgs names the two entries to compare. Both have to exist: there is no mode
// where a missing side counts as empty, because that turns a typo into a whole-file patch.
type DiffScratchpadArgs struct {
	BeforeID string `json:"before_id" jsonschema_description:"4-character ID of the entry holding the earlier state"`
	AfterID  string `json:"after_id" jsonschema_description:"4-character ID of the entry holding the later state"`
}

type DiffScratchpadResult struct {
	// Diff is the unified patch, empty when the entries are identical.
	Diff string `json:"diff"`
	// Identical lets "did anything change" be read off the result instead of tested for an
	// empty string, which is easy to confuse with an entry that is itself empty.
	Identical bool `json:"identical"`
}

// diffScratchpadToolResult maps a rendered diff onto the tool result. It exists as a function so
// that the promise the result makes, identical exactly when there is no patch, is testable
// without driving a whole model turn to reach the tool handler.
func diffScratchpadToolResult(diff string) DiffScratchpadResult {
	return DiffScratchpadResult{Diff: diff, Identical: diff == ""}
}

// registerScratchpadTools builds the six scratchpad tools (create/get/list/search/delete/diff)
// and registers them via addTool in the canonical order.
func registerScratchpadTools(agentDir string, addTool func(tool.Tool)) error {
	// 1. create_scratchpad
	createTool, err := functiontool.New(functiontool.Config{
		Name:        "create_scratchpad",
		Description: "Store a text payload in a persistent, session-level scratchpad entry. Returns a freshly generated 4-character ID.",
	}, func(ctx agent.Context, args CreateScratchpadArgs) (CreateScratchpadResult, error) {
		entry, err := CreateScratchpad(agentDir, args.Text, "create_scratchpad")
		if err != nil {
			return CreateScratchpadResult{}, fmt.Errorf("failed to create scratchpad entry: %w", err)
		}
		return CreateScratchpadResult{
			ID:       entry.ID,
			Size:     entry.Size,
			Warning:  entry.Warning,
			Warnings: entry.Warnings,
		}, nil
	})
	if err != nil {
		return fmt.Errorf("failed to create create_scratchpad tool: %w", err)
	}
	addTool(createTool)

	// 2. get_scratchpad
	getTool, err := functiontool.New(functiontool.Config{
		Name:        "get_scratchpad",
		Description: "Retrieve stored text from a scratchpad entry by ID, optionally paginated by line range. If the entry contains an image and image support is enabled, it is queued for your next turn.",
	}, func(ctx agent.Context, args GetScratchpadArgs) (GetScratchpadResult, error) {
		filePath, _, isBinary, err := findScratchpadFile(agentDir, args.ID)
		if err != nil {
			return GetScratchpadResult{}, err
		}

		if isBinary {
			header, err := ReadMediaHeader(filePath)
			if err != nil {
				return GetScratchpadResult{}, err
			}
			_, mimeType := DetectMediaType(header)

			// Gating: only defer if image support is enabled on runtime config
			runtimeCfg := loadRuntimeCfgForGating(agentDir)
			if runtimeCfg != nil && runtimeCfg.MaxImageDimension > 0 && strings.HasPrefix(mimeType, "image/") {
				return GetScratchpadResult{
					Output:       fmt.Sprintf("This scratchpad contains an image (%s) that will be available in your next turn.", mimeType),
					Deferred:     true,
					ScratchpadID: args.ID,
				}, nil
			}

			return GetScratchpadResult{}, fmt.Errorf("scratchpad entry %q is binary data (%s) and cannot be read as text", args.ID, mimeType)
		}

		out, err := GetScratchpad(agentDir, args.ID, args.SkipLines, args.NumLines)
		if err != nil {
			return GetScratchpadResult{}, err
		}
		return GetScratchpadResult{Output: out}, nil
	})
	if err != nil {
		return fmt.Errorf("failed to create get_scratchpad tool: %w", err)
	}
	addTool(getTool)

	// 3. list_scratchpads
	listTool, err := functiontool.New(functiontool.Config{
		Name:        "list_scratchpads",
		Description: "List metadata for all currently-live scratchpad entries (ID, size, lines, created_by, is_binary, mime_type), ordered oldest-first, and current capacity usage.",
	}, func(ctx agent.Context, args ListScratchpadsArgs) (ListScratchpadsResult, error) {
		items, count, capVal, err := ListScratchpads(agentDir)
		if err != nil {
			return ListScratchpadsResult{}, fmt.Errorf("failed to list scratchpads: %w", err)
		}
		return ListScratchpadsResult{
			Entries: items,
			Count:   count,
			Cap:     capVal,
		}, nil
	})
	if err != nil {
		return fmt.Errorf("failed to create list_scratchpads tool: %w", err)
	}
	addTool(listTool)

	// 4. search_scratchpad
	searchTool, err := functiontool.New(functiontool.Config{
		Name:        "search_scratchpad",
		Description: "Search a specific text scratchpad entry by ID for matching lines. Returns 1-indexed line numbers and precomputed skip_lines for get_scratchpad pagination.",
	}, func(ctx agent.Context, args SearchScratchpadArgs) (*SearchScratchpadResult, error) {
		return SearchScratchpad(agentDir, args.ID, args.Query, args.CaseSensitive, args.Regex, args.MaxResults)
	})
	if err != nil {
		return fmt.Errorf("failed to create search_scratchpad tool: %w", err)
	}
	addTool(searchTool)

	// 5. delete_scratchpad
	deleteTool, err := functiontool.New(functiontool.Config{
		Name:        "delete_scratchpad",
		Description: "Delete a scratchpad entry by ID. Recommended for releasing large binary entries (images, audio) once they are no longer needed.",
	}, func(ctx agent.Context, args DeleteScratchpadArgs) (DeleteScratchpadResult, error) {
		err := DeleteScratchpad(agentDir, args.ID)
		if err != nil {
			return DeleteScratchpadResult{}, err
		}
		return DeleteScratchpadResult{Status: "deleted"}, nil
	})
	if err != nil {
		return fmt.Errorf("failed to create delete_scratchpad tool: %w", err)
	}
	addTool(deleteTool)

	// 6. diff_scratchpad
	diffTool, err := functiontool.New(functiontool.Config{
		Name:        "diff_scratchpad",
		Description: "Render a unified diff between two text scratchpad entries, so an edit can be verified without re-reading either version back into context. Snapshot the thing you are about to change, change it, snapshot again, then pass the two entry IDs here. Identical entries return an empty diff with identical=true, so checking whether anything moved is a field lookup rather than a string test. Use it to review the blast radius of a refactor or another agent's candidate version. Text entries only, both sides obey the single-read size cap, and this previews without applying.",
	}, func(ctx agent.Context, args DiffScratchpadArgs) (DiffScratchpadResult, error) {
		out, err := diffScratchpadEntriesInDir(agentDir, filepath.Base(agentDir), args.BeforeID, args.AfterID)
		if err != nil {
			return DiffScratchpadResult{}, err
		}
		return diffScratchpadToolResult(out), nil
	})
	if err != nil {
		return fmt.Errorf("failed to create diff_scratchpad tool: %w", err)
	}
	addTool(diffTool)

	return nil
}
