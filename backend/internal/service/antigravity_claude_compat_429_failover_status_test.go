//go:build unit

package service

// Ticket 03 — Antigravity 其余两个换号入口的换号状态码契约（Claude 转换与 OpenAI 兼容）。
//
// 姊妹用例 antigravity_gemini_429_failover_status_test.go 已覆盖 ForwardGemini。
// 本文件覆盖另外两条把服务层账号切换信号转成 UpstreamFailoverError 的外部入口：
//
//   - Forward（Claude → Gemini 转换，anthropic /v1/messages 形态）
//   - ForwardAsChatCompletions / ForwardAsResponses（OpenAI 兼容形态，
//     共用 forwardAntigravityCompat → handleAntigravityCompatTransportError）
//
// handler 的「请求内 429 账号上限」只统计 StatusCode == 429 的失败
// （handler/request_429_account_limit.go），因此上游真实返回 429 触发换号时，
// 状态码必须原样透传；折叠成 503 会让上限永不触顶：A、B 各自 429 后仍会继续
// 尝试 C。反向约束同样重要：非 429 的切换原因（调度前的模型限流预检查等）必须
// 保持既有 503 语义，不能因为本修改被放宽为 429。
//
// 上游 429 使用 Antigravity 能识别的 RATE_LIMIT_EXCEEDED + RetryInfo.retryDelay(15s)：
// 大于 antigravityRateLimitThreshold(7s) 时服务层直接判定模型限流并立刻返回换号
// 信号，不进入同账号指数退避，因此本用例无需等待。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// antigravityForward429ClaudeModel 是默认 Antigravity 映射中的 Claude 模型（透传），
// Claude 与 OpenAI 兼容两条入口都用它构造请求。
const antigravityForward429ClaudeModel = "claude-sonnet-4-5"

// antigravityForward429RateLimitBody 构造可识别的账号级限流响应：
// reason=RATE_LIMIT_EXCEEDED + metadata.model + retryDelay 15s（> 7s 阈值）。
func antigravityForward429RateLimitBody(model string) string {
	return `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"rate limited","details":[` +
		`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"` + model + `"}},` +
		`{"@type":"google.rpc.RetryInfo","retryDelay":"15s"}]}}`
}

// antigravityForward429Upstream 恒定返回同一个上游响应，并记录调用次数，
// 用于同时断言「换号信号的状态码」与「没有进入同账号重试 / 预检查未发出请求」。
type antigravityForward429Upstream struct {
	HTTPUpstream
	status int
	body   string
	calls  int
}

func (u *antigravityForward429Upstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return u.respond(), nil
}

func (u *antigravityForward429Upstream) DoWithTLS(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
	return u.respond(), nil
}

func (u *antigravityForward429Upstream) respond() *http.Response {
	u.calls++
	return &http.Response{
		StatusCode: u.status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(u.body)),
	}
}

// antigravityForward429Service 组装真实 AntigravityGatewayService，仅注入 stub 上游。
// 凭据自带未过期的 access_token 与 project_id，配合 (nil,nil,nil) 的 token provider
// 不会触发刷新网络调用，也不会走 credits/overages 分支。
func antigravityForward429Service(upstream HTTPUpstream) *AntigravityGatewayService {
	return NewAntigravityGatewayService(
		nil,
		nil,
		nil,
		NewAntigravityTokenProvider(nil, nil, nil),
		nil,
		upstream,
		NewSettingService(&antigravitySettingRepoStub{}, &config.Config{
			Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize},
		}),
		nil,
	)
}

func antigravityForward429Account() *Account {
	return &Account{
		ID:          41,
		Name:        "antigravity-claude-compat-429",
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "test-token",
			"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"project_id":   "test-project",
		},
	}
}

// antigravityForward429PreCooldown 让账号对指定模型处于限流冷却中，从而命中
// antigravityRetryLoop 的调度前预检查分支：该分支的切换信号不带上游状态码
// （UpstreamStatusCode == 0），必须保持 503。
func antigravityForward429PreCooldown(account *Account, model string) {
	now := time.Now()
	setAccountModelRateLimitSnapshot(account, model, now.Add(time.Hour), "", now)
}

// antigravityForward429Context 构造最小可用的 gin 上下文：只提供请求与响应写入器，
// 不依赖 handler 层的中间件键。
func antigravityForward429Context(t *testing.T, path string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	return c, rec
}

// antigravityForward429ClaudeBody 是 Claude → Gemini 入口（Forward）的最小请求。
func antigravityForward429ClaudeBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": antigravityForward429ClaudeModel,
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
		"max_tokens": 16,
		"stream":     false,
	})
	require.NoError(t, err)
	return body
}

// antigravityForward429ChatCompletionsBody 是 OpenAI Chat Completions 兼容入口的最小请求。
func antigravityForward429ChatCompletionsBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":    antigravityForward429ClaudeModel,
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
		"stream":   false,
	})
	require.NoError(t, err)
	return body
}

// antigravityForward429ResponsesBody 是 OpenAI Responses 兼容入口的最小请求。
func antigravityForward429ResponsesBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":  antigravityForward429ClaudeModel,
		"input":  "hello",
		"stream": false,
	})
	require.NoError(t, err)
	return body
}

// requireAntigravityForward429Failover 断言换号信号的契约：状态码、不携带上游正文/响应头、
// 且错误文本不泄露上游错误正文。
func requireAntigravityForward429Failover(t *testing.T, err error, forceCacheBilling bool) *UpstreamFailoverError {
	t.Helper()
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "换号信号必须转成 UpstreamFailoverError")
	require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode,
		"上游真实 429 必须透传 429，否则请求内 429 账号上限无法计入该账号")
	require.Equal(t, forceCacheBilling, failoverErr.ForceCacheBilling)
	require.Empty(t, failoverErr.ResponseBody, "换号信号不得把上游错误正文带给下游")
	require.Nil(t, failoverErr.ResponseHeaders, "换号信号不得把上游响应头带给下游")
	require.NotContains(t, failoverErr.Error(), "rate limited", "错误文本不得泄露上游错误正文")
	return failoverErr
}

// TestForward_GenuineRateLimit429PropagatesFailoverStatus 覆盖 Claude → Gemini 入口：
// 上游真实限流 429 触发换号时，换号信号按 429 透传（而不是折叠成 503），
// 且不进入同账号退避等待。
func TestForward_GenuineRateLimit429PropagatesFailoverStatus(t *testing.T) {
	upstream := &antigravityForward429Upstream{
		status: http.StatusTooManyRequests,
		body:   antigravityForward429RateLimitBody(antigravityForward429ClaudeModel),
	}
	svc := antigravityForward429Service(upstream)
	account := antigravityForward429Account()
	body := antigravityForward429ClaudeBody(t)
	c, _ := antigravityForward429Context(t, "/v1/messages", body)

	started := time.Now()
	result, err := svc.Forward(c.Request.Context(), c, account, body, false)
	elapsed := time.Since(started)

	require.Nil(t, result)
	requireAntigravityForward429Failover(t, err, false)
	require.Equal(t, 1, upstream.calls, "长 retryDelay 限流不应进入同账号重试")
	require.Less(t, elapsed, 2*time.Second, "可识别的长 retryDelay 限流不应进入同账号退避等待")
}

// TestForward_GenuineRateLimit429KeepsStickySessionCacheBilling 验证透传 429 不改变
// 既有的粘性会话强制缓存计费语义。
func TestForward_GenuineRateLimit429KeepsStickySessionCacheBilling(t *testing.T) {
	upstream := &antigravityForward429Upstream{
		status: http.StatusTooManyRequests,
		body:   antigravityForward429RateLimitBody(antigravityForward429ClaudeModel),
	}
	svc := antigravityForward429Service(upstream)
	account := antigravityForward429Account()
	body := antigravityForward429ClaudeBody(t)
	c, _ := antigravityForward429Context(t, "/v1/messages", body)

	_, err := svc.Forward(c.Request.Context(), c, account, body, true /* 粘性会话 */)

	requireAntigravityForward429Failover(t, err, true)
}

// TestForward_PreCooldownSwitchKeeps503 验证 Claude → Gemini 入口的调度前模型限流
// 预检查（切换信号不带上游状态码）仍按既有 503 语义处理，不会被本修改放宽为 429，
// 且在预检查阶段不会向上游发出请求。
func TestForward_PreCooldownSwitchKeeps503(t *testing.T) {
	upstream := &antigravityForward429Upstream{
		status: http.StatusTooManyRequests,
		body:   antigravityForward429RateLimitBody(antigravityForward429ClaudeModel),
	}
	svc := antigravityForward429Service(upstream)
	account := antigravityForward429Account()
	antigravityForward429PreCooldown(account, antigravityForward429ClaudeModel)
	body := antigravityForward429ClaudeBody(t)
	c, _ := antigravityForward429Context(t, "/v1/messages", body)

	result, err := svc.Forward(c.Request.Context(), c, account, body, false)

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "预检查限流必须仍是换号信号")
	require.Equal(t, http.StatusServiceUnavailable, failoverErr.StatusCode,
		"调度前限流预检查不是上游真实 429，必须保持既有 503 语义")
	require.Empty(t, failoverErr.ResponseBody)
	require.Equal(t, 0, upstream.calls, "预检查阶段不应向上游发出请求")
}

// TestForwardAsChatCompletions_GenuineRateLimit429PropagatesFailoverStatus 覆盖 OpenAI
// Chat Completions 兼容入口：上游真实 429 必须透传到请求内 429 账号上限。
func TestForwardAsChatCompletions_GenuineRateLimit429PropagatesFailoverStatus(t *testing.T) {
	upstream := &antigravityForward429Upstream{
		status: http.StatusTooManyRequests,
		body:   antigravityForward429RateLimitBody(antigravityForward429ClaudeModel),
	}
	svc := antigravityForward429Service(upstream)
	account := antigravityForward429Account()
	body := antigravityForward429ChatCompletionsBody(t)
	c, _ := antigravityForward429Context(t, "/v1/chat/completions", body)

	result, err := svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, nil)

	require.Nil(t, result)
	requireAntigravityForward429Failover(t, err, false)
	require.Equal(t, 1, upstream.calls, "长 retryDelay 限流不应进入同账号重试")
}

// TestForwardAsResponses_GenuineRateLimit429PropagatesFailoverStatus 覆盖 OpenAI Responses
// 兼容入口（与 Chat Completions 共用 compat 传输错误处理）。
func TestForwardAsResponses_GenuineRateLimit429PropagatesFailoverStatus(t *testing.T) {
	upstream := &antigravityForward429Upstream{
		status: http.StatusTooManyRequests,
		body:   antigravityForward429RateLimitBody(antigravityForward429ClaudeModel),
	}
	svc := antigravityForward429Service(upstream)
	account := antigravityForward429Account()
	body := antigravityForward429ResponsesBody(t)
	c, _ := antigravityForward429Context(t, "/v1/responses", body)

	result, err := svc.ForwardAsResponses(c.Request.Context(), c, account, body, nil)

	require.Nil(t, result)
	requireAntigravityForward429Failover(t, err, false)
	require.Equal(t, 1, upstream.calls, "长 retryDelay 限流不应进入同账号重试")
}

// TestForwardAsChatCompletions_PreCooldownSwitchKeeps503 验证 OpenAI 兼容入口的调度前
// 模型限流预检查同样保持 503（本修改只透传上游真实 429）。
func TestForwardAsChatCompletions_PreCooldownSwitchKeeps503(t *testing.T) {
	upstream := &antigravityForward429Upstream{
		status: http.StatusTooManyRequests,
		body:   antigravityForward429RateLimitBody(antigravityForward429ClaudeModel),
	}
	svc := antigravityForward429Service(upstream)
	account := antigravityForward429Account()
	antigravityForward429PreCooldown(account, antigravityForward429ClaudeModel)
	body := antigravityForward429ChatCompletionsBody(t)
	c, _ := antigravityForward429Context(t, "/v1/chat/completions", body)

	result, err := svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, nil)

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "预检查限流必须仍是换号信号")
	require.Equal(t, http.StatusServiceUnavailable, failoverErr.StatusCode,
		"调度前限流预检查不是上游真实 429，必须保持既有 503 语义")
	require.Empty(t, failoverErr.ResponseBody)
	require.Equal(t, 0, upstream.calls, "预检查阶段不应向上游发出请求")
}
