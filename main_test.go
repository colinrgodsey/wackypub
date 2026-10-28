package main

import (
	"os"
	"strings"
	"testing"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

// bundledSources maps each embedded bundle to the file it must match byte for byte. This is
// the guard for the one wiring mistake that is invisible at compile time: editing the file
// under examples/ while the binary keeps shipping something else. The embed and the
// assignment both live in this file, so nothing else can drift between the two.
var bundledSources = []struct {
	name   string
	loaded string
	path   string
}{
	{"compact append", bundledDefaultCompactMD, "examples/compaction/COMPACT-append.md"},
	{"compact hybrid", bundledDefaultCompactMDHybrid, "examples/compaction/COMPACT-hybrid.md"},
	{"default runtime", bundledDefaultRuntimeJSON, "examples/runtimes/openrouter-auto.json"},
}

func TestBundlesMatchTheirSourceFiles(t *testing.T) {
	for _, b := range bundledSources {
		want, err := os.ReadFile(b.path)
		if err != nil {
			t.Fatalf("%s: read %s: %v", b.name, b.path, err)
		}
		if len(b.loaded) == 0 {
			t.Fatalf("%s: embedded bundle is empty, so the go:embed directive is broken or repointed", b.name)
		}
		if b.loaded != string(want) {
			t.Errorf("%s: the shipped bytes differ from %s (%d embedded vs %d on disk); editing that file is the only way to change this default, so a mismatch means the embed no longer reads it",
				b.name, b.path, len(b.loaded), len(want))
		}
	}
}

func TestApplyBundledDefaultsWiresPkgAgent(t *testing.T) {
	for _, pair := range []struct {
		name string
		got  *string
		want string
	}{
		{"DefaultCompactMD", &adkAgent.DefaultCompactMD, bundledDefaultCompactMD},
		{"DefaultCompactMDHybrid", &adkAgent.DefaultCompactMDHybrid, bundledDefaultCompactMDHybrid},
		{"DefaultRuntimeJSON", &adkAgent.DefaultRuntimeJSON, bundledDefaultRuntimeJSON},
	} {
		*pair.got = ""
		applyBundledDefaults()
		if *pair.got != pair.want {
			t.Errorf("%s was not wired from the embedded bundle, so an agent with no config file would fall back to %q",
				pair.name, *pair.got)
		}
	}
}

// TestHybridDefaultCarriesTheNoEchoDirective pins the acceptance from the compact-directive
// card into a test: the no-echo wording must live in the default every agent without a
// COMPACT.md actually receives, not only in a file someone might edit.
func TestHybridDefaultCarriesTheNoEchoDirective(t *testing.T) {
	applyBundledDefaults()
	for _, want := range []string{"## Both Passes", "always-loaded files", "Capture conversation STATE"} {
		if !strings.Contains(adkAgent.DefaultCompactMDHybrid, want) {
			t.Errorf("the embedded compaction default is missing %q", want)
		}
	}
	if strings.Contains(adkAgent.DefaultCompactMD, "always-loaded files") {
		t.Error("the append-only variant is meant to stay as shipped in examples/; the directive belongs to the hybrid default")
	}
}
