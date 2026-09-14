package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	Agent2AgentEnvVar = "AGENT2AGENT"
	// CorrelationIDMetadataKey is the A2AMetadata.Metadata key for a per-request
	// correlation ID (D103). Distinct from the chain-level TraceID: a detached
	// dispatch mints its own correlation ID so logs, session records, and git
	// trailers for one request are traceable without conflating chains.
	CorrelationIDMetadataKey = "correlation_id"
)

// A2AMetadata defines the minified Agent2Agent context payload passed between agent calls via AGENT2AGENT env var according to D33.
type A2AMetadata struct {
	CallerID  string            `json:"caller_id,omitempty"`
	CallChain []string          `json:"call_chain,omitempty"`
	TraceID   string            `json:"trace_id,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// GenerateTraceID generates a random correlation trace ID with "a2a-" prefix.
func GenerateTraceID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "a2a-default"
	}
	return "a2a-" + hex.EncodeToString(b)
}

// ParseA2AMetadata parses the AGENT2AGENT environment variable if present.
// If AGENT2AGENT is empty or absent, it falls back to parsing legacy WACKYPUB_CALL_CHAIN CSV string.
func ParseA2AMetadata() (*A2AMetadata, error) {
	raw := strings.TrimSpace(os.Getenv(Agent2AgentEnvVar))
	if raw != "" {
		var meta A2AMetadata
		if err := json.Unmarshal([]byte(raw), &meta); err == nil {
			if meta.Metadata == nil {
				meta.Metadata = make(map[string]string)
			}
			return &meta, nil
		}
		return nil, fmt.Errorf("malformed %s env var: %s", Agent2AgentEnvVar, raw)
	}

	// Fallback to legacy WACKYPUB_CALL_CHAIN
	chainStr := strings.TrimSpace(os.Getenv(CallChainEnvVar))
	var chain []string
	if chainStr != "" {
		for _, rawID := range strings.Split(chainStr, ",") {
			id := strings.TrimSpace(rawID)
			if id != "" {
				chain = append(chain, id)
			}
		}
	}

	callerID := ""
	if len(chain) > 0 {
		callerID = chain[len(chain)-1]
	}

	return &A2AMetadata{
		CallerID:  callerID,
		CallChain: chain,
		TraceID:   GenerateTraceID(),
		Metadata:  make(map[string]string),
	}, nil
}

// Encode serializes A2AMetadata into a minified (dense) JSON string.
func (m *A2AMetadata) Encode() (string, error) {
	if m == nil {
		return "", nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
