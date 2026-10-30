package agent

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// heavyPayloadTag marks fixtures that are large on purpose. A fixture that overflows the 64MiB
// persist cap cannot be small, and under -race the test binary peaks at 2.4-2.5GB doing it, which
// OOM-killed the 4GB Pi twice (bugs/wackypub/race-suite-oom-pi). Those fixtures live in files
// carrying this build tag, which the default gates run excludes and CI runs explicitly.
const heavyPayloadTag = "heavy_payload"

// defaultBuildFixtureBudgetMiB is the ceiling for a single fixture in an untagged test file.
// Measured on the race-instrumented package: every test that keeps to it peaks at roughly 100MB,
// and one 9216x9216 image fixture peaks at 2755MB.
const defaultBuildFixtureBudgetMiB = 8

// Fixtures are found by the shapes that actually allocate, rather than by test names, so renaming
// a test cannot hide a big allocation. Built from fragments so this file does not match itself.
var fixtureShapes = []struct {
	why    string
	re     *regexp.Regexp
	sizeMi func(sub []string) int
}{
	{
		why:    "a multi-megabyte byte slice literal",
		re:     regexp.MustCompile("make\\(\\[\\]byte, *([0-9]+) *\\* *1024 *\\* *1024"),
		sizeMi: func(m []string) int { n, _ := strconv.Atoi(m[1]); return n },
	},
	{
		why:    "a slice sized from the persist cap, which is 64MiB",
		re:     regexp.MustCompile("make\\(\\[\\]byte, *MaxPersistTurnBytes"),
		sizeMi: func([]string) int { return 64 },
	},
	{
		why: "a raster whose area is the allocation (4 bytes per pixel, and the pipeline decodes and re-encodes it)",
		re:  regexp.MustCompile("image\\.NewRGBA\\(image\\.Rect\\(0, 0, ([0-9]+), *([0-9]+)\\)\\)"),
		sizeMi: func(m []string) int {
			w, _ := strconv.Atoi(m[1])
			h, _ := strconv.Atoi(m[2])
			return w * h * 4 / (1024 * 1024)
		},
	},
	{
		why:    "a rendered noise fixture, whose cost scales with pixel count",
		re:     regexp.MustCompile("encodeNoiseJPEG\\(t, *([0-9]+)\\)"),
		sizeMi: func(m []string) int { n, _ := strconv.Atoi(m[1]); return n * n * 4 / (1024 * 1024) },
	},
}

// TestHeavyFixturesStayBehindTheHeavyPayloadTag keeps the default gates run inside a 4GB host's
// headroom by failing the run when a fixture over the budget appears in an untagged test file.
func TestHeavyFixturesStayBehindTheHeavyPayloadTag(t *testing.T) {
	const self = "test_memory_budget_test.go"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	tested := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") || name == self {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(raw)
		if strings.Contains(text, "//go:build "+heavyPayloadTag) {
			continue
		}
		tested++
		for lineNo, line := range strings.Split(text, "\n") {
			for _, shape := range fixtureShapes {
				m := shape.re.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				if size := shape.sizeMi(m); size > defaultBuildFixtureBudgetMiB {
					t.Errorf("%s:%d builds an ~%dMiB fixture (%s), over the %dMiB budget for the default build. Move it into a //go:build %s file and add it to the heavy step in .github/workflows/ci.yml.",
						name, lineNo+1, size, shape.why, defaultBuildFixtureBudgetMiB, heavyPayloadTag)
				}
			}
		}
	}
	if tested < 50 {
		t.Fatalf("only %d untagged test files scanned, so this check is not really watching the package", tested)
	}
}

// TestHeavyFixturesAreRunByCI closes the loop the build tag opens: a fixture moved out of the
// default run is only honest if something still runs it. Every Test function defined in a
// heavy-tagged file must be named in the workflow, so moving a test cannot silently drop its
// coverage.
func TestHeavyFixturesAreRunByCI(t *testing.T) {
	workflow := "../../" + ".github/workflows/ci.yml"
	raw, err := os.ReadFile(workflow)
	if err != nil {
		t.Fatalf("read %s: %v", workflow, err)
	}
	ci := string(raw)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		text, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(text), "//go:build "+heavyPayloadTag) {
			continue
		}
		for _, line := range strings.Split(string(text), "\n") {
			if !strings.HasPrefix(line, "func Test") {
				continue
			}
			fn := strings.TrimPrefix(line, "func ")
			open := strings.Index(fn, "(")
			if open < 0 {
				continue
			}
			fn = fn[:open]
			found++
			if !strings.Contains(ci, fn) {
				t.Errorf("%s defines %s for the heavy build, but no CI step names it: moving it behind the tag dropped its coverage", name, fn)
			}
		}
	}
	if found < 9 {
		t.Fatalf("expected at least %d heavy tests, found %d, so this check is not watching the set", 9, found)
	}
}

// TestNoScannerBoundFixtureOutsideTheHeavyBuild guards the second scale class, which has no
// allocation shape to match: reaching the scanner bound means using testScannerOverflowLine, a
// 129MiB line, so any reference to it belongs in a heavy-tagged file.
func TestNoScannerBoundFixtureOutsideTheHeavyBuild(t *testing.T) {
	const self = "test_memory_budget_test.go"
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") || name == self {
			continue
		}
		text, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(text), "//go:build "+heavyPayloadTag) {
			continue
		}
		for lineNo, line := range strings.Split(string(text), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, "testS"+"cannerOverflowLine") && !strings.Contains(line, "const ") {
				t.Errorf("%s:%d reaches the scanner bound (%s), which allocates a 129MiB line; move it into a //go:build %s file and name it in the CI heavy step",
					name, lineNo+1, strings.TrimSpace(line), heavyPayloadTag)
			}
		}
	}
}
