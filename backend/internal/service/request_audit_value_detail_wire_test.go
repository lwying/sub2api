package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestClaudeAuditValueDetailBindingUsesEachActualWireAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	counter := httpattempt.NewCounter()
	c.Set(requestAuditHTTPAttemptCounterKey, counter)
	const inboundUID = `{"device_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","account_uuid":"","session_id":"123e4567-e89b-12d3-a456-426614174000"}`
	const rewrittenUID = `{"device_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","account_uuid":"","session_id":"123e4567-e89b-12d3-a456-426614174001"}`

	for index, userID := range []string{inboundUID, rewrittenUID} {
		body := []byte(`{"model":"claude-sonnet-4-5","metadata":{"user_id":` + quoteWireTest(userID) + `},"messages":[{"role":"user","content":"private prompt"}]}`)
		ctx := httpattempt.WithClaudeHeaderValueCapture(context.Background(), true)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("User-Agent", "claude-cli/2.1.258 (external, cli)")
		req.Header.Set("Anthropic-Beta", "claude-code-20250219")
		req.Header.Set("Authorization", "Bearer forbidden")
		req = bindRequestAuditHTTPAttempt(req, c, int64(index+1), "claude-sonnet-4-5", RequestAuditProtocolAnthropic)
		req = bindClaudeRequestAuditValueDetail(req, body, nil)
		attempt := httpattempt.StartRequestAttempt(req)
		if index == 0 {
			attempt.SetResponse(http.StatusTooManyRequests, http.Header{"Retry-After": {"42"}, "Anthropic-Ratelimit-Requests-Reset": {"2026-09-24T14:01:30Z"}}, true)
		} else {
			attempt.SetResponse(http.StatusOK, http.Header{"Content-Type": {"application/json"}}, true)
		}
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	input := RequestAuditValueDetailInput{
		Route:          "/v1/messages",
		Protocol:       RequestAuditProtocolAnthropic,
		Model:          "claude-sonnet-4-5",
		MetadataUserID: inboundUID,
		Attempts:       RequestAuditValueDetailAttemptsFromHTTPMetadata(counter.Metadata()),
	}
	write := BuildRequestAuditValueDetailWrite(input, RequestAuditValueDetailGate{CaptureAllowed: true, EncryptionAvailable: true}, timeNowWireTest())
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailStateStored, write.State)
	values, err := DecodeRequestAuditValueDetailValues(write.Payload)
	require.NoError(t, err)
	require.Len(t, values.Attempts, 2)
	require.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", values.Attempts[0].DeviceID)
	require.Equal(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", values.Attempts[1].DeviceID)
	require.Equal(t, []string{"42"}, values.Attempts[0].ResponseHeaders["Retry-After"])
	require.NotContains(t, values.Attempts[1].ResponseHeaders, "Retry-After")
	require.NotContains(t, string(write.Payload), "forbidden")
	require.NotContains(t, string(write.Payload), "private prompt")
}

func TestClaudeAuditValueDetailRejectsFreeFormPlaintextProtocol(t *testing.T) {
	in := RequestAuditValueDetailInput{Route: "/v1/messages", Protocol: "token=private", Model: "claude-sonnet-4-5"}
	write := BuildRequestAuditValueDetailWrite(in, RequestAuditValueDetailGate{CaptureAllowed: true, EncryptionAvailable: true}, time.Now())
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailSkippedOutOfScope, write.Reason)
	require.Empty(t, write.Fields.Protocol)
	require.Empty(t, write.Payload)
}

func TestClaudeAuditValueDetailBedrockIsMarkedOutOfScope(t *testing.T) {
	in := RequestAuditValueDetailInput{
		Route: "/v1/messages", Protocol: "bedrock", Model: "claude-sonnet-4-5",
		InboundHeaderValues: httpattempt.SanitizeClaudeRequestHeaderValues(http.Header{"User-Agent": {"claude-cli/2.1.258 (external, cli)"}}),
	}
	write := BuildRequestAuditValueDetailWrite(in, RequestAuditValueDetailGate{CaptureAllowed: true, EncryptionAvailable: true}, time.Now())
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailSkippedOutOfScope, write.Reason)
	require.Empty(t, write.Payload)
}

func TestClaudeAuditValueDetailWireBindingIsOffByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	counter := httpattempt.NewCounter()
	c.Set(requestAuditHTTPAttemptCounterKey, counter)
	body := []byte(`{"metadata":{"user_id":"user_a"}}`)
	req, err := http.NewRequestWithContext(context.Background(), "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("User-Agent", "claude-cli/2.1.258 (external, cli)")
	req = bindRequestAuditHTTPAttempt(req, c, 1, "claude", RequestAuditProtocolAnthropic)
	req = bindClaudeRequestAuditValueDetail(req, body, nil)
	attempt := httpattempt.StartRequestAttempt(req)
	attempt.SetResponse(429, http.Header{"Retry-After": {"42"}}, false)
	snapshot := counter.Metadata()
	require.Len(t, snapshot, 1)
	require.Empty(t, snapshot[0].MetadataUserID)
	require.Empty(t, snapshot[0].RequestHeaderValues)
	require.Empty(t, snapshot[0].ResponseHeaderValues)
}

// 端到端：入站阶段被净化器丢掉的未知头名，在服务层已经看不见了；省略摘要必须由采集侧
// 带进来，载荷才能如实报出 truncated。名字与取值本身绝不落库。
func TestClaudeAuditValueDetailWireTruncatedTracksIntakeOmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	counter := httpattempt.NewCounter()
	c.Set(requestAuditHTTPAttemptCounterKey, counter)
	const canaryName = "X-Unknown-Canary"
	const canaryValue = "private-unknown-canary-value"

	inboundHeaders := http.Header{}
	inboundHeaders.Set("User-Agent", "claude-cli/2.1.258 (external, cli)")
	inboundHeaders.Set("Authorization", "Bearer forbidden")
	inboundHeaders.Set(canaryName, canaryValue)
	inboundValues, inboundOmission := RequestAuditValueDetailInboundHeaders(inboundHeaders)
	require.True(t, inboundOmission.Any(), "闭集外的入站头名必须被计入省略")

	body := []byte(`{"model":"claude-sonnet-4-5","metadata":{"user_id":"user_a"},"messages":[{"role":"user","content":"private prompt"}]}`)
	ctx := httpattempt.WithClaudeHeaderValueCapture(context.Background(), true)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("User-Agent", "claude-cli/2.1.258 (external, cli)")
	req.Header.Set("Authorization", "Bearer forbidden")
	req.Header.Set(canaryName, canaryValue)
	req = bindRequestAuditHTTPAttempt(req, c, 1, "claude-sonnet-4-5", RequestAuditProtocolAnthropic)
	req = bindClaudeRequestAuditValueDetail(req, body, nil)
	attempt := httpattempt.StartRequestAttempt(req)
	attempt.SetResponse(http.StatusOK, http.Header{"Content-Type": {"application/json"}, "X-Upstream-Canary": {canaryValue}}, true)
	_, _ = io.Copy(io.Discard, req.Body)
	_ = req.Body.Close()

	input := RequestAuditValueDetailInput{
		Route:                 "/v1/messages",
		Protocol:              RequestAuditProtocolAnthropic,
		InboundHeaderValues:   inboundValues,
		InboundHeaderOmission: inboundOmission,
		Model:                 "claude-sonnet-4-5",
		Attempts:              RequestAuditValueDetailAttemptsFromHTTPMetadata(counter.Metadata()),
	}
	write := BuildRequestAuditValueDetailWrite(input, RequestAuditValueDetailGate{CaptureAllowed: true, EncryptionAvailable: true}, timeNowWireTest())
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailStateStored, write.State)
	payload := string(write.Payload)
	require.Contains(t, payload, `"truncated":true`, "丢过条目必须如实标记")
	require.NotContains(t, payload, canaryName, "未知头名不得落库")
	require.NotContains(t, payload, canaryValue, "未知头取值不得落库")

	// 已知凭据头（每个正常 Claude Code 请求都带）不是「丢过条目」：
	// 只有正常头时载荷不得标成不完整。
	cleanHeaders := http.Header{}
	cleanHeaders.Set("User-Agent", "claude-cli/2.1.258 (external, cli)")
	cleanHeaders.Set("Authorization", "Bearer forbidden")
	cleanValues, cleanOmission := RequestAuditValueDetailInboundHeaders(cleanHeaders)
	require.False(t, cleanOmission.Any())
	input.InboundHeaderValues = cleanValues
	input.InboundHeaderOmission = cleanOmission
	input.Attempts = nil
	clean := BuildRequestAuditValueDetailWrite(input, RequestAuditValueDetailGate{CaptureAllowed: true, EncryptionAvailable: true}, timeNowWireTest())
	require.NotNil(t, clean)
	cleanValuesDecoded, err := DecodeRequestAuditValueDetailValues(clean.Payload)
	require.NoError(t, err)
	require.False(t, cleanValuesDecoded.Truncated)
	require.NotContains(t, string(clean.Payload), "forbidden")
}

// 端到端：17 次真实上游尝试时必须仍然写得进去。attempt_count 饱和在存储上限
// （迁移 253 的检查约束 attempt_count <= 16），而不是越界让整条事实被数据库拒收。
func TestClaudeAuditValueDetailWireSaturatesAttemptCountAtStorageBound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	counter := httpattempt.NewCounter()
	c.Set(requestAuditHTTPAttemptCounterKey, counter)
	body := []byte(`{"model":"claude-sonnet-4-5","metadata":{"user_id":"user_a"},"messages":[{"role":"user","content":"private prompt"}]}`)
	ctx := httpattempt.WithClaudeHeaderValueCapture(context.Background(), true)
	for index := 0; index <= RequestAuditValueDetailMaxStoredAttemptCount; index++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("User-Agent", "claude-cli/2.1.258 (external, cli)")
		req = bindRequestAuditHTTPAttempt(req, c, int64(index+1), "claude-sonnet-4-5", RequestAuditProtocolAnthropic)
		req = bindClaudeRequestAuditValueDetail(req, body, nil)
		attempt := httpattempt.StartRequestAttempt(req)
		attempt.SetResponse(http.StatusTooManyRequests, http.Header{"Retry-After": {"42"}}, true)
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	input := RequestAuditValueDetailInput{
		Route:                 "/v1/messages",
		Protocol:              RequestAuditProtocolAnthropic,
		InboundHeaderValues:   mustInboundWireHeaderValues(t),
		InboundHeaderOmission: httpattempt.ClaudeHeaderValueOmission{},
		Attempts:              RequestAuditValueDetailAttemptsFromHTTPMetadata(counter.Metadata()),
	}
	write := BuildRequestAuditValueDetailWrite(input, RequestAuditValueDetailGate{CaptureAllowed: true, EncryptionAvailable: true}, timeNowWireTest())
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailSkippedTooManyAttempts, write.Reason)
	require.Equal(t, RequestAuditValueDetailMaxStoredAttemptCount, write.AttemptCount,
		"17 次尝试必须饱和在存储上限，而不是越界")
	require.Empty(t, write.Payload)
}

func mustInboundWireHeaderValues(t *testing.T) map[string]any {
	t.Helper()
	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/2.1.258 (external, cli)")
	values, omission := RequestAuditValueDetailInboundHeaders(headers)
	require.False(t, omission.Any())
	return values
}

func quoteWireTest(s string) string {
	return strconv.Quote(s)
}

func timeNowWireTest() time.Time { return time.Now().UTC() }
