package agent

import (
	"fmt"
	"strconv"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"google.golang.org/genai"
)

// MEMORY.md is managed by the compaction writer through a small YAML
// frontmatter block. The keys are owned by that writer - the model never
// emits them. compact-cycle counts this file's compaction passes (minor and
// major); last-major-cycle is the cycle on which the last major pass ran,
// which is what makes the cadence cyclical: the pass after a major is a
// minor again, so the file cycles minor/minor/.../major/minor/... instead of
// degrading into permanent full rewrites once a major has run.
const (
	memoryCompactCycleKey   = "compact-cycle"
	memoryLastMajorCycleKey = "last-major-cycle"
)

// ledgerAddendumHeading is the section minor passes append under. A major
// pass terminates its output with this heading and nothing after it, which is
// what hands the next pass back to the minor branch.
const ledgerAddendumHeading = "## Ledger Addendum"

// splitMemoryFrontmatter separates MEMORY.md's frontmatter from its body.
// It returns the frontmatter mapping node (nil when absent), the body, and
// whether a parseable frontmatter block was present. A file without
// frontmatter - or with an unparseable block - is returned as-is with
// present=false so the managed keys can never clobber whatever the agent
// wrote there.
func splitMemoryFrontmatter(content string) (*yaml.Node, string, bool) {
	body := strings.TrimSpace(content)
	if !strings.HasPrefix(body, "---") {
		return nil, body, false
	}
	parts := strings.SplitN(body[3:], "---", 2)
	if len(parts) != 2 {
		return nil, body, false
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(parts[0]), &doc); err != nil {
		return nil, strings.TrimSpace(content), false
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, strings.TrimSpace(content), false
	}
	return doc.Content[0], strings.TrimSpace(parts[1]), true
}

func memoryIntFromNode(mapping *yaml.Node, key string) (int, bool) {
	if mapping == nil {
		return 0, false
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			var v int
			if err := mapping.Content[i+1].Decode(&v); err != nil {
				return 0, false
			}
			return v, true
		}
	}
	return 0, false
}

// setMemoryKey upserts key into the frontmatter mapping, preserving the
// order of existing keys (new keys append at the end).
func setMemoryKey(mapping *yaml.Node, key string, v int) *yaml.Node {
	if mapping == nil {
		mapping = &yaml.Node{Kind: yaml.MappingNode}
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1].Value = strconv.Itoa(v)
			mapping.Content[i+1].Tag = "!!int"
			return mapping
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(v)},
	)
	return mapping
}

func renderMemoryContent(mapping *yaml.Node, body string) string {
	if mapping == nil || len(mapping.Content) == 0 {
		return body + "\n"
	}
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(mapping); err != nil {
		// an unmarshalable node would be a bug in the managed keys; fall back
		// to emitting the body so the pass still ships
		return body + "\n"
	}
	enc.Close()
	return "---\n" + strings.TrimRight(buf.String(), "\n") + "\n---\n" + body + "\n"
}

// memoryCycleOf returns the compact-cycle value of a MEMORY.md document
// (0 for a legacy file without managed frontmatter).
func memoryCycleOf(content string) int {
	mapping, _, present := splitMemoryFrontmatter(content)
	if !present {
		return 0
	}
	c, _ := memoryIntFromNode(mapping, memoryCompactCycleKey)
	return c
}

// decideMemoryPass reports whether this compaction cycle runs the major
// branch. Bootstrap rule: the first compaction on a nonexistent MEMORY.md is
// always minor. A cycle directly after a major is always minor (the cyclical
// reset). Otherwise either trigger qualifies: major-compact-every counts
// cycles, major-compact-memory-pct measures the file itself against the
// context window.
func decideMemoryPass(existingMemory string, cfg *CompactConfig, runtimeCfg *RuntimeConfig) bool {
	if existingMemory == "" {
		return false
	}
	mapping, _, present := splitMemoryFrontmatter(existingMemory)
	if !present {
		mapping = nil
	}
	nextCycle := memoryCycleOf(existingMemory) + 1
	lastMajor, hasLast := memoryIntFromNode(mapping, memoryLastMajorCycleKey)
	if hasLast && lastMajor == nextCycle-1 {
		return false
	}
	if cfg.MajorCompactEvery > 0 && nextCycle%cfg.MajorCompactEvery == 0 {
		return true
	}
	return memoryExceedsPct(existingMemory, cfg, runtimeCfg)
}

// memoryExceedsPct reports whether the memory file crosses
// major-compact-memory-pct and the cycling guard allows a major this cycle.
// It is the ONLY session-independent major trigger: major-compact-every is a
// mode selector for compactions triggered by other means, never a trigger
// itself.
func memoryExceedsPct(existingMemory string, cfg *CompactConfig, runtimeCfg *RuntimeConfig) bool {
	if existingMemory == "" || cfg == nil || cfg.MajorCompactMemoryPct <= 0 {
		return false
	}
	if runtimeCfg == nil || runtimeCfg.ContextWindow <= 0 {
		return false
	}
	mapping, _, present := splitMemoryFrontmatter(existingMemory)
	if !present {
		mapping = nil
	}
	nextCycle := memoryCycleOf(existingMemory) + 1
	lastMajor, hasLast := memoryIntFromNode(mapping, memoryLastMajorCycleKey)
	if hasLast && lastMajor == nextCycle-1 {
		return false
	}
	memTurn := genai.NewContentFromText(existingMemory, "user")
	memTokens := EstimateTokens([]*genai.Content{memTurn}, false)
	return float64(memTokens) >= cfg.MajorCompactMemoryPct/100.0*float64(runtimeCfg.ContextWindow)
}

// memoryNeedsMajor reports whether the agent's memory file alone justifies a
// major pass right now: it crosses major-compact-memory-pct and the cycling
// guard allows a major this cycle. major-compact-every is a mode selector,
// not a trigger. Read errors are treated as "not needed" - a broken memory
// read must not force a compaction on every turn.
func memoryNeedsMajor(agentDir string, runtimeCfg *RuntimeConfig) bool {
	mem, err := ReadMemoryFile(agentDir)
	if err != nil || mem == "" {
		return false
	}
	cfg, err := LoadCompactConfig(agentDir)
	if err != nil || cfg == nil {
		return false
	}
	return memoryExceedsPct(mem, cfg, runtimeCfg)
}

// appendAddendumToLedger places a minor-pass addendum under the document's
// Ledger Addendum section, creating the section at the end of the document
// when it does not exist yet.
func appendAddendumToLedger(body, addendum string) string {
	body = strings.TrimSpace(body)
	addendum = strings.TrimSpace(addendum)
	if addendum == "" {
		return body
	}
	if body == "" {
		return ledgerAddendumHeading + "\n" + addendum
	}
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == ledgerAddendumHeading {
			end := i + 1
			for end < len(lines) && !isHeadingLine(lines[end]) {
				end++
			}
			out := make([]string, 0, len(lines)+1)
			out = append(out, lines[:end]...)
			out = append(out, addendum)
			out = append(out, lines[end:]...)
			return strings.Join(out, "\n")
		}
	}
	return body + "\n\n" + ledgerAddendumHeading + "\n" + addendum
}

// applyMinorAppend produces the new MEMORY.md for a minor (append) pass:
// the existing document with the addendum placed under ## Ledger Addendum and
// the cycle counter bumped. The document's narrative is never rewritten.
func applyMinorAppend(existing, addendum string, nextCycle int) string {
	mapping, body, present := splitMemoryFrontmatter(existing)
	if !present {
		mapping = nil
	}
	mapping = setMemoryKey(mapping, memoryCompactCycleKey, nextCycle)
	return renderMemoryContent(mapping, appendAddendumToLedger(body, addendum))
}

// applyMajorRewrite produces the new MEMORY.md for a major (full rewrite)
// pass: the model's replacement document under freshly managed frontmatter,
// ending with an empty Ledger Addendum heading so the very next pass is a
// minor append again (the cyclical reset, enforced structurally rather than
// left to the model).
func applyMajorRewrite(output string, nextCycle int) string {
	mapping, body, present := splitMemoryFrontmatter(output)
	if !present {
		mapping = nil
	}
	mapping = setMemoryKey(mapping, memoryCompactCycleKey, nextCycle)
	mapping = setMemoryKey(mapping, memoryLastMajorCycleKey, nextCycle)
	if !strings.HasSuffix(strings.TrimRight(body, "\n"), ledgerAddendumHeading) {
		body = strings.TrimRight(body, "\n") + "\n\n" + ledgerAddendumHeading
	}
	return renderMemoryContent(mapping, body)
}

// compactionModeTemplate is appended to the base compaction directive
// (COMPACT.md's body) and rendered with the .IsAppend/.IsMajor conditionals.
// The base prompt's strict guidelines apply to both branches; this block
// only fixes the pass mode and the output shape.
const compactionModeTemplate = `

{{if .IsMajor}}
### PASS MODE: MAJOR - FULL CONSOLIDATION REWRITE
This pass REPLACES <PERSISTENT_MEMORY> entirely. Read the ENTIRE existing <PERSISTENT_MEMORY> (structured sections plus any accumulated Ledger Addendum bullets) together with the archived turns, and produce a single deduplicated replacement document.

- NOTHING carries over automatically: anything worth keeping from the old document must appear in your output, or it is gone.
- Each fact appears ONCE, in its FINAL state. Fold the addenda and invalidate: if they show an item completed, abandoned, or superseded, write only the final state - do not carry forward stale in-progress notes.
- Use exactly these section headings, in this order:
  # Task Overview
  ## Current State
  ## Key Decisions & Discoveries
  ## Next Steps
  ## Context to Preserve
- If the input has a ## Claims section, carry its claim lines VERBATIM into a ## Claims section of the output, dropping only claims the archived turns explicitly close.
- Terminate the document with an empty ## Ledger Addendum heading (nothing after it).
- Output ONLY the replacement document: no YAML frontmatter (the system manages it), no code fences around it, no introductory or concluding text.
{{else}}
### PASS MODE: MINOR - APPEND
This pass appends to the existing <PERSISTENT_MEMORY>. The system places your output under the document's ## Ledger Addendum section. Emit ONLY the raw markdown bullet points (each line starting with *): no section headers, no code fences, no introductory or concluding text.
{{end}}
`

// renderCompactionDirective renders the mode block and appends it to the
// base directive. The base text is never parsed as a template: it is
// agent-authored COMPACT.md content, and only this managed block carries the
// {{if}} conditionals.
func renderCompactionDirective(base string, isMajor bool) (string, error) {
	tmpl, err := template.New("compaction-mode").Parse(compactionModeTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse compaction mode template: %w", err)
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, struct {
		IsAppend bool
		IsMajor  bool
	}{!isMajor, isMajor}); err != nil {
		return "", fmt.Errorf("failed to render compaction mode template: %w", err)
	}
	return base + buf.String(), nil
}
