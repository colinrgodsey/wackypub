package agent

import (
	"bytes"
	"image"
	"image/jpeg"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The payload-scale persist fixtures live in persist_cap_heavy_test.go behind the heavy_payload
// build tag, because a fixture that overflows the 64MiB persist cap costs gigabytes of RSS once
// race-instrumented. What stays here is everything that can be proven with a small image, so the
// default gates run still exercises the deferred-image happy path on a 4GB host.

// encodeNoiseJPEG renders an N x N white-noise RGBA image to JPEG at the platform
// quality. Noise is the worst case for JPEG: it does not compress, so this is the
// deterministic way to make a re-encoded image of a known, controllable size.
func encodeNoiseJPEG(t *testing.T, n int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	p := rand.New(rand.NewSource(1))
	for i := range img.Pix {
		img.Pix[i] = byte(p.Intn(256))
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: DefaultJPEGQuality}); err != nil {
		t.Fatalf("jpeg encode: %v", err)
	}
	return buf.Bytes()
}

// TestAppendDeferredImages_SmallImagePersistsWithoutErrorTurn is the untagged half of the
// deferred-image contract: an image inside the persist cap must land as an inline image turn with
// no <IMAGE_ERROR> turn beside it. The over-cap half needs a fixture larger than the cap and so
// lives behind heavy_payload.
func TestAppendDeferredImages_SmallImagePersistsWithoutErrorTurn(t *testing.T) {
	agentDir := t.TempDir()
	spDir := filepath.Join(agentDir, ScratchpadDirName)
	if err := os.MkdirAll(spDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spDir, "smllspim-0-agent.dat"), encodeNoiseJPEG(t, 32), 0644); err != nil {
		t.Fatal(err)
	}

	fa := &FolderAgent{
		AgentDir:      agentDir,
		AgentID:       "imgagent",
		RuntimeConfig: &RuntimeConfig{MaxImageDimension: 100000},
	}
	if valid := fa.appendDeferredImages("", []string{"smllspim"}); valid != 1 {
		t.Fatalf("expected 1 valid image turn, got %d", valid)
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn with no error turn beside it, got %d", len(turns))
	}
	parts := turns[0].Parts
	if len(parts) != 2 || parts[1].InlineData == nil {
		t.Fatalf("small image turn should persist its inline image, got %+v", parts)
	}
	decoded, _, derr := image.Decode(bytes.NewReader(parts[1].InlineData.Data))
	if derr != nil || decoded.Bounds().Dx() != 32 || decoded.Bounds().Dy() != 32 {
		t.Errorf("persisted small image should decode to 32x32, got %v (%v)", decoded, derr)
	}
	if strings.Contains(parts[0].Text, "IMAGE_ERROR") {
		t.Errorf("small image must not produce an error turn")
	}
}
