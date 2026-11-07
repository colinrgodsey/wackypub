package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Adjacent search cases: multi-entry, unicode, deleted entries.

func TestScratchpad_SearchAdjacentCases(t *testing.T) {
	agentDir := t.TempDir()

	t.Run("multi-entry isolates queries per entry", func(t *testing.T) {
		e1, err := CreateScratchpad(agentDir, "alpha beta\ngamma delta\n", "case")
		if err != nil {
			t.Fatal(err)
		}
		e2, err := CreateScratchpad(agentDir, "epsilon zeta\ntheta iota\n", "case")
		if err != nil {
			t.Fatal(err)
		}

		r1, err := SearchScratchpad(agentDir, e1.ID, "beta", nil, false, 50)
		if err != nil {
			t.Fatal(err)
		}
		if r1.TotalMatches != 1 {
			t.Fatalf("e1 'beta': want 1 got %d", r1.TotalMatches)
		}
		r2, err := SearchScratchpad(agentDir, e2.ID, "beta", nil, false, 50)
		if err != nil {
			t.Fatal(err)
		}
		if r2.TotalMatches != 0 {
			t.Fatalf("e2 'beta': want 0 got %d", r2.TotalMatches)
		}
		r1b, err := SearchScratchpad(agentDir, e1.ID, "zeta", nil, false, 50)
		if err != nil {
			t.Fatal(err)
		}
		if r1b.TotalMatches != 0 {
			t.Fatalf("e1 'zeta': want 0 got %d", r1b.TotalMatches)
		}
	})

	t.Run("unicode content matches", func(t *testing.T) {
		text := "héllo wörld\n日本語テキスト\nemoji 🚀 rocket\n"
		e, err := CreateScratchpad(agentDir, text, "case")
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{"héllo", "日本語", "🚀"} {
			r, err := SearchScratchpad(agentDir, e.ID, q, nil, false, 50)
			if err != nil {
				t.Fatal(err)
			}
			if r.TotalMatches != 1 {
				t.Fatalf("query %q on unicode entry: want 1 got %d", q, r.TotalMatches)
			}
		}
	})

	t.Run("deleted entry is not searchable", func(t *testing.T) {
		e, err := CreateScratchpad(agentDir, "to be deleted\n", "case")
		if err != nil {
			t.Fatal(err)
		}
		if err := DeleteScratchpad(agentDir, e.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := SearchScratchpad(agentDir, e.ID, "deleted", nil, false, 50); err == nil {
			t.Fatalf("expected error searching deleted entry")
		}
	})

	t.Run("case_insensitive finds different case", func(t *testing.T) {
		e, err := CreateScratchpad(agentDir, "MiXeD CaSe NeEdLe\n", "case")
		if err != nil {
			t.Fatal(err)
		}
		f := false
		r, err := SearchScratchpad(agentDir, e.ID, "mixed case needle", &f, false, 50)
		if err != nil {
			t.Fatal(err)
		}
		if r.TotalMatches != 1 {
			t.Fatalf("case-insensitive: want 1 got %d", r.TotalMatches)
		}
		// default (nil) stays case-sensitive
		rSens, err := SearchScratchpad(agentDir, e.ID, "mixed case needle", nil, false, 50)
		if err != nil {
			t.Fatal(err)
		}
		if rSens.TotalMatches != 0 {
			t.Fatalf("case-sensitive default: want 0 got %d", rSens.TotalMatches)
		}
	})

	t.Run("needs account for scratchpad dir with old files", func(t *testing.T) {
		spDir := filepath.Join(agentDir, ScratchpadDirName)
		if err := os.MkdirAll(spDir, 0755); err != nil {
			t.Fatal(err)
		}
		// A stale global file matching no valid id format must not break glob.
		if err := os.WriteFile(filepath.Join(spDir, "stray-file.txt"), []byte("abc"), 0644); err != nil {
			t.Fatal(err)
		}
		e, err := CreateScratchpad(agentDir, "unique content here\n", "case")
		if err != nil {
			t.Fatal(err)
		}
		r, err := SearchScratchpad(agentDir, e.ID, "unique content", nil, false, 50)
		if err != nil {
			t.Fatal(err)
		}
		if r.TotalMatches != 1 {
			t.Fatalf("with stray file: want 1 got %d", r.TotalMatches)
		}
		_ = strings.TrimSpace
	})
}
