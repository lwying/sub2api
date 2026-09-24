//go:build unit

package service

// 诊断邻近日志的 sentinel 测试（契约见 docs/adr/0005-short-lived-error-body-diagnostics-scope.md
// 排除的结构化凭据：认证头、Cookie、API Key、代理凭据、URL query 密钥一律不得进入普通日志）。
// 该契约针对的已知风险是现有网关调试与错误日志可能输出完整请求体、system prompt 片段或上游
// 响应正文。
//
// 每个用例注入合成的哨兵值（正文自由文本、Authorization、Cookie、代理凭据、URL query 密钥）
// 并断言这些值不会出现在网关普通日志里；期望值全部写在测试内，不用被断言的生产函数反推。

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 合成哨兵：只用于本测试文件，不来自任何真实请求或凭据。
const (
	sentinelFreeTextBody = "CANARY_FREEFORM_UPSTREAM_BODY_9f2c"
	sentinelUpstreamMsg  = "CANARY_UPSTREAM_ERROR_MESSAGE_1a7b"
	sentinelSystemPrompt = "CANARY_SYSTEM_PROMPT_a41d"
	sentinelAuthToken    = "CANARY_AUTH_BEARER_7c33"
	sentinelAPIKeyHeader = "CANARY_XAPIKEY_5be1"
	sentinelCookie       = "CANARY_COOKIE_2d90"
	sentinelProxyAuth    = "CANARY_PROXY_AUTH_8f45"
	sentinelURLQueryCred = "CANARY_URL_QUERY_CRED_6e02"
	sentinelRequestID    = "CANARY_UPSTREAM_REQUEST_ID_4b18"
)

var gatewayLogCaptureMu sync.Mutex

type gatewayLogCapture struct {
	mu     sync.Mutex
	events []*logger.LogEvent
}

func (c *gatewayLogCapture) WriteLogEvent(event *logger.LogEvent) {
	if event == nil {
		return
	}
	cloned := *event
	if event.Fields != nil {
		cloned.Fields = make(map[string]any, len(event.Fields))
		for k, v := range event.Fields {
			cloned.Fields[k] = v
		}
	}
	c.mu.Lock()
	c.events = append(c.events, &cloned)
	c.mu.Unlock()
}

func (c *gatewayLogCapture) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.events))
	for _, ev := range c.events {
		if ev != nil {
			out = append(out, ev.Message)
		}
	}
	return out
}

func (c *gatewayLogCapture) contains(substr string) bool {
	for _, msg := range c.messages() {
		if strings.Contains(msg, substr) {
			return true
		}
	}
	return false
}

// captureGatewayLogs 复用既有 logger sink 接缝捕获结构化日志事件（与
// openai_oauth_passthrough_test.go 的 captureStructuredLog 同一机制），
// 不劫持进程 stdout。
func captureGatewayLogs(t *testing.T) *gatewayLogCapture {
	t.Helper()
	gatewayLogCaptureMu.Lock()
	t.Cleanup(func() {
		logger.SetSink(nil)
		gatewayLogCaptureMu.Unlock()
	})
	require.NoError(t, logger.Init(logger.InitOptions{
		Level:       "debug",
		Format:      "json",
		ServiceName: "sub2api",
		Environment: "test",
		Output: logger.OutputOptions{
			ToStdout: true,
			ToFile:   false,
		},
		Sampling: logger.SamplingOptions{Enabled: false},
	}))
	capture := &gatewayLogCapture{}
	logger.SetSink(capture)
	return capture
}

func sentinelAccount() *Account {
	return &Account{ID: 42, Name: "sentinel-account", Platform: PlatformAnthropic, Type: AccountTypeOAuth}
}

func sentinelGatewayRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"https://api.anthropic.com/v1/messages?beta=true&api_key="+sentinelURLQueryCred, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+sentinelAuthToken)
	req.Header.Set("x-api-key", sentinelAPIKeyHeader)
	req.Header.Set("Cookie", "session="+sentinelCookie)
	req.Header.Set("Proxy-Authorization", "Basic "+sentinelProxyAuth)
	req.Header.Set("User-Agent", "claude-cli/2.1.193 (external, cli)")
	return req
}

func sentinelSystemBody() []byte {
	return []byte(`{"model":"claude-sonnet-4-6","system":[{"type":"text","text":"` + sentinelSystemPrompt +
		`"}],"metadata":{"user_id":"user_sentinel_account_1_session_2"},"messages":[]}`)
}

// RED（修复前）：[ClaudeMimicDebugOnError] 这类「无需开启任何调试标志」的错误日志会把
// system prompt 片段（模型正文）连同请求头一起写进普通日志。哨兵必须证明它不再出现，
// 同时保留可排障的安全结构字段。
func TestBuildClaudeMimicDebugLine_OmitsFreeFormSystemPrompt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := sentinelSystemBody()
	req := sentinelGatewayRequest(t, body)

	line := buildClaudeMimicDebugLine(req, body, sentinelAccount(), "oauth", true)
	require.NotEmpty(t, line)

	// 模型正文（system prompt 自由文本）不得出现在日志行里。
	require.NotContains(t, line, sentinelSystemPrompt)
	// 凭据明文不得出现在日志行里（头、URL query）。
	require.NotContains(t, line, sentinelAuthToken)
	require.NotContains(t, line, sentinelAPIKeyHeader)
	require.NotContains(t, line, sentinelCookie)
	require.NotContains(t, line, sentinelProxyAuth)
	require.NotContains(t, line, sentinelURLQueryCred)

	// 稳定的安全字段必须保留：阶段/账号/凭据类型/伪装开关与有界的 system 结构信号。
	require.Contains(t, line, "account=42(sentinel-account)")
	require.Contains(t, line, "tokenType=oauth")
	require.Contains(t, line, "mimic=true")
	require.Contains(t, line, "url=https://api.anthropic.com/v1/messages?beta=true")
	require.Contains(t, line, "system.present=true")
	require.Contains(t, line, "system.bytes=")
}

// RED（修复前）：上游 4xx/5xx 的「不可重试」日志无条件打印上游响应正文字段，
// 与是否开启 Gateway.LogUpstreamErrorBody 无关。哨兵必须证明上游自由文本不再进入日志，
// 而账号/状态码/上游请求 ID/错误种类继续保留。
func TestHandleErrorResponse_LogsNoUpstreamBodyOrCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	capture := captureGatewayLogs(t)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"` +
		sentinelUpstreamMsg + `"},"free_text":"` + sentinelFreeTextBody + `"}`)
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"X-Request-Id": []string{sentinelRequestID}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}

	s := &GatewayService{}
	_, err := s.handleErrorResponse(context.Background(), resp, c, sentinelAccount())
	require.Error(t, err, "上游 5xx 仍应返回错误，日志硬化不得改变错误处理")

	// 修硬化的目标：自由文本正文与上游错误 message 原文都不写普通日志。
	require.False(t, capture.contains(sentinelFreeTextBody),
		"上游响应正文自由文本不得进入普通日志: %v", capture.messages())
	require.False(t, capture.contains(sentinelUpstreamMsg),
		"上游错误 message 原文不得进入普通日志: %v", capture.messages())

	// 稳定安全字段保留（阶段/账号/状态码/code/上游请求 ID）。
	require.True(t, capture.contains("[Forward] Upstream error (non-retryable)"),
		"阶段标识必须保留: %v", capture.messages())
	require.True(t, capture.contains("Status=500"), "上游状态码必须保留: %v", capture.messages())
	require.True(t, capture.contains("Kind=http_error"), "错误种类必须保留: %v", capture.messages())
	require.True(t, capture.contains("Account=42(sentinel-account)"), "账号标识必须保留: %v", capture.messages())
	require.True(t, capture.contains(sentinelRequestID), "上游请求 ID 必须保留: %v", capture.messages())
}

// RED（修复前）：Claude Code 凭据域错误的诊断行在「未开启任何调试标志」时也会打印。
// 该路径必须只打印脱敏后的指纹行。
func TestHandleErrorResponse_CredentialScopeErrorLogsNoModelBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	capture := captureGatewayLogs(t)

	body := sentinelSystemBody()
	req := sentinelGatewayRequest(t, body)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	c.Set(claudeMimicDebugInfoKey, buildClaudeMimicDebugLine(req, body, sentinelAccount(), "oauth", true))

	upstreamBody := []byte(`{"type":"error","error":{"type":"permission_error","message":"OAuth token is only authorized for use with Claude Code and cannot be used for other API requests"}}`)
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"X-Request-Id": []string{sentinelRequestID}},
		Body:       io.NopCloser(bytes.NewReader(upstreamBody)),
	}

	s := &GatewayService{}
	_, _ = s.handleErrorResponse(context.Background(), resp, c, sentinelAccount())

	require.True(t, capture.contains("[ClaudeMimicDebugOnError]"),
		"凭据域错误的诊断行必须仍然打印（否则丢失排查能力）: %v", capture.messages())
	require.False(t, capture.contains(sentinelSystemPrompt),
		"system prompt 内容不得随诊断行进入普通日志: %v", capture.messages())
	require.False(t, capture.contains(sentinelAuthToken),
		"认证凭据明文不得随诊断行进入普通日志: %v", capture.messages())
	require.False(t, capture.contains(sentinelCookie),
		"Cookie 明文不得随诊断行进入普通日志: %v", capture.messages())
}

// 凭据类请求头必须整值脱敏（ADR 0005 已排除的结构化凭据：认证头、Cookie、API Key、代理凭据）。
func TestSafeHeaderValueForLog_RedactsCredentialHeaderClasses(t *testing.T) {
	cases := []struct {
		header string
		value  string
		secret string
	}{
		{header: "Authorization", value: "Bearer " + sentinelAuthToken, secret: sentinelAuthToken},
		{header: "Proxy-Authorization", value: "Basic " + sentinelProxyAuth, secret: sentinelProxyAuth},
		{header: "x-api-key", value: sentinelAPIKeyHeader, secret: sentinelAPIKeyHeader},
		{header: "api-key", value: sentinelAPIKeyHeader, secret: sentinelAPIKeyHeader},
		{header: "x-goog-api-key", value: sentinelAPIKeyHeader, secret: sentinelAPIKeyHeader},
		{header: "Cookie", value: "session=" + sentinelCookie, secret: sentinelCookie},
		{header: "Set-Cookie", value: "session=" + sentinelCookie, secret: sentinelCookie},
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			got := safeHeaderValueForLog(tc.header, tc.value)
			require.NotContains(t, got, tc.secret)
			// 既有约定：Bearer 保留 scheme（"Bearer [redacted]"），其余整值 [redacted]。
			require.Contains(t, got, "[redacted]")
			require.True(t, got == "[redacted]" || strings.HasSuffix(got, " [redacted]"),
				"凭据类头应只剩脱敏标记，实际 %q", got)
		})
	}

	// 非凭据的协议头保持不变，避免把 anthropic-beta 之类的语义头误伤成 [redacted]。
	require.Equal(t, "context-management-2025-06-27",
		safeHeaderValueForLog("anthropic-beta", "context-management-2025-06-27"))
	require.Equal(t, "2023-06-01", safeHeaderValueForLog("anthropic-version", "2023-06-01"))
}

// 默认关闭：未显式开启 SUB2API_DEBUG_GATEWAY_BODY 时，快照接缝不写任何内容。
// 显式开启后只写脱敏后的头；完整正文写入是该运维开关下的既有行为，属已声明剩余风险。
func TestDebugLogGatewaySnapshot_DefaultOffAndRedactsCredentialHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	path := filepath.Join(dir, "gateway_debug.log")

	// 默认关闭：没有写入句柄，调用不应产生任何文件内容。
	off := &GatewayService{}
	require.Nil(t, off.debugGatewayBodyFile.Load(), "调试快照默认必须关闭")
	off.debugLogGatewaySnapshot("CLIENT_ORIGINAL", sentinelGatewayRequest(t, nil).Header, sentinelSystemBody(), nil)
	require.NoFileExists(t, path, "默认关闭时不得创建调试日志文件")

	// 显式开启（等价于运维设置 SUB2API_DEBUG_GATEWAY_BODY）：凭据类头必须脱敏。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	on := &GatewayService{}
	on.debugGatewayBodyFile.Store(f)
	on.debugLogGatewaySnapshot("CLIENT_ORIGINAL", sentinelGatewayRequest(t, nil).Header, sentinelSystemBody(), map[string]string{"url": "https://api.anthropic.com/v1/messages"})
	require.NoError(t, f.Sync())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	written := string(raw)
	require.Contains(t, written, "CLIENT_ORIGINAL")
	require.NotContains(t, written, sentinelAuthToken)
	require.NotContains(t, written, sentinelAPIKeyHeader)
	require.NotContains(t, written, sentinelCookie)
	require.NotContains(t, written, sentinelProxyAuth)
}
