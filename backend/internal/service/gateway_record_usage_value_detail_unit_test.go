//go:build unit

package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/stretchr/testify/require"
)

func TestGatewayRecordUsageAttachesClaudeValueDetailAfterAuditWithTwoAttempts(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	auditRepo := &stubRequestAuditRepo{}
	valueRepo := &stubValueDetailRepo{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	svc.requestAuditRepo = auditRepo
	svc.SetRequestAuditValueDetailCapture(NewRequestAuditValueDetailCapture(valueRepo, stubValueDetailGate{gate: valueDetailOpenGate()}))
	const inboundUID = `{"device_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","account_uuid":"","session_id":"123e4567-e89b-12d3-a456-426614174000"}`
	const outboundUID = `{"device_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","account_uuid":"","session_id":"123e4567-e89b-12d3-a456-426614174001"}`
	input := RecordUsageInput{
		Result: &ForwardResult{RequestID: "gateway_value_detail", Model: "claude-sonnet-4", Usage: ClaudeUsage{InputTokens: 10, OutputTokens: 6}, Duration: time.Second},
		APIKey: &APIKey{ID: 501, Quota: 100}, User: &User{ID: 601}, Account: &Account{ID: 701},
		RequestAuditHeaders:  http.Header{"User-Agent": {"claude-cli/2.1.258 (external, cli)"}},
		RequestAuditAttempts: []RequestAuditAttempt{{AccountID: 701, Stage: RequestAuditStageWire, Protocol: RequestAuditProtocolAnthropic}},
		RequestAuditValueDetail: RequestAuditValueDetailInput{
			Route: "/v1/messages", Protocol: RequestAuditProtocolAnthropic, MetadataUserID: inboundUID, Model: "claude-sonnet-4", ClientStatus: 200,
			InboundHeaderValues: httpattempt.SanitizeClaudeRequestHeaderValues(http.Header{"User-Agent": {"claude-cli/2.1.258 (external, cli)"}}),
			Attempts: []RequestAuditValueDetailAttempt{
				{Index: 1, AccountID: 701, MetadataUserID: inboundUID, UpstreamStatus: valueDetailInt(429), RequestHeaderValues: httpattempt.SanitizeClaudeRequestHeaderValues(http.Header{"User-Agent": {"claude-cli/2.1.258 (external, cli)"}}), ResponseHeaderValues: httpattempt.SanitizeClaudeResponseHeaderValues(http.Header{"Retry-After": {"42"}})},
				{Index: 2, AccountID: 702, MetadataUserID: outboundUID, UpstreamStatus: valueDetailInt(200), RequestHeaderValues: httpattempt.SanitizeClaudeRequestHeaderValues(http.Header{"User-Agent": {"claude-cli/2.1.258 (external, cli)"}})},
			},
		},
	}
	err := svc.RecordUsage(context.Background(), &input)
	require.NoError(t, err)
	require.NotNil(t, auditRepo.created)
	require.Equal(t, map[string]any{"present": true}, auditRepo.created.Headers["User-Agent"], "long-lived audit keeps only UA presence")
	require.Len(t, valueRepo.created, 1)
	require.Equal(t, auditRepo.created.UsageLogID, valueRepo.created[0].UsageLogID)
	values, err := DecodeRequestAuditValueDetailValues(valueRepo.created[0].Payload)
	require.NoError(t, err)
	require.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", values.Inbound.DeviceID)
	require.Len(t, values.Attempts, 2)
	require.Equal(t, int64(701), values.Attempts[0].AccountID)
	require.Equal(t, int64(702), values.Attempts[1].AccountID)
	require.Equal(t, "123e4567-e89b-12d3-a456-426614174001", values.Attempts[1].SessionID)
	require.Equal(t, []string{"42"}, values.Attempts[0].ResponseHeaders["Retry-After"])
	require.NotContains(t, strings.ToLower(string(valueRepo.created[0].Payload)), "private prompt")
}

func valueDetailInt(n int) *int { return &n }
