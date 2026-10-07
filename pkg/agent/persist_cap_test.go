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

	"google.golang.org/genai"
)

// TestPersistTurn_40MiBImageRoundTrip is the acceptance case for the cap raise: a
// 40MiB queued image survives persist -> read byte-identical, and seq recovery
// still works with a 53MiB final line in the log.
func TestPersistTurn_40MiBImageRoundTrip(t *testing.T) {
	agentDir := t.TempDir()

	imageData := make([]byte, 40*1024*1024)
	for i := range imageData {
		imageData[i] = byte(i % 251)
	}
	content := &genai.Content{
		Role: "user",
		Parts: []*genai.Part{
			{Text: "<IMAGE>The following image is stored in scratchpad 'rt40'</IMAGE>"},
			{InlineData: &genai.Blob{MIMEType: "image/jpeg", Data: imageData}},
		},
	}

	seq, report, err := AppendSessionContentGetSeq(agentDir, content)
	if err != nil {
		t.Fatalf("AppendSessionContentGetSeq: %v", err)
	}
	if report.Dropped() || report.TruncatedBytes != 0 {
		t.Fatalf("40MiB image must not be dropped: %+v", report)
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}
	parts := turns[0].Parts
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if !bytes.Equal(parts[1].InlineData.Data, imageData) {
		t.Fatalf("image round-trip not byte-identical: got %d bytes, want %d", len(parts[1].InlineData.Data), len(imageData))
	}

	// The persisted line is ~53MiB (base64 of 40MiB) and must fit the cap.
	raw, err := os.ReadFile(filepath.Join(agentDir, SessionFileName))
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	if len(raw) > MaxPersistTurnBytes+1 {
		t.Errorf("persisted line %d exceeds cap %d", len(raw), MaxPersistTurnBytes)
	}

	// Seq recovery with a 53MiB final line: allocation must continue, not restart.
	if got, err := CurrentSeq(agentDir); err != nil || got != seq {
		t.Errorf("CurrentSeq after 53MiB final line: got %d (%v), want %d", got, err, seq)
	}
	if got, err := NextSeq(agentDir); err != nil || got != seq+1 {
		t.Errorf("NextSeq after 53MiB final line: got %d (%v), want %d", got, err, seq+1)
	}
}

// TestPersistTurn_OverCapDropIsReported pins the loud-drop contract at the marshal
// boundary: a turn whose marshaled form exceeds MaxPersistTurnBytes loses its
// non-text parts, keeps the first text part plus the banner, and reports the drop
// with the exact byte count.
func TestPersistTurn_OverCapDropIsReported(t *testing.T) {
	agentDir := t.TempDir()

	// MaxPersistTurnBytes of raw image data marshals to ~1.33x the cap - guaranteed overflow.
	imageData := make([]byte, MaxPersistTurnBytes)
	content := &genai.Content{
		Role: "user",
		Parts: []*genai.Part{
			{Text: "keep me"},
			{InlineData: &genai.Blob{MIMEType: "image/jpeg", Data: imageData}},
		},
	}

	report, err := AppendSessionContent(agentDir, content)
	if err != nil {
		t.Fatalf("AppendSessionContent: %v", err)
	}
	if report.DroppedParts != 1 {
		t.Errorf("expected DroppedParts 1, got %d", report.DroppedParts)
	}
	if report.DroppedBytes != int64(len(imageData)) {
		t.Errorf("expected DroppedBytes %d, got %d", len(imageData), report.DroppedBytes)
	}

	raw, err := os.ReadFile(filepath.Join(agentDir, SessionFileName))
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	if len(raw) > MaxPersistTurnBytes+1 {
		t.Errorf("persisted line %d exceeds cap %d - the hard invariant is broken", len(raw), MaxPersistTurnBytes)
	}
	if !strings.Contains(string(raw), "remaining content dropped") {
		t.Errorf("expected drop banner in persisted line")
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns: %v", err)
	}
	if len(turns) != 1 || len(turns[0].Parts) != 2 {
		t.Fatalf("expected 1 turn with 2 parts, got %+v", turns)
	}
	if turns[0].Parts[0].Text != "keep me" {
		t.Errorf("first text part not preserved: %q", turns[0].Parts[0].Text)
	}
	if !strings.Contains(turns[0].Parts[1].Text, "exceeded") {
		t.Errorf("second part should be the drop banner, got %q", turns[0].Parts[1].Text)
	}
}

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

// TestAppendDeferredImages_DropIsLoud is the end-to-end form of the silent-drop
// fix: a queued scratchpad image that overflows the persist cap must surface as an
// explicit <IMAGE_ERROR> turn the model can see (plus the banner in the image
// turn), not as a silent absence. A small image of the same shape must persist
// intact with no error turn.
func TestAppendDeferredImages_DropIsLoud(t *testing.T) {
	agentDir := t.TempDir()
	spDir := filepath.Join(agentDir, ScratchpadDirName)
	if err := os.MkdirAll(spDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Large: 9216x9216 noise re-encodes past the persist cap (fixture verifies its
	// own premise so an encoder change fails the test instead of the fixture).
	large := encodeNoiseJPEG(t, 9216)
	if int64(len(large))*4/3 < MaxPersistTurnBytes {
		t.Fatalf("fixture: re-encoded noise %d bytes (base64 %d) does not exceed cap %d", len(large), len(large)*4/3, MaxPersistTurnBytes)
	}
	if err := os.WriteFile(filepath.Join(spDir, "bigspima-0-agent.dat"), large, 0644); err != nil {
		t.Fatal(err)
	}
	small := encodeNoiseJPEG(t, 32)
	if err := os.WriteFile(filepath.Join(spDir, "smllspim-0-agent.dat"), small, 0644); err != nil {
		t.Fatal(err)
	}

	fa := &FolderAgent{
		AgentDir:      agentDir,
		AgentID:       "imgagent",
		RuntimeConfig: &RuntimeConfig{MaxImageDimension: 100000},
	}

	// Plain tmp dir is not a git workspace, so CommitWorkspaceEvent no-ops.
	valid := fa.appendDeferredImages("", []string{"bigspima", "smllspim"})
	if valid != 2 {
		t.Fatalf("expected 2 valid image turns, got %d", valid)
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns: %v", err)
	}
	// Expected shape: [big image turn (banner), big IMAGE_ERROR turn, small image turn]
	if len(turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(turns))
	}
	if !strings.Contains(turns[0].Parts[len(turns[0].Parts)-1].Text, "remaining content dropped") {
		t.Errorf("big image turn should carry the drop banner, got %q", turns[0].Parts[len(turns[0].Parts)-1].Text)
	}
	if len(turns[1].Parts) != 1 || !strings.Contains(turns[1].Parts[0].Text, "<IMAGE_ERROR>") || !strings.Contains(turns[1].Parts[0].Text, "bigspima") {
		t.Errorf("expected an explicit IMAGE_ERROR turn for bigspima, got %+v", turns[1].Parts)
	}
	smallPart := turns[2].Parts
	if len(smallPart) != 2 || smallPart[1].InlineData == nil {
		t.Errorf("small image turn should persist its inline image, got %+v", smallPart)
	} else {
		// The persist layer is byte-faithful to what the normalizer produced (the
		// round-trip fidelity itself is pinned by TestPersistTurn_40MiBImageRoundTrip);
		// the decode check confirms the persisted bytes are the intact 32x32 image.
		decoded, _, derr := image.Decode(bytes.NewReader(smallPart[1].InlineData.Data))
		if derr != nil || decoded.Bounds().Dx() != 32 || decoded.Bounds().Dy() != 32 {
			t.Errorf("persisted small image should decode to 32x32, got %v (%v)", decoded, derr)
		}
	}
	_ = small
	if strings.Contains(turns[2].Parts[0].Text, "IMAGE_ERROR") {
		t.Errorf("small image must not produce an error turn")
	}
}
