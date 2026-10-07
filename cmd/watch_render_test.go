package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/genai"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
)

// watchTurnContentJSON is the fixture content_json, byte-identical to what a
// session.jsonl line would carry for a turn (the D120 payload). Part order:
// thought, text, function call, text.
const watchTurnContentJSON = `{"role":"model","parts":[{"text":"Reasoning: the build script needs the -race flag.","thought":true},{"text":"Running the build now"},{"functionCall":{"name":"run_command","args":{"args":["bash","-c","go test -race"],"command":"bash"}}},{"text":"Then I will check the output."}]}`

const (
	watchThoughtText    = "Reasoning: the build script needs the -race flag."
	watchToolCallRender = "[tool: run_command({\"args\":[\"bash\",\"-c\",\"go test -race\"],\"command\":\"bash\"})]"
)

// TestRenderTurnBody_FlagCombinations is the per-combination rendering acceptance test.
func TestRenderTurnBody_FlagCombinations(t *testing.T) {
	turn := &agentv1.SessionTurn{Role: "model", Seq: 7, ContentJson: watchTurnContentJSON}

	cases := []struct {
		name      string
		thoughts  bool
		toolCalls bool
		want      string
	}{
		{"flags off (default tail)", false, false, "Running the build now Then I will check the output."},
		{"thoughts only", true, false, "<thought>" + watchThoughtText + "</thought> Running the build now Then I will check the output."},
		{"tool-calls only", false, true, "Running the build now " + watchToolCallRender + " Then I will check the output."},
		{"both flags", true, true, "<thought>" + watchThoughtText + "</thought> Running the build now " + watchToolCallRender + " Then I will check the output."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderTurnBody(turn, tc.thoughts, tc.toolCalls)
			if got != tc.want {
				t.Errorf("renderTurnBody(thoughts=%v, toolCalls=%v):\n got  %q\nwant  %q", tc.thoughts, tc.toolCalls, got, tc.want)
			}
		})
	}
}

// TestRenderTurnBody_ByteFaithfulRoundTrip proves the thought bytes the renderer emits
// are exactly the bytes that were in the content_json (session.jsonl), D120: the
// thought text round-trips through marshal -> render unaltered, tags included.
func TestRenderTurnBody_ByteFaithfulRoundTrip(t *testing.T) {
	thoughtText := `multi  line thought
with a newline and   odd  spacing`
	content := genai.Content{
		Role: "model",
		Parts: []*genai.Part{
			{Text: thoughtText, Thought: true},
			{Text: "visible reply"},
			{FunctionCall: &genai.FunctionCall{Name: "run_command", Args: map[string]any{"command": "bash", "args": []any{"-c", "echo hi"}}}},
		},
	}
	contentJSON, err := json.Marshal(&content)
	if err != nil {
		t.Fatalf("marshal fixture content: %v", err)
	}
	turn := &agentv1.SessionTurn{Role: "model", Seq: 1, ContentJson: string(contentJSON)}

	got := renderTurnBody(turn, true, true)

	wantThought := "<thought>" + thoughtText + "</thought>"
	if !strings.Contains(got, wantThought) {
		t.Errorf("rendered body missing byte-faithful thought segment:\ngot  %q\nwant %q as substring", got, wantThought)
	}
	if want := wantThought + " visible reply"; !strings.HasPrefix(got, want) {
		t.Errorf("rendered body should start with the thought followed by the visible text:\ngot  %q\nwant prefix %q", got, want)
	}
	if want := "[tool: run_command({\"args\":[\"-c\",\"echo hi\"],\"command\":\"bash\"})]"; !strings.Contains(got, want) {
		t.Errorf("rendered body missing tool call %q, got %q", want, got)
	}
}

// TestRenderTurnBody_LegacyPartsFallback: events without content_json (or with
// unparseable content_json) keep the deprecated parts rendering.
func TestRenderTurnBody_LegacyPartsFallback(t *testing.T) {
	noContentJSON := &agentv1.SessionTurn{
		Role:  "user",
		Seq:   2,
		Parts: []*agentv1.SessionPart{{Text: "hello"}, {Text: "world"}},
	}
	if got := renderTurnBody(noContentJSON, true, true); got != "hello world" {
		t.Errorf("legacy rendering: got %q, want %q", got, "hello world")
	}

	badContentJSON := &agentv1.SessionTurn{
		Role:        "user",
		Seq:         3,
		ContentJson: `{"role":"user","parts":[oops`,
		Parts:       []*agentv1.SessionPart{{Text: "fallback"}},
	}
	if got := renderTurnBody(badContentJSON, true, true); got != "fallback" {
		t.Errorf("unparseable content_json fallback: got %q, want %q", got, "fallback")
	}
}

// TestRenderHumanEvent_TurnLineFormat pins the full line shape for one combination.
func TestRenderHumanEvent_TurnLineFormat(t *testing.T) {
	turn := &agentv1.SessionTurn{Role: "model", Seq: 7, ContentJson: watchTurnContentJSON}
	ev := &agentv1.SessionEvent{Seq: 7, Event: &agentv1.SessionEvent_Turn{Turn: turn}}

	out, err := captureStdout(t, func() error {
		renderHumanEvent(ev, true, true)
		return nil
	})
	want := "[Turn #7 model] <thought>" + watchThoughtText + "</thought> Running the build now " + watchToolCallRender + " Then I will check the output.\n"
	if out != want {
		t.Errorf("renderHumanEvent turn line:\ngot  %q\nwant %q", out, want)
	}
	if err != nil {
		t.Fatalf("renderHumanEvent: %v", err)
	}
}

// TestWatchRender_PersistedThoughtRoundTrip is the end-to-end fidelity chain:
// production append -> session.jsonl -> disk read event -> content_json -> render.
// The thought bytes rendered are verified byte-faithful against the on-disk content_json.
func TestWatchRender_PersistedThoughtRoundTrip(t *testing.T) {
	agentDir := t.TempDir()

	thoughtText := "e2e thought  with  double  spaces"
	content := genai.Content{
		Role: "model",
		Parts: []*genai.Part{
			{Text: thoughtText, Thought: true},
			{Text: "persisted reply"},
		},
	}
	if _, _, err := adkAgent.AppendSessionContentGetSeq(agentDir, &content); err != nil {
		t.Fatalf("AppendSessionContentGetSeq: %v", err)
	}

	events, _, _, _, err := adkAgent.ReadSessionEventsFromDisk(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionEventsFromDisk: %v", err)
	}
	var turn *agentv1.SessionTurn
	for _, ev := range events {
		if te, ok := ev.GetEvent().(*agentv1.SessionEvent_Turn); ok && te != nil {
			turn = te.Turn
		}
	}
	if turn == nil || turn.GetContentJson() == "" {
		t.Fatal("no turn event with content_json read back from session.jsonl")
	}

	// The content_json on disk must carry the thought part byte-faithfully.
	var onDisk genai.Content
	if err := json.Unmarshal([]byte(turn.GetContentJson()), &onDisk); err != nil {
		t.Fatalf("unmarshal content_json: %v", err)
	}
	if len(onDisk.Parts) != 2 || onDisk.Parts[0] == nil || !onDisk.Parts[0].Thought || onDisk.Parts[0].Text != thoughtText {
		t.Fatalf("on-disk content_json lost the thought part or its bytes: %+v", onDisk.Parts)
	}

	got := renderTurnBody(turn, true, false)
	want := "<thought>" + thoughtText + "</thought> persisted reply"
	if got != want {
		t.Errorf("rendered persisted turn:\ngot  %q\nwant %q", got, want)
	}
}
