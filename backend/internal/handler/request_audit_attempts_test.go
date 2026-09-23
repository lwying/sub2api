//go:build unit

package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAppendOpenAITransportAttemptsBuildsOneTimeline(t *testing.T) {
	var attempts []service.RequestAuditAttempt
	metadata := []httpattempt.Metadata{
		{AccountID: 11, Model: "claude-sonnet-4-20250514", Protocol: service.RequestAuditProtocolAnthropic},
		{AccountID: 22, Model: "gpt-5.4", Protocol: service.RequestAuditProtocolOpenAIResp},
		{AccountID: 22, Model: "gpt-5.4", Protocol: service.RequestAuditProtocolOpenAIChat},
	}

	attempts = appendOpenAITransportAttempts(
		attempts,
		metadata,
		0,
		"gpt-5.4",
		"claude-sonnet-4",
		nil,
		service.RequestAuditProtocolOpenAIResp,
	)

	require.Equal(t, []service.RequestAuditAttempt{
		{Protocol: service.RequestAuditProtocolOpenAIResp, Stage: service.RequestAuditStageClientEntry},
		{Protocol: service.RequestAuditProtocolOpenAIResp, Stage: service.RequestAuditStagePostNormalize},
		{AccountID: 11, Protocol: service.RequestAuditProtocolAnthropic, Stage: service.RequestAuditStageWire},
		{AccountID: 22, Protocol: service.RequestAuditProtocolOpenAIResp, Stage: service.RequestAuditStageWire},
		{AccountID: 22, Protocol: service.RequestAuditProtocolOpenAIChat, Stage: service.RequestAuditStageWire},
	}, attempts)
}

func TestAppendOpenAITransportAttemptsUsesOnlyMetadataDelta(t *testing.T) {
	attempts := []service.RequestAuditAttempt{{
		Model: "gpt-5.4", Protocol: service.RequestAuditProtocolOpenAIResp, Stage: service.RequestAuditStageClientEntry,
	}}
	metadata := []httpattempt.Metadata{
		{AccountID: 11, Model: "old", Protocol: service.RequestAuditProtocolOpenAIResp},
		{AccountID: 22, Model: "new", Protocol: service.RequestAuditProtocolOpenAIChat},
	}

	got := appendOpenAITransportAttempts(
		attempts,
		metadata,
		1,
		"ignored-client",
		"ignored-post-normalize",
		nil,
		service.RequestAuditProtocolOpenAIResp,
	)

	require.Equal(t, []service.RequestAuditAttempt{
		{Model: "gpt-5.4", Protocol: service.RequestAuditProtocolOpenAIResp, Stage: service.RequestAuditStageClientEntry},
		{AccountID: 22, Protocol: service.RequestAuditProtocolOpenAIChat, Stage: service.RequestAuditStageWire},
	}, got)
}

func TestAppendOpenAITransportAttemptsIncludesObservedWireFactsOnly(t *testing.T) {
	zero := int64(0)
	status := 204
	complete := true
	attempts := appendOpenAITransportAttempts(nil, []httpattempt.Metadata{{
		AccountID:       4,
		Model:           "sk-hidden-model",
		Protocol:        service.RequestAuditProtocolOpenAIResp,
		RequestHeaders:  map[string]any{"X-Stainless-Lang": "go", "Authorization": map[string]any{"present": true}, "X-Secrets": "sk-hidden"},
		ResponseHeaders: map[string]any{"Content-Type": "application/json", "Set-Cookie": map[string]any{"present": true}},
		StatusCode:      &status, RequestBytes: &zero, ResponseBytes: &zero, ResponseReadComplete: &complete,
	}}, 0, "sk-client-model", "sk-upstream-model", nil, service.RequestAuditProtocolOpenAIResp)
	require.Len(t, attempts, 3)
	wire := attempts[2]
	require.Empty(t, wire.Model)
	require.Equal(t, status, *wire.UpstreamStatus)
	require.Equal(t, zero, *wire.RequestPayloadBytes)
	require.Equal(t, zero, *wire.ResponsePayloadBytes)
	require.True(t, *wire.ResponseReadComplete)
	require.Equal(t, "go", wire.WireRequestHeaders["X-Stainless-Lang"])
	require.NotContains(t, wire.WireRequestHeaders, "X-Secrets")
	require.Equal(t, map[string]any{"present": true}, wire.UpstreamResponseHeaders["Set-Cookie"])
}

func TestAppendOpenAITransportAttemptsFingerprintsModelAliasesPerRecord(t *testing.T) {
	fingerprinter, err := service.NewRequestAuditFingerprinter(strings.Repeat("k", 32))
	require.NoError(t, err)
	first, err := fingerprinter.BeginForUser(7)
	require.NoError(t, err)
	otherUser, err := fingerprinter.BeginForUser(8)
	require.NoError(t, err)

	const clientAlias = "model-canary-client"
	const normalizedAlias = "model-canary-normalized"
	attempts := appendOpenAITransportAttempts(nil, []httpattempt.Metadata{{
		Model:    "model-canary-client",
		Protocol: service.RequestAuditProtocolOpenAIResp,
	}, {
		Model:    "model-canary-normalized",
		Protocol: service.RequestAuditProtocolOpenAIResp,
	}}, 0, clientAlias, normalizedAlias, first, service.RequestAuditProtocolOpenAIResp)
	require.Len(t, attempts, 4)
	require.Equal(t, first.DigestModel(clientAlias), attempts[0].ModelFingerprint)
	require.Equal(t, first.DigestModel(normalizedAlias), attempts[1].ModelFingerprint)
	require.Equal(t, attempts[0].ModelFingerprint, attempts[2].ModelFingerprint, "same alias at client and wire stages has same digest")
	require.Equal(t, attempts[1].ModelFingerprint, attempts[3].ModelFingerprint, "same alias at normalized and wire stages has same digest")
	require.NotEqual(t, attempts[0].ModelFingerprint, attempts[1].ModelFingerprint, "different aliases have distinct digests")
	require.NotEqual(t, attempts[0].ModelFingerprint, otherUser.DigestModel(clientAlias), "different users have distinct digests")

	encoded, err := json.Marshal(service.BuildRequestAuditRecord(service.RequestAuditInput{Attempts: attempts}))
	require.NoError(t, err)
	require.NotContains(t, string(encoded), clientAlias)
	require.NotContains(t, string(encoded), normalizedAlias)
	require.Contains(t, string(encoded), `"model_fingerprint"`)
}

func TestOpenAIMessagesPostNormalizeModelFingerprintsNormalizedReasoningAlias(t *testing.T) {
	fingerprinter, err := service.NewRequestAuditFingerprinter(strings.Repeat("k", 32))
	require.NoError(t, err)
	fp, err := fingerprinter.BeginForUser(7)
	require.NoError(t, err)

	const clientModel = "gpt-5.4-high"
	const normalizedModel = "gpt-5.4"
	postNormalizeModel := openAIMessagesPostNormalizeModel(service.ChannelMappingResult{}, clientModel)
	require.Equal(t, normalizedModel, postNormalizeModel)

	attempts := appendOpenAITransportAttempts(nil, []httpattempt.Metadata{{
		Model: normalizedModel,
		Protocol: service.RequestAuditProtocolOpenAIResp,
	}}, 0, clientModel, postNormalizeModel, fp, service.RequestAuditProtocolAnthropic)

	require.Len(t, attempts, 3)
	require.Equal(t, fp.DigestModel(clientModel), attempts[0].ModelFingerprint)
	require.Equal(t, fp.DigestModel(normalizedModel), attempts[1].ModelFingerprint)
	require.Equal(t, fp.DigestModel(normalizedModel), attempts[2].ModelFingerprint)
	require.NotEqual(t, attempts[0].ModelFingerprint, attempts[1].ModelFingerprint)

	encoded, err := json.Marshal(service.BuildRequestAuditRecord(service.RequestAuditInput{Attempts: attempts}))
	require.NoError(t, err)
	require.NotContains(t, string(encoded), clientModel)
	require.NotContains(t, string(encoded), normalizedModel)
}

func TestAppendOpenAITransportAttemptsSkipsEmptyDelta(t *testing.T) {
	attempts := appendOpenAITransportAttempts(
		nil,
		nil,
		0,
		"gpt-5.4",
		"claude-sonnet-4",
		nil,
		service.RequestAuditProtocolOpenAIResp,
	)

	require.Nil(t, attempts)
}
