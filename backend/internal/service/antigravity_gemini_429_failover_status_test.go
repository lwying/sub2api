//go:build unit

package service

// Ticket 03 — Antigravity「Gemini 转换」入口（ForwardGemini）的换号状态码契约。
//
// ForwardGemini 把服务层的账号切换信号转成 UpstreamFailoverError 交给 handler，
// handler 的「请求内 429 账号上限」只统计 StatusCode == 429 的失败
// （handler/request_429_account_limit.go）。因此上游真实返回 429 触发换号时，
// 状态码必须原样透传，否则上限永不触顶：A、B 各自 429 后仍会继续尝试 C。
//
// 反向约束同样重要：非 429 的切换原因（503 模型容量、调度前的模型限流预检查、
// 策略触发的临时不可调度）必须保持既有的 503 语义，不能因为本修改被放宽为 429。
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

const antigravityGemini429StatusModel = "gemini-2.5-pro"

// antigravityGemini429StatusRateLimitBody 是可识别的账号级限流响应：
// reason=RATE_LIMIT_EXCEEDED + metadata.model + retryDelay 15s（> 7s 阈值）。
const antigravityGemini429StatusRateLimitBody = `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"rate limited","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"gemini-2.5-pro"}},{"@type":"google.rpc.RetryInfo","retryDelay":"15s"}]}}`

// antigravityGemini429StatusUpstream 恒定返回同一个上游响应，并记录调用次数，
// 用于同时断言「换号信号的状态码」与「没有进入同账号重试」。
type antigravityGemini429StatusUpstream struct {
	HTTPUpstream
	status int
	body   string
	calls  int
}

func (u *antigravityGemini429StatusUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return u.respond(), nil
}

func (u *antigravityGemini429StatusUpstream) DoWithTLS(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
	return u.respond(), nil
}

func (u *antigravityGemini429StatusUpstream) respond() *http.Response {
	u.calls++
	return &http.Response{
		StatusCode: u.status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(u.body)),
	}
}

// antigravityGemini429StatusService 组装真实 AntigravityGatewayService，仅注入 stub 上游。
// 凭据自带未过期的 access_token 与 project_id，配合 (nil,nil,nil) 的 token provider
// 不会触发刷新网络调用，也不会走 credits/overages 分支。
func antigravityGemini429StatusService(upstream HTTPUpstream) *AntigravityGatewayService {
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

func antigravityGemini429StatusAccount() *Account {
	return &Account{
		ID:          1,
		Name:        "antigravity-gemini-429",
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

// antigravityGemini429StatusContext 构造最小可用的 gin 上下文：只提供请求与响应写入器，
// 不依赖 handler 层的中间件键。
func antigravityGemini429StatusContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(
		http.MethodPost,
		"/antigravity/v1beta/models/"+antigravityGemini429StatusModel+":generateContent",
		bytes.NewBufferString(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	return c, rec
}

func antigravityGemini429StatusRequestBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]any{{"text": "hello"}}},
		},
	})
	require.NoError(t, err)
	return body
}

// TestForwardGemini_GenuineRateLimit429PropagatesFailoverStatus 验证上游真实限流 429
// 触发换号时，换号信号按 429 透传（而不是折叠成 503），且不进入同账号退避等待。
func TestForwardGemini_GenuineRateLimit429PropagatesFailoverStatus(t *testing.T) {
	upstream := &antigravityGemini429StatusUpstream{
		status: http.StatusTooManyRequests,
		body:   antigravityGemini429StatusRateLimitBody,
	}
	svc := antigravityGemini429StatusService(upstream)
	account := antigravityGemini429StatusAccount()
	c, _ := antigravityGemini429StatusContext(t)

	started := time.Now()
	result, err := svc.ForwardGemini(
		c.Request.Context(), c, account, antigravityGemini429StatusModel, "generateContent", false,
		antigravityGemini429StatusRequestBody(t), false,
	)
	elapsed := time.Since(started)

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "换号信号必须转成 UpstreamFailoverError")
	require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode,
		"上游真实 429 必须透传 429，否则请求内 429 账号上限无法计入该账号")
	require.False(t, failoverErr.ForceCacheBilling, "非粘性会话不应强制缓存计费")
	require.Equal(t, 1, upstream.calls, "长 retryDelay 限流不应进入同账号重试")
	require.Less(t, elapsed, 2*time.Second, "可识别的长 retryDelay 限流不应进入同账号退避等待")
}

// TestForwardGemini_RateLimit429KeepsStickySessionCacheBilling 验证透传 429 不改变
// 既有的粘性会话强制缓存计费语义。
func TestForwardGemini_RateLimit429KeepsStickySessionCacheBilling(t *testing.T) {
	upstream := &antigravityGemini429StatusUpstream{
		status: http.StatusTooManyRequests,
		body:   antigravityGemini429StatusRateLimitBody,
	}
	svc := antigravityGemini429StatusService(upstream)
	account := antigravityGemini429StatusAccount()
	c, _ := antigravityGemini429StatusContext(t)

	_, err := svc.ForwardGemini(
		c.Request.Context(), c, account, antigravityGemini429StatusModel, "generateContent", false,
		antigravityGemini429StatusRequestBody(t), true, // 粘性会话
	)

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
	require.True(t, failoverErr.ForceCacheBilling, "粘性会话切换应保持强制缓存计费")
}

// TestAntigravitySwitchFailoverStatusCode 固化状态码映射表：只有上游真实 429 透传 429，
// 其余切换原因（503 容量 / 调度前限流预检查 / 无上游状态的策略切换）保持既有 503。
func TestAntigravitySwitchFailoverStatusCode(t *testing.T) {
	cases := []struct {
		name      string
		switchErr *AntigravityAccountSwitchError
		want      int
	}{
		{
			name:      "upstream 429 rate limit",
			switchErr: &AntigravityAccountSwitchError{UpstreamStatusCode: http.StatusTooManyRequests},
			want:      http.StatusTooManyRequests,
		},
		{
			name:      "upstream 503 model capacity",
			switchErr: &AntigravityAccountSwitchError{UpstreamStatusCode: http.StatusServiceUnavailable},
			want:      http.StatusServiceUnavailable,
		},
		{
			name:      "pre-check switch without upstream response",
			switchErr: &AntigravityAccountSwitchError{},
			want:      http.StatusServiceUnavailable,
		},
		{
			name:      "nil switch error",
			switchErr: nil,
			want:      http.StatusServiceUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, antigravitySwitchFailoverStatusCode(tc.switchErr))
		})
	}
}
