package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
)

const (
	Agent2AgentEnvVar = "AGENT2AGENT"
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

// ProtoToA2AMetadata converts an agentv1.A2AMetadata protobuf message to an A2AMetadata struct.
func ProtoToA2AMetadata(p *agentv1.A2AMetadata) *A2AMetadata {
	if p == nil {
		return nil
	}
	var chain []string
	if len(p.GetCallChain()) > 0 {
		chain = append([]string{}, p.GetCallChain()...)
	}
	metaMap := make(map[string]string)
	for k, v := range p.GetMetadata() {
		metaMap[k] = v
	}
	return &A2AMetadata{
		CallerID:  p.GetCallerId(),
		CallChain: chain,
		TraceID:   p.GetTraceId(),
		Metadata:  metaMap,
	}
}

// A2AMetadataToProto converts an A2AMetadata struct to an agentv1.A2AMetadata protobuf message.
func A2AMetadataToProto(m *A2AMetadata) *agentv1.A2AMetadata {
	if m == nil {
		return nil
	}
	var chain []string
	if len(m.CallChain) > 0 {
		chain = append([]string{}, m.CallChain...)
	}
	metaMap := make(map[string]string)
	for k, v := range m.Metadata {
		metaMap[k] = v
	}
	return &agentv1.A2AMetadata{
		CallerId:  m.CallerID,
		CallChain: chain,
		TraceId:   m.TraceID,
		Metadata:  metaMap,
	}
}
