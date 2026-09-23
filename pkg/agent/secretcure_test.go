package agent

import (
	"strings"
	"testing"
)

// TestSecretCure_CompoundKeysAndJSON pins phoebe's tested cure for the whole-token regression:
// underscore-joined compound secret keys must redact again (auth_token, client_secret,
// refresh_token, oauth_token, id_token, session_token) while the benign keys she verified
// (secret_shares_ratio, password_length_hint, author, tokenizer, message, file_path) stay
// untouched. Also the JSON nested form {"client_secret":"..."} must redact without touching
// {"author":"Colin"}.
func TestSecretCure_CompoundKeysAndJSON(t *testing.T) {
	mustCatch := []string{
		"auth_token", "client_secret", "refresh_token", "oauth_token", "id_token", "session_token",
		"api_key", "apikey", "api-key", "access_token", "bearer_token", "password", "auth", "authorization",
	}
	for _, k := range mustCatch {
		if !isSecretKey(k) {
			t.Errorf("isSecretKey(%q) = false, want true (compound/secret key)", k)
		}
	}
	benign := []string{
		"secret_shares_ratio", "password_length_hint", "author", "authorize", "authority", "tokenizer", "message", "file_path",
	}
	for _, k := range benign {
		if isSecretKey(k) {
			t.Errorf("isSecretKey(%q) = true, want false (benign)", k)
		}
	}

	// Value-level: auth_token=... with a generic token value must redact the pair.
	got := redactSecretValues("auth_token=y2x9k4m7q1w8e3r5t6u9i0o2p4s6d8f1g3h5j7k9l2n4b6v8c1x3z5a7q9w2e4r6t8y0u1i3o5p7a9s2d4f6g8h1j3k5l7m9n1b3v5c7x9z2w4e6r8t0y1u3i5o7p9a2s4d6f8g0h2j4k6l8m1n3b5v7c9x1z")
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("auth_token= value not redacted: %s", got)
	}

	// JSON nested form: quoted compound key + value must redact.
	js := `{"client_secret":"y2x9k4m7q1w8e3r5t6u9i0o2", "ok": true}`
	red := redactSecretValues(js)
	if strings.Contains(red, "y2x9k4m7q1w8e3r5t6u9i0o2") {
		t.Errorf("nested JSON client_secret leaked: %s", red)
	}
	if !strings.Contains(red, "[REDACTED]") {
		t.Errorf("nested JSON client_secret not redacted: %s", red)
	}

	// JSON benign: quoted author must stay.
	js2 := `{"author":"Colin", "message":"hi"}`
	red2 := redactSecretValues(js2)
	if !strings.Contains(red2, "Colin") {
		t.Errorf("JSON author value wrongly redacted: %s", red2)
	}

	// tokenizer=cl100k_base stays (no kv match on 'tokenizer').
	tok := redactSecretValues("tokenizer=cl100k_base")
	if strings.Contains(tok, "[REDACTED]") {
		t.Errorf("tokenizer= wrongly redacted: %s", tok)
	}
}
