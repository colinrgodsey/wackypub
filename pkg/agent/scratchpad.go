package agent

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/h2non/filetype"
)

const (
	ScratchpadDirName         = "scratchpad"
	MaxScratchpadEntries      = 300
	MaxExpandedArgBytes       = 500000
	ScratchpadOutputThreshold = 4000
	MediaDetectionHeaderBytes = 262
	BinaryCheckPrefixBytes    = 24
)

type ScratchpadEntry struct {
	ID        string   `json:"id"`
	Size      int      `json:"size"`
	Lines     int      `json:"lines"`
	CreatedBy string   `json:"created_by"`
	Text      string   `json:"text,omitempty"`
	IsBinary  bool     `json:"is_binary,omitempty"`
	MIMEType  string   `json:"mime_type,omitempty"`
	Warning   string   `json:"warning,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

type ScratchpadItem struct {
	ID        string `json:"id"`
	Size      int    `json:"size"`
	Lines     int    `json:"lines"`
	CreatedBy string `json:"created_by"`
	IsBinary  bool   `json:"is_binary,omitempty"`
	MIMEType  string `json:"mime_type,omitempty"`
}

// ReadMediaHeader reads up to MediaDetectionHeaderBytes from filePath.
// Closes file properly and verifies errors from both read and close without swallowing.
func ReadMediaHeader(filePath string) ([]byte, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open media file: %w", err)
	}
	header := make([]byte, MediaDetectionHeaderBytes)
	n, readErr := f.Read(header)
	closeErr := f.Close()
	if readErr != nil && readErr != io.EOF {
		return nil, fmt.Errorf("failed to read media header: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("failed to close media file: %w", closeErr)
	}
	return header[:n], nil
}

// IsBinaryContent checks the first min(BinaryCheckPrefixBytes, len(data)) bytes for any byte <= 8 (NUL, etc.) according to D48.
func IsBinaryContent(data []byte) bool {
	checkLen := len(data)
	if checkLen > BinaryCheckPrefixBytes {
		checkLen = BinaryCheckPrefixBytes
	}
	for i := 0; i < checkLen; i++ {
		if data[i] <= 8 {
			return true
		}
	}
	return false
}

// DetectMediaType returns whether data is binary and its MIME type using a 2-stage heuristic per D48.
func DetectMediaType(data []byte) (bool, string) {
	if IsBinaryContent(data) {
		kind, err := filetype.Match(data)
		if err == nil && kind != filetype.Unknown {
			return true, kind.MIME.Value
		}
		return true, "application/octet-stream"
	}
	return false, "text/plain"
}

// CountLines counts the number of lines in text according to D39.
// Matches TailFile's convention: splits on \n, drops trailing empty segment if text ends in \n.
func CountLines(text string) int {
	if text == "" {
		return 0
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return len(lines)
}

func generateRandomID() string {
	const charset = "0123456789abcdefghijklmnopqrstuvwxyz"
	var result strings.Builder
	for i := 0; i < 4; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			result.WriteByte(charset[time.Now().UnixNano()%int64(len(charset))])
			continue
		}
		result.WriteByte(charset[n.Int64()])
	}
	return result.String()
}

var scratchpadIDRegex = regexp.MustCompile(`^[0-9a-z]{4}$`)

// validateScratchpadID validates that id conforms to the required 4-character [0-9a-z] token shape per D81.
func validateScratchpadID(id string) error {
	if !scratchpadIDRegex.MatchString(id) {
		return fmt.Errorf("invalid scratchpad entry ID %q: must be exactly 4 lowercase alphanumeric characters ([0-9a-z])", id)
	}
	return nil
}

// listScratchpadMatches resolves all entry files matching id in <agentDir>/scratchpad/ per D81.
func listScratchpadMatches(agentDir string, id string) ([]string, error) {
	if err := validateScratchpadID(id); err != nil {
		return nil, err
	}
	spDir := filepath.Join(agentDir, ScratchpadDirName)
	pattern := filepath.Join(spDir, id+"-*")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return nil, fmt.Errorf("scratchpad entry %q not found", id)
	}
	return matches, nil
}

func findScratchpadFile(agentDir string, id string) (string, string, bool, error) {
	matches, err := listScratchpadMatches(agentDir, id)
	if err != nil {
		return "", "", false, err
	}
	for _, m := range matches {
		base := filepath.Base(m)
		if strings.HasSuffix(base, ".txt") {
			return m, base, false, nil
		}
		if strings.HasSuffix(base, ".dat") {
			return m, base, true, nil
		}
	}
	return "", "", false, fmt.Errorf("scratchpad entry %q not found", id)
}

func parseFilenameParts(baseName string) (id string, lines int, createdBy string, isBinary bool) {
	isBinary = strings.HasSuffix(baseName, ".dat")
	baseNoExt := strings.TrimSuffix(strings.TrimSuffix(baseName, ".txt"), ".dat")
	parts := strings.SplitN(baseNoExt, "-", 3)
	if len(parts) == 3 {
		id = parts[0]
		n, err := strconv.Atoi(parts[1])
		if err == nil {
			lines = n
		}
		createdBy = parts[2]
		return id, lines, createdBy, isBinary
	}
	if len(parts) == 2 {
		id = parts[0]
		createdBy = parts[1]
		return id, 0, createdBy, isBinary
	}
	return baseNoExt, 0, "", isBinary
}

func EvictOldestScratchpad(spDir string, maxCap int) {
	entries, err := os.ReadDir(spDir)
	if err != nil || len(entries) <= maxCap {
		return
	}

	type fileInfo struct {
		name    string
		modTime time.Time
	}
	var files []fileInfo
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".txt") && !strings.HasSuffix(entry.Name(), ".dat")) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{
			name:    entry.Name(),
			modTime: info.ModTime(),
		})
	}

	if len(files) <= maxCap {
		return
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	toRemove := len(files) - maxCap
	for i := 0; i < toRemove; i++ {
		_ = os.Remove(filepath.Join(spDir, files[i].name))
	}
}

// createScratchpadRaw stores text verbatim in <agentDir>/scratchpad/<id>-<lines>-<createdBy>.txt
// without macro expansion per D80. Used internally for machine-captured output (stdout, stderr).
func createScratchpadRaw(agentDir string, text string, createdBy string) (*ScratchpadEntry, error) {
	if createdBy == "" {
		createdBy = "run_command"
	}

	spDir := filepath.Join(agentDir, ScratchpadDirName)
	if err := os.MkdirAll(spDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create scratchpad directory: %w", err)
	}

	lineCount := CountLines(text)
	var id string
	var filePath string
	var file *os.File

	for i := 0; i < 100; i++ {
		candidateID := generateRandomID()
		// Check if any entry (.txt or .dat) already exists with this ID
		if matches, _ := filepath.Glob(filepath.Join(spDir, candidateID+"-*")); len(matches) > 0 {
			continue
		}
		candidatePath := filepath.Join(spDir, fmt.Sprintf("%s-%d-%s.txt", candidateID, lineCount, createdBy))
		f, err := os.OpenFile(candidatePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			id = candidateID
			filePath = candidatePath
			file = f
			break
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("failed to create scratchpad entry file: %w", err)
		}
	}
	if file == nil {
		return nil, fmt.Errorf("failed to generate unique scratchpad ID after retries")
	}

	if _, err := file.WriteString(text); err != nil {
		_ = file.Close()
		_ = os.Remove(filePath)
		return nil, fmt.Errorf("failed to write scratchpad content: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(filePath)
		return nil, fmt.Errorf("failed to close scratchpad file: %w", err)
	}

	EvictOldestScratchpad(spDir, MaxScratchpadEntries)

	return &ScratchpadEntry{
		ID:        id,
		Size:      len(text),
		Lines:     lineCount,
		CreatedBy: createdBy,
		Text:      text,
		IsBinary:  false,
		MIMEType:  "text/plain",
	}, nil
}

// CreateScratchpad creates a new text scratchpad entry in <agentDir>/scratchpad/<id>-<lines>-<createdBy>.txt
// according to D30/D39.
// Automatically expands inline <SCRATCHPAD_DATA id="X" ... /> macros before storing per D90.
// Atomic and collision-safe across separate OS processes via O_CREATE|O_EXCL.
// Automatically evicts the entry with the oldest mtime when live entries exceed cap (300).
func CreateScratchpad(agentDir string, text string, createdBy string) (*ScratchpadEntry, error) {
	expandedText, warnings, err := ExpandScratchpadMacros(agentDir, text)
	if err != nil {
		return nil, fmt.Errorf("failed to expand scratchpad macros: %w", err)
	}

	if createdBy == "" {
		createdBy = "create_scratchpad"
	}

	entry, err := createScratchpadRaw(agentDir, expandedText, createdBy)
	if err != nil {
		return nil, err
	}
	if len(warnings) > 0 {
		entry.Warnings = warnings
		entry.Warning = strings.Join(warnings, "\n")
	}
	return entry, nil
}

// CreateBinaryScratchpad creates a new binary scratchpad entry in <agentDir>/scratchpad/<id>-0-<createdBy>.dat per D48.
func CreateBinaryScratchpad(agentDir string, data []byte, createdBy string, mimeType string) (*ScratchpadEntry, error) {
	if createdBy == "" {
		createdBy = "run_command"
	}
	if mimeType == "" {
		_, mimeType = DetectMediaType(data)
	}

	spDir := filepath.Join(agentDir, ScratchpadDirName)
	if err := os.MkdirAll(spDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create scratchpad directory: %w", err)
	}

	var id string
	var filePath string
	var file *os.File

	for i := 0; i < 100; i++ {
		candidateID := generateRandomID()
		// Check if any entry (.txt or .dat) already exists with this ID
		if matches, _ := filepath.Glob(filepath.Join(spDir, candidateID+"-*")); len(matches) > 0 {
			continue
		}
		candidatePath := filepath.Join(spDir, fmt.Sprintf("%s-0-%s.dat", candidateID, createdBy))
		f, err := os.OpenFile(candidatePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			id = candidateID
			filePath = candidatePath
			file = f
			break
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("failed to create binary scratchpad entry file: %w", err)
		}
	}
	if file == nil {
		return nil, fmt.Errorf("failed to generate unique scratchpad ID after retries")
	}

	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(filePath)
		return nil, fmt.Errorf("failed to write binary scratchpad content: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(filePath)
		return nil, fmt.Errorf("failed to close binary scratchpad file: %w", err)
	}

	EvictOldestScratchpad(spDir, MaxScratchpadEntries)

	return &ScratchpadEntry{
		ID:        id,
		Size:      len(data),
		Lines:     0,
		CreatedBy: createdBy,
		IsBinary:  true,
		MIMEType:  mimeType,
	}, nil
}

// DeleteScratchpad removes a scratchpad entry (.txt or .dat) by ID per D48/D81.
func DeleteScratchpad(agentDir string, id string) error {
	matches, err := listScratchpadMatches(agentDir, id)
	if err != nil {
		return err
	}
	deleted := false
	for _, m := range matches {
		if strings.HasSuffix(m, ".txt") || strings.HasSuffix(m, ".dat") {
			if err := os.Remove(m); err != nil {
				return fmt.Errorf("failed to delete scratchpad entry %q: %w", id, err)
			}
			deleted = true
		}
	}
	if !deleted {
		return fmt.Errorf("scratchpad entry %q not found", id)
	}
	return nil
}

// MaxScratchpadReadSizeBytes is the fallback maximum byte size for a single scratchpad read (200KB)
// when runtime.json contextWindow is unset or <= 0.
const MaxScratchpadReadSizeBytes = 200 * 1024

// MaxScratchpadContextWindowFraction is the maximum fraction of contextWindow a single scratchpad read can consume (25%).
const MaxScratchpadContextWindowFraction = 0.25

// readScratchpadRaw retrieves raw stored text by entry ID, optionally paginated by line range,
// without enforcing LLM contextWindow output size caps (used internally for zero-token macro expansion).
func readScratchpadRaw(agentDir string, id string, skipLines *int, numLines *int) (string, error) {
	filePath, _, isBinary, err := findScratchpadFile(agentDir, id)
	if err != nil {
		return "", err
	}

	if isBinary {
		mimeType := "application/octet-stream"
		header, err := ReadMediaHeader(filePath)
		if err == nil {
			_, mimeType = DetectMediaType(header)
		}
		return "", fmt.Errorf("scratchpad entry %q is binary data (%s) and cannot be read as text", id, mimeType)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read scratchpad entry %q: %w", id, err)
	}

	lines := strings.Split(string(data), "\n")
	start := 0
	if skipLines != nil && *skipLines > 0 {
		start = *skipLines
		if start > len(lines) {
			start = len(lines)
		}
	}

	end := len(lines)
	if numLines != nil && *numLines >= 0 {
		end = start + *numLines
		if end > len(lines) {
			end = len(lines)
		}
	}

	if start >= len(lines) {
		return "", nil
	}

	return strings.Join(lines[start:end], "\n"), nil
}

// GetScratchpad retrieves stored text by entry ID, optionally paginated by line range.
// Rejects binary (.dat) entries outright per D48.
// Enforces a hard output size cap per D63: refuses output exceeding 25% of runtime.json contextWindow
// (or 200KB fallback when unset), suggesting line-based pagination or search.
func GetScratchpad(agentDir string, id string, skipLines *int, numLines *int) (string, error) {
	out, err := readScratchpadRaw(agentDir, id, skipLines, numLines)
	if err != nil {
		return "", err
	}

	runtimeCfg, _ := LoadRuntimeConfig(agentDir)
	if runtimeCfg != nil && runtimeCfg.ContextWindow > 0 {
		maxTokens := int(float64(runtimeCfg.ContextWindow) * MaxScratchpadContextWindowFraction)
		if maxTokens < 1 {
			maxTokens = 1
		}
		estTokens := len(out) / 4
		if estTokens > maxTokens {
			return "", fmt.Errorf("scratchpad entry %q output is %d bytes (~%d estimated tokens), which exceeds the single-read limit of %d tokens (25%% of %d contextWindow) - use skip_lines and num_lines or search_scratchpad for pagination", id, len(out), estTokens, maxTokens, runtimeCfg.ContextWindow)
		}
	} else {
		if len(out) > MaxScratchpadReadSizeBytes {
			return "", fmt.Errorf("scratchpad entry %q output is %d bytes, which exceeds the %d byte read limit - use skip_lines and num_lines or search_scratchpad for pagination", id, len(out), MaxScratchpadReadSizeBytes)
		}
	}

	return out, nil
}

// ListScratchpads returns metadata items for all live entries in <agentDir>/scratchpad/ ordered by mtime ascending.
func ListScratchpads(agentDir string) ([]ScratchpadItem, int, int, error) {
	spDir := filepath.Join(agentDir, ScratchpadDirName)
	entries, err := os.ReadDir(spDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []ScratchpadItem{}, 0, MaxScratchpadEntries, nil
		}
		return nil, 0, MaxScratchpadEntries, fmt.Errorf("failed to read scratchpad directory: %w", err)
	}

	type itemWithTime struct {
		item    ScratchpadItem
		modTime time.Time
	}

	var list []itemWithTime
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".txt") && !strings.HasSuffix(entry.Name(), ".dat")) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}

		baseName := entry.Name()
		id, lineCount, createdBy, isBinary := parseFilenameParts(baseName)

		mimeType := "text/plain"
		if isBinary {
			mimeType = "application/octet-stream"
			header, err := ReadMediaHeader(filepath.Join(spDir, baseName))
			if err == nil {
				_, mimeType = DetectMediaType(header)
			}
		}

		list = append(list, itemWithTime{
			item: ScratchpadItem{
				ID:        id,
				Size:      int(info.Size()),
				Lines:     lineCount,
				CreatedBy: createdBy,
				IsBinary:  isBinary,
				MIMEType:  mimeType,
			},
			modTime: info.ModTime(),
		})
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].modTime.Before(list[j].modTime)
	})

	items := make([]ScratchpadItem, len(list))
	for i, l := range list {
		items[i] = l.item
	}

	return items, len(items), MaxScratchpadEntries, nil
}

var (
	scratchpadMacroRegex = regexp.MustCompile(`(\\?)<SCRATCHPAD_DATA\s+([^>]+)\s*/?>`)
	macroIDRegex         = regexp.MustCompile(`id=\\?"([^"\\]+)\\?"`)
	macroSkipLinesRegex  = regexp.MustCompile(`skip_lines=\\?"(\d+)\\?"`)
	macroNumLinesRegex   = regexp.MustCompile(`num_lines=\\?"(\d+)\\?"`)
	macroJsonEscapeRegex = regexp.MustCompile(`json_escape=\\?"?(true|1|false|0)\\?"?`)
)

// ExpandScratchpadMacros replaces any inline <SCRATCHPAD_DATA id="X" skip_lines="N" num_lines="M" json_escape="true" /> macros
// in text with the corresponding scratchpad text content according to D18/D28/D30/D37/D90.
// Non-escaped macros whose IDs do not exist pass through literally and emit warnings.
// A leading backslash (\) escapes the macro, rendering it literally without warnings even if the ID exists.
func ExpandScratchpadMacros(agentDir string, text string) (string, []string, error) {
	if !strings.Contains(text, "<SCRATCHPAD_DATA") {
		return text, nil, nil
	}

	var firstErr error
	var warnings []string
	expanded := scratchpadMacroRegex.ReplaceAllStringFunc(text, func(match string) string {
		if firstErr != nil {
			return match
		}

		if strings.HasPrefix(match, "\\") {
			return strings.TrimPrefix(match, "\\")
		}

		idMatch := macroIDRegex.FindStringSubmatch(match)
		if len(idMatch) < 2 {
			firstErr = fmt.Errorf("invalid <SCRATCHPAD_DATA /> macro syntax: missing id attribute")
			return match
		}
		id := idMatch[1]

		var skipLines *int
		if skipMatch := macroSkipLinesRegex.FindStringSubmatch(match); len(skipMatch) >= 2 {
			val, _ := strconv.Atoi(skipMatch[1])
			skipLines = &val
		}

		var numLines *int
		if numMatch := macroNumLinesRegex.FindStringSubmatch(match); len(numMatch) >= 2 {
			val, _ := strconv.Atoi(numMatch[1])
			numLines = &val
		}

		jsonEscape := false
		if jsonMatch := macroJsonEscapeRegex.FindStringSubmatch(match); len(jsonMatch) >= 2 {
			val := strings.ToLower(jsonMatch[1])
			if val == "true" || val == "1" {
				jsonEscape = true
			}
		}

		_, _, isBinary, err := findScratchpadFile(agentDir, id)
		if err != nil {
			warnMsg := fmt.Sprintf("warning: scratchpad entry %q not found; macro passed through literally", id)
			found := false
			for _, w := range warnings {
				if w == warnMsg {
					found = true
					break
				}
			}
			if !found {
				warnings = append(warnings, warnMsg)
			}
			return match
		}

		if isBinary {
			firstErr = fmt.Errorf("scratchpad entry %q is binary data and cannot be expanded as text", id)
			return match
		}

		content, err := readScratchpadRaw(agentDir, id, skipLines, numLines)
		if err != nil {
			firstErr = err
			return match
		}

		if jsonEscape {
			b, err := json.Marshal(content)
			if err == nil && len(b) >= 2 {
				content = string(b[1 : len(b)-1])
			}
		}

		return content
	})

	if firstErr != nil {
		return "", nil, firstErr
	}
	return expanded, warnings, nil
}

type ScratchpadMatch struct {
	Line      int    `json:"line"`
	SkipLines int    `json:"skip_lines"`
	Text      string `json:"text"`
}

type SearchScratchpadResult struct {
	ID           string            `json:"id"`
	Query        string            `json:"query"`
	TotalMatches int               `json:"total_matches"`
	MaxResults   int               `json:"max_results"`
	Matches      []ScratchpadMatch `json:"matches"`
}

// SearchScratchpad searches a specific scratchpad entry (by ID) for query according to D25/D30.
// Rejects binary (.dat) entries outright per D48.
func SearchScratchpad(agentDir string, id string, query string, caseSensitive *bool, useRegex bool, maxResults int) (*SearchScratchpadResult, error) {
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	if query == "" {
		return nil, fmt.Errorf("query must not be empty")
	}
	if maxResults <= 0 {
		maxResults = 50
	}
	isCaseSensitive := true
	if caseSensitive != nil {
		isCaseSensitive = *caseSensitive
	}

	filePath, _, isBinary, err := findScratchpadFile(agentDir, id)
	if err != nil {
		return nil, err
	}

	if isBinary {
		return nil, fmt.Errorf("scratchpad entry %q is binary data and cannot be searched as text", id)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read scratchpad entry %q: %w", id, err)
	}

	lines := strings.Split(string(data), "\n")

	var reg *regexp.Regexp
	if useRegex {
		pattern := query
		if !isCaseSensitive {
			pattern = "(?i)" + pattern
		}
		r, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex query %q: %w", query, err)
		}
		reg = r
	}

	var matches []ScratchpadMatch
	totalMatches := 0

	queryLower := ""
	if !useRegex && !isCaseSensitive {
		queryLower = strings.ToLower(query)
	}

	for i, lineText := range lines {
		matched := false
		if useRegex {
			matched = reg.MatchString(lineText)
		} else {
			if isCaseSensitive {
				matched = strings.Contains(lineText, query)
			} else {
				matched = strings.Contains(strings.ToLower(lineText), queryLower)
			}
		}

		if matched {
			totalMatches++
			if len(matches) < maxResults {
				truncatedText := lineText
				if len(truncatedText) > 200 {
					truncatedText = truncatedText[:200]
				}
				matches = append(matches, ScratchpadMatch{
					Line:      i + 1,
					SkipLines: i,
					Text:      truncatedText,
				})
			}
		}
	}

	if matches == nil {
		matches = []ScratchpadMatch{}
	}

	return &SearchScratchpadResult{
		ID:           id,
		Query:        query,
		TotalMatches: totalMatches,
		MaxResults:   maxResults,
		Matches:      matches,
	}, nil
}
