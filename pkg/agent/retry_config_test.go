package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// TestRuntimeConfig_MaxRetriesParsing verifies the runtime.json field
// round-trips and that pointer semantics distinguish 0 from unset.
func TestRuntimeConfig_MaxRetriesParsing(t *testing.T) {
	t.Run("unset yields nil", func(t *testing.T) {
		var cfg RuntimeConfig
		if cfg.MaxRetries != nil {
			t.Fatalf("expected nil MaxRetries when unset, got %d", *cfg.MaxRetries)
		}
	})

	t.Run("zero parses as pointer to 0", func(t *testing.T) {
		data := []byte(`{"endpoint":"http://x/v1","model":"m","maxRetries":0}`)
		var cfg RuntimeConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}
		if cfg.MaxRetries == nil {
			t.Fatalf("expected non-nil MaxRetries pointer")
		}
		if *cfg.MaxRetries != 0 {
			t.Errorf("expected 0, got %d", *cfg.MaxRetries)
		}
	})

	t.Run("negative one parses as pointer to -1", func(t *testing.T) {
		data := []byte(`{"endpoint":"http://x/v1","model":"m","maxRetries":-1}`)
		var cfg RuntimeConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}
		if cfg.MaxRetries == nil || *cfg.MaxRetries != -1 {
			t.Fatalf("expected pointer to -1, got %+v", cfg.MaxRetries)
		}
	})

	t.Run("loads from runtime.json file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "runtime.json")
		if err := os.WriteFile(path, []byte(`{"endpoint":"http://x/v1","model":"m","maxRetries":3}`), 0644); err != nil {
			t.Fatalf("write runtime.json: %v", err)
		}
		cfg, err := LoadRuntimeConfig(dir)
		if err != nil {
			t.Fatalf("LoadRuntimeConfig failed: %v", err)
		}
		if cfg.MaxRetries == nil || *cfg.MaxRetries != 3 {
			t.Errorf("expected maxRetries 3, got %+v", cfg.MaxRetries)
		}
	})
}

type retryBackend struct {
	calls int32
}

func (b *retryBackend) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&b.calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"message":"slow backend"}}`)
	}
}

// countRequests drives one generation against m on srv and returns how many
// HTTP requests reached the server (retries included, per openai-go).
func countRequests(t *testing.T, srv *httptest.Server, m model.LLM) {
	t.Helper()
	req := &model.LLMRequest{Model: m.Name(), Contents: []*genai.Content{genai.NewContentFromText("hi", "user")}}
	for _, err := range m.GenerateContent(context.Background(), req, false) {
		_ = err // errors here are expected when the backend 503s; we count attempts, not success
	}
}

// TestNewOpenAIModel_MaxRetriesZeroNoRetry: with maxRetries 0 a failing
// backend receives exactly ONE request - no silent retry storm.
func TestNewOpenAIModel_MaxRetriesZeroNoRetry(t *testing.T) {
	b := &retryBackend{}
	srv := httptest.NewServer(b.handler())
	defer srv.Close()

	zero := 0
	m := NewOpenAIModel(&RuntimeConfig{
		Model:          "test-model",
		Endpoint:       srv.URL,
		MaxRetries:     &zero,
		TimeoutSeconds: 5,
	})

	countRequests(t, srv, m)

	if got := atomic.LoadInt32(&b.calls); got != 1 {
		t.Errorf("expected exactly 1 request with maxRetries=0, got %d", got)
	}
}

// TestNewOpenAIModel_MaxRetriesNegativeClampedToZero is the same behavior with
// -1 (explicit disable): one attempt, no retry storm.
func TestNewOpenAIModel_MaxRetriesNegativeClampedToZero(t *testing.T) {
	b := &retryBackend{}
	srv := httptest.NewServer(b.handler())
	defer srv.Close()

	minusOne := -1
	m := NewOpenAIModel(&RuntimeConfig{
		Model:          "test-model",
		Endpoint:       srv.URL,
		MaxRetries:     &minusOne,
		TimeoutSeconds: 5,
	})

	countRequests(t, srv, m)

	if got := atomic.LoadInt32(&b.calls); got != 1 {
		t.Errorf("expected exactly 1 request with maxRetries=-1, got %d", got)
	}
}

// TestNewOpenAIModel_MaxRetriesUnsetKeepsDefaultRetries pins the default
// contract: with maxRetries unset, openai-go retries a 503 (default 2 retries
// = 3 total attempts). This is the "no behavior change for existing
// runtime.json" guarantee.
func TestNewOpenAIModel_MaxRetriesUnsetKeepsDefaultRetries(t *testing.T) {
	b := &retryBackend{}
	srv := httptest.NewServer(b.handler())
	defer srv.Close()

	m := NewOpenAIModel(&RuntimeConfig{
		Model:          "test-model",
		Endpoint:       srv.URL,
		TimeoutSeconds: 5,
	})

	countRequests(t, srv, m)

	if got := atomic.LoadInt32(&b.calls); got != 3 {
		t.Errorf("expected 3 total attempts (1 + 2 default retries) with maxRetries unset, got %d", got)
	}
}
