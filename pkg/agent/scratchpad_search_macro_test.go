package agent

import (
	"fmt"
	"strings"
	"testing"
)

// The "freshly-created invisible to search" report may stem from create-time
// macro expansion mangling the stored body (the D90/D115 escape-literal class):
// if a model stores code or text containing <SCRATCHPAD_DATA ...> lookalikes,
// CreateScratchpad expands/escapes them, so the bytes on disk no longer contain
// the original needle and search returns zero.
func TestScratchpad_SearchAfterMacroLookalikeCreate(t *testing.T) {
	agentDir := t.TempDir()

	cases := []struct {
		name   string
		create string
		search string
	}{
		{
			name:   "plain needle survives",
			create: "before\nMY_UNIQUE_NEEDLE_123\nafter\n",
			search: "MY_UNIQUE_NEEDLE_123",
		},
		{
			name:   "angle bracket needle survives",
			create: "before\n<MY_UNIQUE_NEEDLE_123>\nafter\n",
			search: "MY_UNIQUE_NEEDLE_123",
		},
		{
			name:   "macro-lookalike id is preserved as literal",
			create: "before\n<SCRATCHPAD_DATA id=\"MY_UNIQUE_NEEDLE_123\" />\nafter\n",
			search: "MY_UNIQUE_NEEDLE_123",
		},
		{
			name:   "double-token escape form survives",
			create: "before\n<SCRATCHPAD_DATA id=\"MY_UNIQUE_NEEDLE_123\" />\nafter\n",
			search: "MY_UNIQUE_NEEDLE_123",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, err := CreateScratchpad(agentDir, tc.create, "test")
			if err != nil {
				t.Fatal(err)
			}
			res, err := SearchScratchpad(agentDir, entry.ID, tc.search, nil, false, 50)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("entry text on disk: %q", entry.Text)
			if res.TotalMatches != 1 {
				t.Fatalf("FALSE ZERO: search %q on created body %q -> total=%d (matches=%d)",
					tc.search, entry.Text, res.TotalMatches, len(res.Matches))
			}
		})
	}
	_ = fmt.Sprintf
	_ = strings.Contains
}
