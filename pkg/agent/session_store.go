package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/genai"
)

const SessionFileName = "session.jsonl"

// ReadSessionTurns reads all turns from <agent_dir>/session.jsonl as genai.Content objects.
// If the file does not exist, returns an empty list without error.
func ReadSessionTurns(agentDir string) ([]*genai.Content, error) {
	sessionPath := filepath.Join(agentDir, "session.jsonl")

	file, err := os.Open(sessionPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to open session file at %s: %w", sessionPath, err)
	}
	defer file.Close()

	var turns []*genai.Content
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB max line size for large parts
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var turn genai.Content
		if err := json.Unmarshal(line, &turn); err != nil {
			// Skip corrupted lines gracefully
			continue
		}
		turns = append(turns, &turn)
	}

	if err := scanner.Err(); err != nil {
		return turns, fmt.Errorf("error reading session file at %s: %w", sessionPath, err)
	}

	return turns, nil
}

// AppendSessionContent appends a genai.Content turn to <agent_dir>/session.jsonl.
// If the file exists and its last byte is not a newline (e.g. from a hand-edit that
// dropped the trailing newline), a healing '\n' is written first so the new turn lands
// on its own line rather than being merged with the previous one. See D75.
func AppendSessionContent(agentDir string, content *genai.Content) error {
	sessionPath := filepath.Join(agentDir, SessionFileName)

	data, err := json.Marshal(content)
	if err != nil {
		return fmt.Errorf("failed to marshal content: %w", err)
	}
	data = append(data, '\n')

	// O_RDWR required for ReadAt (used to check the last byte below).
	// O_APPEND ensures writes are still atomically forced to end-of-file.
	file, err := os.OpenFile(sessionPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open %s for writing: %w", SessionFileName, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat %s: %w", SessionFileName, err)
	}
	if info.Size() > 0 {
		lastByte := make([]byte, 1)
		if _, err := file.ReadAt(lastByte, info.Size()-1); err != nil {
			return fmt.Errorf("failed to read last byte of %s: %w", SessionFileName, err)
		}
		if lastByte[0] != '\n' {
			if _, err := file.Write([]byte{'\n'}); err != nil {
				return fmt.Errorf("failed to write healing newline to %s: %w", SessionFileName, err)
			}
		}
	}

	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("failed to write content to %s: %w", SessionFileName, err)
	}

	return nil
}

// AppendSessionTurn is a convenience wrapper that appends a simple text turn.
func AppendSessionTurn(agentDir string, role string, text string) error {
	return AppendSessionContent(agentDir, genai.NewContentFromText(text, genai.Role(role)))
}

// WriteSessionTurns overwrites <agent_dir>/session.jsonl with a new list of turns.
func WriteSessionTurns(agentDir string, turns []*genai.Content) error {
	sessionPath := filepath.Join(agentDir, "session.jsonl")

	file, err := os.Create(sessionPath)
	if err != nil {
		return fmt.Errorf("failed to create session.jsonl: %w", err)
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	for _, t := range turns {
		data, err := json.Marshal(t)
		if err != nil {
			continue
		}
		if _, err := writer.Write(data); err != nil {
			return fmt.Errorf("failed writing turn to session.jsonl: %w", err)
		}
		if err := writer.WriteByte('\n'); err != nil {
			return fmt.Errorf("failed writing newline to session.jsonl: %w", err)
		}
	}

	return writer.Flush()
}

// ContentText extracts the concatenated final-answer text from a genai.Content's parts,
// excluding any parts marked as Thought (reasoning/thinking output).
func ContentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var text string
	for _, p := range c.Parts {
		if p != nil && p.Text != "" && !p.Thought {
			text += p.Text
		}
	}
	return text
}

// EstimateTokens calculates an approximate token count for session turns.
// includeThinking should match RuntimeConfig.PreserveThinking: when true,
// Thought-marked part text is counted too, since it's actually replayed to
// the model on every subsequent request for backends that preserve thinking.
// Accounts for text, tool calls, tool responses, and inline images.
func EstimateTokens(turns []*genai.Content, includeThinking bool) int {
	totalTokens := 0
	for _, t := range turns {
		if t == nil {
			continue
		}
		chars := 0
		for _, p := range t.Parts {
			if p == nil {
				continue
			}
			if p.Text != "" {
				if includeThinking || !p.Thought {
					chars += len(p.Text)
				}
			}
			if p.FunctionCall != nil {
				chars += len(p.FunctionCall.Name)
				if p.FunctionCall.Args != nil {
					b, _ := json.Marshal(p.FunctionCall.Args)
					chars += len(b)
				}
			}
			if p.FunctionResponse != nil {
				chars += len(p.FunctionResponse.Name)
				if p.FunctionResponse.Response != nil {
					b, _ := json.Marshal(p.FunctionResponse.Response)
					chars += len(b)
				}
			}
			if p.InlineData != nil && len(p.InlineData.Data) > 0 {
				rawLen := len(p.InlineData.Data)
				b64Len := (rawLen + 2) / 3 * 4
				totalTokens += b64Len / 150
			}
		}
		if chars > 0 {
			totalTokens += (chars + 3) / 4
		}
	}
	return totalTokens
}

// contentTextAll extracts the concatenated text from all of a genai.Content's
// parts, including Thought-marked ones.
func contentTextAll(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var text string
	for _, p := range c.Parts {
		if p != nil && p.Text != "" {
			text += p.Text
		}
	}
	return text
}

// CleanSessionTurns sanitizes a sequence of conversation turns for model requests by:
// 1. Removing dangling FunctionResponse parts (responses without a matching FunctionCall in the preceding model turn).
// 2. Pruning empty turns (turns with zero parts remaining after filtering).
// 3. Merging consecutive "user"-role turns into a single user turn per run, concatenating their parts in order.
//
// session.jsonl intentionally allows consecutive user turns to accumulate —
// multiple `add` calls without an intervening `generate`, and, on every
// generation, the injected system-prompt+memory turn landing immediately
// before whatever the first real turn happens to be (itself usually "user").
// That's fine for storage, but many OpenAI-compatible chat templates reject
// or silently mishandle non-alternating roles. Furthermore, LLM backends
// (OpenAI, Anthropic, Gemini) reject requests with a 400 Bad Request error if
// a FunctionResponse appears without a matching FunctionCall in the immediately
// preceding assistant message (e.g. if compaction cut history mid-exchange).
// This normalizes the sequence right before it's sent to a model, without touching
// what's stored on disk; callers should apply it to the Contents slice built for
// a model.LLMRequest, not to what gets persisted via AppendSessionContent/WriteSessionTurns.
func CleanSessionTurns(contents []*genai.Content) []*genai.Content {
	if len(contents) == 0 {
		return nil
	}

	// 1. Merge consecutive user turns first so that related user turns
	// (e.g. text + responses or multiple user turns between model turns)
	// are grouped together before validating against the preceding model turn.
	mergedUserTurns := make([]*genai.Content, 0, len(contents))
	for _, c := range contents {
		if c == nil || len(c.Parts) == 0 {
			continue
		}
		if n := len(mergedUserTurns); n > 0 && mergedUserTurns[n-1].Role == "user" && c.Role == "user" {
			combinedParts := make([]*genai.Part, 0, len(mergedUserTurns[n-1].Parts)+len(c.Parts))
			combinedParts = append(combinedParts, mergedUserTurns[n-1].Parts...)
			combinedParts = append(combinedParts, c.Parts...)
			mergedUserTurns[n-1] = &genai.Content{Role: "user", Parts: combinedParts}
			continue
		}
		partsCopy := make([]*genai.Part, len(c.Parts))
		copy(partsCopy, c.Parts)
		mergedUserTurns = append(mergedUserTurns, &genai.Content{
			Role:  c.Role,
			Parts: partsCopy,
		})
	}

	// 2. Validate and filter parts (remove dangling FunctionResponses, strip nil parts).
	// Also prune any turns that become empty after filtering.
	cleaned := make([]*genai.Content, 0, len(mergedUserTurns))
	for _, turn := range mergedUserTurns {
		var prevModelTurn *genai.Content
		if len(cleaned) > 0 && cleaned[len(cleaned)-1].Role == "model" {
			prevModelTurn = cleaned[len(cleaned)-1]
		}

		var availableCalls []*genai.FunctionCall
		if prevModelTurn != nil {
			for _, p := range prevModelTurn.Parts {
				if p != nil && p.FunctionCall != nil {
					availableCalls = append(availableCalls, p.FunctionCall)
				}
			}
		}
		usedCalls := make([]bool, len(availableCalls))

		validParts := make([]*genai.Part, 0, len(turn.Parts))
		for _, p := range turn.Parts {
			if p == nil {
				continue
			}

			// If this part is a FunctionResponse, check if it matches an unconsumed FunctionCall in prevModelTurn
			if p.FunctionResponse != nil {
				if prevModelTurn == nil || len(availableCalls) == 0 {
					// No preceding model turn or preceding model turn had no function calls -> dangling!
					continue
				}

				resp := p.FunctionResponse
				matchedIdx := -1

				// 1st pass: try exact ID match if response ID is non-empty
				if resp.ID != "" {
					for idx, call := range availableCalls {
						if !usedCalls[idx] && call.ID != "" && call.ID == resp.ID {
							if resp.Name == "" || call.Name == "" || call.Name == resp.Name {
								matchedIdx = idx
								break
							}
						}
					}
				}

				// 2nd pass: match by Name if not matched by ID
				if matchedIdx == -1 {
					for idx, call := range availableCalls {
						if !usedCalls[idx] && call.Name == resp.Name {
							matchedIdx = idx
							break
						}
					}
				}

				if matchedIdx == -1 {
					// No matching call found -> dangling response, discard it!
					continue
				}

				usedCalls[matchedIdx] = true
				matchedCall := availableCalls[matchedIdx]

				// Ensure response has the call's ID (or call has response's ID) so wire tool_call_id matches
				if resp.ID == "" && matchedCall.ID != "" {
					resp.ID = matchedCall.ID
				} else if resp.ID != "" && matchedCall.ID == "" {
					matchedCall.ID = resp.ID
				}

				validParts = append(validParts, p)
				continue
			}

			// Non-FunctionResponse parts (text, images, FunctionCalls in model turns) are kept
			validParts = append(validParts, p)
		}

		// If all parts in this turn were stripped, drop the turn entirely
		if len(validParts) == 0 {
			continue
		}

		// If dropping an earlier turn caused two user turns to become adjacent, merge them
		if n := len(cleaned); n > 0 && cleaned[n-1].Role == "user" && turn.Role == "user" {
			combined := make([]*genai.Part, 0, len(cleaned[n-1].Parts)+len(validParts))
			combined = append(combined, cleaned[n-1].Parts...)
			combined = append(combined, validParts...)
			cleaned[n-1] = &genai.Content{Role: "user", Parts: combined}
			continue
		}

		cleaned = append(cleaned, &genai.Content{
			Role:  turn.Role,
			Parts: validParts,
		})
	}

	return cleaned
}

// MergeConsecutiveUserTurns is a backwards-compatible alias for CleanSessionTurns.
func MergeConsecutiveUserTurns(contents []*genai.Content) []*genai.Content {
	return CleanSessionTurns(contents)
}
