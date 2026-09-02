package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"google.golang.org/genai"
)

// TraceOptions configures causal graph traversal and formatting according to D36.
type TraceOptions struct {
	MaxSteps  int
	Verbosity int
}

// DefaultTraceOptions returns standard defaults for tracing.
func DefaultTraceOptions() TraceOptions {
	return TraceOptions{
		MaxSteps:  20,
		Verbosity: 1,
	}
}

// TraceStep represents a single hop in the causal trace chain.
type TraceStep struct {
	StepIndex        int              `json:"step_index"`
	AgentID          string           `json:"agent_id"`
	CommitSHA        string           `json:"commit_sha"`
	ShortSHA         string           `json:"short_sha"`
	EventType        string           `json:"event_type"`
	A2AMetadata      *A2AMetadata     `json:"a2a_metadata,omitempty"`
	RawCommitMessage string           `json:"raw_commit_message"`
	TurnContent      *genai.Content   `json:"turn_content,omitempty"`
	TurnContents     []*genai.Content `json:"turn_contents,omitempty"`
}

// TraceResult holds the ordered steps of a causal trace.
type TraceResult struct {
	TargetAgentID string      `json:"target_agent_id,omitempty"`
	TargetCommit  string      `json:"target_commit,omitempty"`
	TraceID       string      `json:"trace_id,omitempty"`
	Steps         []TraceStep `json:"steps"`
}

// ResolveCommitHash resolves a commit specifier (SHA, prefix, suffix, branch, tag, HEAD~N) in repo.
func ResolveCommitHash(repo *git.Repository, spec string) (plumbing.Hash, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return plumbing.ZeroHash, fmt.Errorf("empty commit specifier")
	}

	// 1. Try direct git.Revision resolution (HEAD, refs, tags, SHA)
	if hash, err := repo.ResolveRevision(plumbing.Revision(spec)); err == nil && hash != nil {
		return *hash, nil
	}

	// 2. Iterate all commit objects to match prefix or suffix
	cIter, err := repo.CommitObjects()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to list commit objects: %w", err)
	}

	var matchedHashes []plumbing.Hash
	lowerSpec := strings.ToLower(spec)

	_ = cIter.ForEach(func(c *object.Commit) error {
		shaStr := c.Hash.String()
		if strings.HasPrefix(shaStr, lowerSpec) || strings.HasSuffix(shaStr, lowerSpec) {
			matchedHashes = append(matchedHashes, c.Hash)
		}
		return nil
	})

	if len(matchedHashes) == 1 {
		return matchedHashes[0], nil
	}
	if len(matchedHashes) > 1 {
		return plumbing.ZeroHash, fmt.Errorf("ambiguous commit specifier %q matches %d commits", spec, len(matchedHashes))
	}

	return plumbing.ZeroHash, fmt.Errorf("commit specifier %q not found in repository", spec)
}

func extractSessionTurnsAtCommit(cObj *object.Commit) ([]*genai.Content, error) {
	if cObj == nil {
		return nil, nil
	}
	tree, err := cObj.Tree()
	if err != nil {
		return nil, err
	}
	file, err := tree.File(SessionFileName)
	if err != nil {
		return nil, err
	}
	contentStr, err := file.Contents()
	if err != nil {
		return nil, err
	}

	lines := strings.Split(contentStr, "\n")
	var turns []*genai.Content
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			continue
		}
		var turn genai.Content
		if err := json.Unmarshal([]byte(trimmed), &turn); err == nil {
			turns = append(turns, &turn)
		}
	}
	return turns, nil
}

func extractTurnDiff(cObj *object.Commit) []*genai.Content {
	currentTurns, err := extractSessionTurnsAtCommit(cObj)
	if err != nil || len(currentTurns) == 0 {
		return nil
	}

	if cObj.NumParents() == 0 {
		return currentTurns
	}

	parentObj, err := cObj.Parent(0)
	if err != nil {
		return currentTurns
	}

	parentTurns, err := extractSessionTurnsAtCommit(parentObj)
	if err != nil || len(parentTurns) == 0 {
		return currentTurns
	}

	if len(currentTurns) > len(parentTurns) {
		return currentTurns[len(parentTurns):]
	}

	return currentTurns[len(currentTurns)-1:]
}

// TraceAgentCommit traces backward starting from <agentID> at <commitSpec>.
func TraceAgentCommit(wsDir, agentID, commitSpec string, opts TraceOptions) (*TraceResult, error) {
	if wsDir == "" || agentID == "" || commitSpec == "" {
		return nil, fmt.Errorf("workspace directory, agentID, and commitSpec cannot be empty")
	}

	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 20
	}

	res := &TraceResult{
		TargetAgentID: agentID,
		TargetCommit:  commitSpec,
		Steps:         make([]TraceStep, 0),
	}

	currentAgentID := agentID
	currentCommitSpec := commitSpec
	visited := make(map[string]bool)

	for len(res.Steps) < opts.MaxSteps {
		agentDir := filepath.Join(wsDir, currentAgentID)
		if !IsWorkspaceGitRepo(agentDir) {
			break
		}

		repo, err := git.PlainOpen(agentDir)
		if err != nil {
			reportGitFailure(agentDir, "trace", err)
			break
		}
		clearGitFailure(agentDir, "trace")

		hash, err := ResolveCommitHash(repo, currentCommitSpec)
		if err != nil {
			break
		}

		visitKey := fmt.Sprintf("%s@%s", currentAgentID, hash.String())
		if visited[visitKey] {
			break
		}
		visited[visitKey] = true

		cObj, err := repo.CommitObject(hash)
		if err != nil {
			break
		}

		// Parse commit message body for eventType and A2A metadata
		eventType, a2aMeta := parseCommitMessage(cObj.Message)
		if res.TraceID == "" && a2aMeta != nil && a2aMeta.TraceID != "" {
			res.TraceID = a2aMeta.TraceID
		}

		turnDiff := extractTurnDiff(cObj)

		step := TraceStep{
			StepIndex:        len(res.Steps) + 1,
			AgentID:          currentAgentID,
			CommitSHA:        hash.String(),
			ShortSHA:         shortHash(hash.String()),
			EventType:        eventType,
			A2AMetadata:      a2aMeta,
			RawCommitMessage: cObj.Message,
			TurnContents:     turnDiff,
		}
		if len(turnDiff) > 0 {
			step.TurnContent = turnDiff[0]
		}

		res.Steps = append(res.Steps, step)

		// Check if we should hop to a parent/calling agent repository
		var nextAgentID, nextCommitSHA string

		if a2aMeta != nil && a2aMeta.CallerID != "" && a2aMeta.Metadata != nil {
			rev := a2aMeta.Metadata["workspace_revision"]
			if rev != "" && a2aMeta.CallerID != currentAgentID {
				callerDir := filepath.Join(wsDir, a2aMeta.CallerID)
				if IsWorkspaceGitRepo(callerDir) {
					nextAgentID = a2aMeta.CallerID
					nextCommitSHA = rev
				}
			}
		}

		if nextAgentID != "" && nextCommitSHA != "" {
			currentAgentID = nextAgentID
			currentCommitSpec = nextCommitSHA
			continue
		}

		// Otherwise, step backward to commit parent in the same agent repo
		if cObj.NumParents() > 0 {
			parentHash := cObj.ParentHashes[0]
			currentCommitSpec = parentHash.String()
		} else {
			break
		}
	}

	return res, nil
}

// TraceByTraceID searches all agent repos for commits matching <traceID> and builds the trace chain.
func TraceByTraceID(wsDir, traceID string, opts TraceOptions) (*TraceResult, error) {
	if wsDir == "" || traceID == "" {
		return nil, fmt.Errorf("workspace directory and traceID cannot be empty")
	}

	sdk := NewSDK(wsDir)
	agentIDs, err := sdk.ListAgents()
	if err != nil {
		return nil, fmt.Errorf("failed to list agents: %w", err)
	}

	var latestAgentID string
	var latestCommitSHA string
	var latestTime int64

	for _, agentID := range agentIDs {
		agentDir := sdk.AgentDir(agentID)
		if !IsWorkspaceGitRepo(agentDir) {
			continue
		}
		repo, err := git.PlainOpen(agentDir)
		if err != nil {
			reportGitFailure(agentDir, "trace-search", err)
			continue
		}
		clearGitFailure(agentDir, "trace-search")

		cIter, err := repo.CommitObjects()
		if err != nil {
			continue
		}

		_ = cIter.ForEach(func(c *object.Commit) error {
			_, meta := parseCommitMessage(c.Message)
			if meta != nil && meta.TraceID == traceID {
				if c.Committer.When.UnixNano() >= latestTime {
					latestTime = c.Committer.When.UnixNano()
					latestAgentID = agentID
					latestCommitSHA = c.Hash.String()
				}
			}
			return nil
		})
	}

	if latestAgentID == "" || latestCommitSHA == "" {
		return nil, fmt.Errorf("no commits found matching trace_id %q across workspace repositories", traceID)
	}

	res, err := TraceAgentCommit(wsDir, latestAgentID, latestCommitSHA, opts)
	if err != nil {
		return nil, err
	}
	res.TraceID = traceID
	return res, nil
}

// FormatTraceResult formats a TraceResult to a readable string based on opts.Verbosity (0..4).
func FormatTraceResult(wsDir string, res *TraceResult, opts TraceOptions) string {
	if res == nil || len(res.Steps) == 0 {
		return "No trace steps found."
	}

	var sb strings.Builder
	headerTarget := res.TargetAgentID
	if headerTarget == "" {
		headerTarget = "trace_id:" + res.TraceID
	} else {
		headerTarget = fmt.Sprintf("%s @ %s", res.TargetAgentID, res.TargetCommit)
	}

	sb.WriteString(fmt.Sprintf("Trace for %s (trace_id: %s)\n", headerTarget, res.TraceID))
	sb.WriteString(strings.Repeat("-", 80) + "\n")

	for _, step := range res.Steps {
		sb.WriteString(fmt.Sprintf("[Step %d] %s @ %s (%s)\n", step.StepIndex, step.AgentID, step.ShortSHA, step.EventType))

		if step.A2AMetadata != nil && step.A2AMetadata.CallerID != "" {
			sb.WriteString(fmt.Sprintf("        Caller:  %s\n", step.A2AMetadata.CallerID))
		}

		switch opts.Verbosity {
		case 0:
			renderVerbosity0(&sb, step)
		case 1:
			renderVerbosity1(&sb, step)
		case 2:
			renderVerbosity2(&sb, step)
		case 3:
			renderVerbosity3(&sb, step)
		case 4:
			renderVerbosity4(&sb, step)
		default:
			renderVerbosity1(&sb, step)
		}
		sb.WriteString("\n")
	}

	sb.WriteString(strings.Repeat("-", 80) + "\n")
	sb.WriteString(fmt.Sprintf("Trace complete: %d steps traced.\n", len(res.Steps)))
	return sb.String()
}

func parseCommitMessage(msg string) (string, *A2AMetadata) {
	lines := strings.Split(strings.TrimSpace(msg), "\n")
	eventType := "event"
	if len(lines) > 0 {
		eventType = strings.TrimSpace(lines[0])
	}

	var a2aMeta *A2AMetadata
	idx := strings.Index(msg, "AGENT2AGENT:")
	if idx != -1 {
		jsonStr := strings.TrimSpace(msg[idx+len("AGENT2AGENT:"):])
		_ = json.Unmarshal([]byte(jsonStr), &a2aMeta)
	}

	return eventType, a2aMeta
}

func shortHash(hash string) string {
	if len(hash) >= 7 {
		return hash[:7]
	}
	return hash
}

func renderVerbosity0(sb *strings.Builder, step TraceStep) {
	if len(step.TurnContents) == 0 {
		sb.WriteString(fmt.Sprintf("        Details: %s\n", step.EventType))
		return
	}
	for _, turn := range step.TurnContents {
		if turn.Role == "user" {
			txt := ContentText(turn)
			if len(txt) > 60 {
				txt = txt[:57] + "..."
			}
			sb.WriteString(fmt.Sprintf("        User:    %q\n", txt))
		} else {
			for _, p := range turn.Parts {
				if p.FunctionCall != nil {
					sb.WriteString(fmt.Sprintf("        Call:    %s\n", p.FunctionCall.Name))
				}
			}
		}
	}
}

func renderVerbosity1(sb *strings.Builder, step TraceStep) {
	if len(step.TurnContents) == 0 {
		sb.WriteString(fmt.Sprintf("        Details: %s\n", step.EventType))
		return
	}
	for _, turn := range step.TurnContents {
		if turn.Role == "user" {
			sb.WriteString(fmt.Sprintf("        Prompt:  %s\n", ContentText(turn)))
		} else {
			for _, p := range turn.Parts {
				if p.FunctionCall != nil {
					cmdArg := ""
					if p.FunctionCall.Name == "run_command" && p.FunctionCall.Args != nil {
						if c, ok := p.FunctionCall.Args["command"].(string); ok {
							cmdArg = fmt.Sprintf(" command=%q", c)
						}
					}
					sb.WriteString(fmt.Sprintf("        Tool:    %s%s\n", p.FunctionCall.Name, cmdArg))
				} else if p.FunctionResponse != nil {
					sb.WriteString(fmt.Sprintf("        Output:  %s\n", p.FunctionResponse.Name))
				} else if p.Text != "" && !p.Thought {
					sb.WriteString(fmt.Sprintf("        Message: %s\n", p.Text))
				}
			}
		}
	}
}

func renderVerbosity2(sb *strings.Builder, step TraceStep) {
	if len(step.TurnContents) == 0 {
		sb.WriteString(fmt.Sprintf("        Details: %s\n", step.EventType))
		return
	}
	reThinking := regexp.MustCompile(`(?s)<think>.*?</think>`)
	for _, turn := range step.TurnContents {
		sb.WriteString(fmt.Sprintf("        Role:    %s\n", turn.Role))
		for _, p := range turn.Parts {
			if p.Thought {
				continue
			}
			if p.Text != "" {
				text := reThinking.ReplaceAllString(p.Text, "")
				if strings.TrimSpace(text) != "" {
					sb.WriteString(fmt.Sprintf("        Text:\n%s\n", indentText(strings.TrimSpace(text), "          ")))
				}
			}
			if p.FunctionCall != nil {
				argsBytes, _ := json.Marshal(p.FunctionCall.Args)
				sb.WriteString(fmt.Sprintf("        ToolCall: %s(%s)\n", p.FunctionCall.Name, string(argsBytes)))
			}
			if p.FunctionResponse != nil {
				respBytes, _ := json.Marshal(p.FunctionResponse.Response)
				sb.WriteString(fmt.Sprintf("        ToolResp: %s => %s\n", p.FunctionResponse.Name, string(respBytes)))
			}
		}
	}
}

func renderVerbosity3(sb *strings.Builder, step TraceStep) {
	if len(step.TurnContents) == 0 {
		sb.WriteString(fmt.Sprintf("        Details: %s\n", step.EventType))
		return
	}
	for _, turn := range step.TurnContents {
		sb.WriteString(fmt.Sprintf("        Role:    %s\n", turn.Role))
		for _, p := range turn.Parts {
			if p.Thought || strings.Contains(p.Text, "<think>") {
				sb.WriteString(fmt.Sprintf("        Thinking:\n%s\n", indentText(strings.TrimSpace(p.Text), "          ")))
			} else if p.Text != "" {
				sb.WriteString(fmt.Sprintf("        Text:\n%s\n", indentText(strings.TrimSpace(p.Text), "          ")))
			}
			if p.FunctionCall != nil {
				argsBytes, _ := json.Marshal(p.FunctionCall.Args)
				sb.WriteString(fmt.Sprintf("        ToolCall: %s(%s)\n", p.FunctionCall.Name, string(argsBytes)))
			}
			if p.FunctionResponse != nil {
				respBytes, _ := json.Marshal(p.FunctionResponse.Response)
				sb.WriteString(fmt.Sprintf("        ToolResp: %s => %s\n", p.FunctionResponse.Name, string(respBytes)))
			}
		}
	}
}

func renderVerbosity4(sb *strings.Builder, step TraceStep) {
	if len(step.TurnContents) > 0 {
		sb.WriteString("        Turns (JSONL):\n")
		for _, turn := range step.TurnContents {
			bytes, _ := json.Marshal(turn)
			sb.WriteString(fmt.Sprintf("          %s\n", string(bytes)))
		}
	}
	sb.WriteString(fmt.Sprintf("        Commit Message:\n%s\n", indentText(strings.TrimSpace(step.RawCommitMessage), "          ")))
}

func indentText(text, indent string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = indent + l
	}
	return strings.Join(lines, "\n")
}
