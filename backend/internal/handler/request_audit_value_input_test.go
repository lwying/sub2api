//go:build unit

package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRequestAuditValueInputFromTransportUsesBoundedActualBodyIdentifier(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	counter := httpattempt.NewCounter()
	c.Set("request_audit_http_attempt_counter", counter)
	ctx := httpattempt.WithClaudeHeaderValueCapture(t.Context(), true)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.local/v1/messages", nil)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req = serviceTestBindValueAttempt(req, counter, service.RequestAuditProtocolAnthropic)
	c.Request = req
	seen := requestAuditValueInputFromTransport(c, true, "/v1/messages", service.RequestAuditProtocolAnthropic,
		"model", time.Now(), service.RequestAuditMetadata{}, []byte(`{"metadata":{"user_id":"user_bounded"}}`))
	require.Equal(t, "user_bounded", seen.MetadataUserID)
	require.Len(t, seen.Attempts, 1)

	tooLong := make([]byte, 300)
	for i := range tooLong {
		tooLong[i] = 'a'
	}
	seen = requestAuditValueInputFromTransport(c, true, "/v1/messages", service.RequestAuditProtocolAnthropic,
		"model", time.Now(), service.RequestAuditMetadata{}, append(append([]byte(`{"metadata":{"user_id":"`), tooLong...), []byte(`"}}`)...))
	require.Empty(t, seen.MetadataUserID)
}

// 入站是 Chat Completions、真实出站是 Anthropic：入站头值必须按**入站**协议评审，
// 不得因为最终账号是 Claude 就把 Messages 的闭集套到入站 wire 上。
func TestRequestAuditValueInputFromTransportUsesInboundProtocolForInboundHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	counter := httpattempt.NewCounter()
	c.Set("request_audit_http_attempt_counter", counter)
	ctx := httpattempt.WithClaudeHeaderValueCapture(t.Context(), true)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://gateway.local/v1/chat/completions", nil)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Anthropic-Beta", "claude-code-20250219")
	req.Header.Set("User-Agent", "claude-cli/2.1.258 (external, cli)")
	req = serviceTestBindValueAttempt(req, counter, service.RequestAuditProtocolAnthropic)
	c.Request = req

	seen := requestAuditValueInputFromTransport(c, true, "/v1/chat/completions",
		service.RequestAuditProtocolAnthropic, "model", time.Now(), service.RequestAuditMetadata{})
	require.Equal(t, service.RequestAuditProtocolAnthropic, seen.Protocol,
		"出站形态只来自逐次真实尝试")
	require.Equal(t, "application/json", seen.InboundHeaderValues["Content-Type"])
	require.Equal(t, "gzip", seen.InboundHeaderValues["Accept-Encoding"])
	require.NotContains(t, seen.InboundHeaderValues, "Anthropic-Beta",
		"入站是 Chat Completions，Messages 闭集不得套到入站 wire 上")
	require.NotContains(t, seen.InboundHeaderValues, "User-Agent",
		"Messages 闭集里的客户端头不适用于 Chat Completions 入站")
}

// 入站是 Messages、真实出站是 OpenAI Responses：入站头值与入站标识必须按 Messages 协议采集，
// 不能被逐次尝试的 wire 形态改写。
func TestRequestAuditValueInputFromTransportKeepsMessagesInboundUnderOpenAIWire(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	counter := httpattempt.NewCounter()
	c.Set("request_audit_http_attempt_counter", counter)
	ctx := httpattempt.WithClaudeHeaderValueCapture(t.Context(), true)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://gateway.local/v1/messages", nil)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Beta", "claude-code-20250219")
	req.Header.Set("User-Agent", "claude-cli/2.1.258 (external, cli)")
	req.Header.Set("Authorization", "Bearer forbidden-canary")
	req = serviceTestBindValueAttempt(req, counter, service.RequestAuditProtocolOpenAIResp)
	c.Request = req

	seen := requestAuditValueInputFromTransport(c, true, "/v1/messages",
		service.RequestAuditProtocolAnthropic, "model", time.Now(), service.RequestAuditMetadata{},
		[]byte(`{"metadata":{"user_id":"user_bounded"}}`))
	require.Equal(t, service.RequestAuditProtocolOpenAIResp, seen.Protocol,
		"出站形态来自真实尝试，入站路由不参与推断")
	require.Equal(t, "claude-code-20250219", seen.InboundHeaderValues["Anthropic-Beta"],
		"Messages 入站头值必须按 Messages 闭集采集")
	require.Equal(t, "claude-cli/2.1.258 (external, cli)", seen.InboundHeaderValues["User-Agent"])
	require.Equal(t, map[string]any{"present": true}, seen.InboundHeaderValues["Authorization"],
		"凭据头只留存在性标记，取值绝不进入快照")
	require.Equal(t, "user_bounded", seen.MetadataUserID,
		"入站正文里的标识按入站协议解析，不随出站 wire 漂移")
}

// 值明细的采集入口判定必须用**规范化后的**入站路由：/antigravity/v1/messages 与
// /v1/messages 是同一个 Messages 入口，只比原始 path 会让 Antigravity 平台的请求
// 「有逐次审计、却没有值明细行」。count_tokens 会被路径规范化折叠成 Messages，
// 必须仍被判在范围外。
func TestRequestAuditValueDetailInboundRouteMatchesNormalizedRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, testCase := range []struct {
		path string
		want bool
	}{
		{"/v1/messages", true},
		{"/antigravity/v1/messages", true},
		{"/v1/messages/count_tokens", false},
		{"/antigravity/v1/messages/count_tokens", false},
		{"/v1/chat/completions", false},
		{"/v1/responses", false},
	} {
		c, _ := gin.CreateTestContext(nil)
		c.Request = httptest.NewRequest(http.MethodPost, testCase.path, nil)
		require.Equal(t, testCase.want, requestAuditValueDetailInboundRouteMatches(c),
			"入站路由判定错误：%s", testCase.path)
	}
}

// 出站 wire 形态的推导：未知/空/混合形态必须标 unsupported，绝不冒充 Bedrock；
// Bedrock 只能由账号事实或真实的 bedrock 尝试得到。
func TestRequestAuditValueDetailWireProtocolNeverFakesBedrock(t *testing.T) {
	anthropic := []service.RequestAuditValueDetailAttempt{{Index: 1, Protocol: service.RequestAuditProtocolAnthropic}}
	require.Equal(t, service.RequestAuditProtocolAnthropic, requestAuditValueDetailWireProtocol(anthropic, false))
	require.Equal(t, requestAuditValueDetailProtocolBedrock, requestAuditValueDetailWireProtocol(anthropic, true))
	require.Equal(t, requestAuditValueDetailProtocolBedrock,
		requestAuditValueDetailWireProtocol([]service.RequestAuditValueDetailAttempt{{Index: 1, Protocol: requestAuditValueDetailProtocolBedrock}}, false))

	// 空协议（未知形态）与闭集外的协议都不能被写成 Bedrock。
	require.Equal(t, requestAuditValueDetailProtocolUnsupported,
		requestAuditValueDetailWireProtocol([]service.RequestAuditValueDetailAttempt{{Index: 1}}, false),
		"空协议是未知形态，不是 Bedrock")
	require.Equal(t, requestAuditValueDetailProtocolUnsupported,
		requestAuditValueDetailWireProtocol([]service.RequestAuditValueDetailAttempt{{Index: 1, Protocol: "openai.responses"}}, false))
	require.Equal(t, requestAuditValueDetailProtocolUnsupported,
		requestAuditValueDetailWireProtocol([]service.RequestAuditValueDetailAttempt{
			{Index: 1, Protocol: requestAuditValueDetailProtocolBedrock},
			{Index: 2},
		}, false), "混合形态必须收敛成未支持")
	require.Equal(t, requestAuditValueDetailProtocolUnsupported,
		requestAuditValueDetailWireProtocol([]service.RequestAuditValueDetailAttempt{{Index: 1}}, true),
		"未知形态即使来自 Bedrock 账号也不得被当成 Bedrock 形态")
}

func serviceTestBindValueAttempt(req *http.Request, counter *httpattempt.Counter, protocol string) *http.Request {
	ctx := httpattempt.WithCounter(req.Context(), counter)
	ctx = httpattempt.WithMetadata(ctx, httpattempt.Metadata{Protocol: protocol, AccountID: 1})
	req = req.WithContext(ctx)
	attempt := httpattempt.StartRequestAttempt(req)
	attempt.SetResponse(200, http.Header{"Content-Type": {"application/json"}}, false)
	return req
}
