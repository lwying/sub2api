package handler

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRequestAuditProtocolFieldsAllowlistedShapeOnly(t *testing.T) {
	const canary = "PRIVATE_CANARY_tool_name_arguments_unknown_field"
	body := []byte(`{
		"model":"` + canary + `",
		"messages":[{"role":"user","content":"` + canary + `"}],
		"input":"` + canary + `",
		"tools":[{"name":"` + canary + `","description":"` + canary + `","input_schema":{"properties":{"` + canary + `":{"type":"string"}}}}],
		"stream":false,
		"thinking":{"type":"disabled","budget_tokens":123,"` + canary + `":"` + canary + `"},
		"` + canary + `":"` + canary + `"
	}`)

	got := requestAuditProtocolFields(body)
	require.NotNil(t, got.Stream)
	require.False(t, *got.Stream, "explicit false must remain distinguishable from an absent stream field")
	require.Equal(t, "disabled", got.ThinkingType)
	require.Equal(t, []string{"model", "messages", "input", "tools", "stream", "thinking"}, got.PresentFields)
	require.Empty(t, got.NormalizedFields, "the body alone cannot prove a normalization occurred")

	serialized, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(serialized), canary, "untrusted field and tool data must never enter audit metadata")

	// Keep this fixture explicitly bound to the service contract consumed by callers.
	metadata := service.RequestAuditMetadata{ProtocolFields: &got}
	metadataJSON, err := json.Marshal(metadata)
	require.NoError(t, err)
	require.NotContains(t, string(metadataJSON), canary)
}

func TestRequestAuditProtocolFieldsPreserveAbsentAndRejectUntrustedEnums(t *testing.T) {
	t.Run("absent values stay absent", func(t *testing.T) {
		got := requestAuditProtocolFields([]byte(`{"model":"gpt-test","messages":[]}`))
		require.Nil(t, got.Stream)
		require.Empty(t, got.ThinkingType)
		require.Equal(t, []string{"model", "messages"}, got.PresentFields)
	})

	t.Run("non-boolean stream is not recorded", func(t *testing.T) {
		got := requestAuditProtocolFields([]byte(`{"stream":"false","thinking":{"type":"enabled"}}`))
		require.Nil(t, got.Stream)
		require.Equal(t, "enabled", got.ThinkingType)
		require.Equal(t, []string{"stream", "thinking"}, got.PresentFields)
	})

	t.Run("unknown thinking enum is omitted", func(t *testing.T) {
		got := requestAuditProtocolFields([]byte(`{"thinking":{"type":"adaptive-canary"}}`))
		require.Empty(t, got.ThinkingType)
		require.Equal(t, []string{"thinking"}, got.PresentFields)
	})
}

func TestRequestAuditProtocolFieldsIgnoreInvalidOrNonObjectJSON(t *testing.T) {
	for _, body := range [][]byte{
		nil,
		[]byte(`{"model":"private-canary"`),
		[]byte(`"private-canary"`),
		[]byte(`[ {"model":"private-canary"} ]`),
	} {
		t.Run(string(body), func(t *testing.T) {
			got := requestAuditProtocolFields(body)
			require.Nil(t, got.Stream)
			require.Empty(t, got.ThinkingType)
			require.Empty(t, got.PresentFields)
			require.Empty(t, got.NormalizedFields)
		})
	}
}
