//go:build unit

package service

// 评审发现 G2 — 同账号智能重试耗尽后的换号信号必须携带「最后一次上游尝试」的状态码。
//
// handleSmartRetry 在智能重试耗尽返回 AntigravityAccountSwitchError 时，用初始响应
// resp.StatusCode 填充 UpstreamStatusCode；而同账号重试只在 429/503 之间反复，
// 最后一次尝试的状态码可能与初始不同。上层 antigravitySwitchFailoverStatusCode
// 仅在 UpstreamStatusCode == 429 时透传 429，handler 的「请求内 429 账号上限」
// 也只统计 StatusCode == 429 的失败（handler/request_429_account_limit.go），
// 因此初始状态码会错误地决定该账号是否计入本次逻辑请求的 429 额度：
//
//   - 初始 503（限流正文）+ 重试真实 429 耗尽 → 必须按 429 透传，
//     否则该账号真实的 429 结果不会被计入 429 账号上限；
//   - 初始 429 + 重试 503 耗尽 → 必须回到既有 503 语义，5xx 不占 429 额度；
//   - 初始 503 + 重试 503 耗尽 → 保持既有 503 语义不变。
//
// 场景可达性：HTTP 状态码与正文 error.status 相互独立，"503 + RESOURCE_EXHAUSTED
// + RATE_LIMIT_EXCEEDED + retryDelay 0.1s（< antigravityRateLimitThreshold 7s）"
// 会走 shouldSmartRetry 分支；antigravitySmartRetryMaxAttempts 为 1，因此只发生
// 一次同账号重试（等待被钳制为 antigravitySmartRetryMinWait，约 1s）。

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// antigravityRetrySwitchStatusBody 构造 Antigravity 能识别的账号级限流正文：
// error.status=RESOURCE_EXHAUSTED + reason=RATE_LIMIT_EXCEEDED + metadata.model
// + retryDelay 0.1s（< 7s 阈值，触发同账号智能重试而非直接限流换号）。
// 正文内容与 HTTP 状态码无关，因此可用于 503 与 429 两种响应。
func antigravityRetrySwitchStatusBody(code int, modelName string) []byte {
	return []byte(fmt.Sprintf(
		`{"error":{"code":%d,"status":"RESOURCE_EXHAUSTED","message":"rate limited","details":[`+
			`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","metadata":{"model":%q},"reason":"RATE_LIMIT_EXCEEDED"},`+
			`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.1s"}]}}`,
		code, modelName,
	))
}

// TestAntigravitySmartRetryExhausted_SwitchStatusFollowsTerminalUpstreamAttempt
// 固化换号信号的状态码来源：智能重试耗尽后，UpstreamStatusCode 必须等于最后一次
// 上游尝试的状态码，而不是初始响应的状态码。
func TestAntigravitySmartRetryExhausted_SwitchStatusFollowsTerminalUpstreamAttempt(t *testing.T) {
	const modelName = "gemini-3-pro"

	cases := []struct {
		name             string
		initialStatus    int
		retryStatus      int
		wantSwitchStatus int
	}{
		{
			name:             "initial 503 then genuine 429 exhaustion propagates 429",
			initialStatus:    http.StatusServiceUnavailable,
			retryStatus:      http.StatusTooManyRequests,
			wantSwitchStatus: http.StatusTooManyRequests,
		},
		{
			name:             "initial 429 then 503 exhaustion keeps 503",
			initialStatus:    http.StatusTooManyRequests,
			retryStatus:      http.StatusServiceUnavailable,
			wantSwitchStatus: http.StatusServiceUnavailable,
		},
		{
			name:             "initial 503 then 503 exhaustion keeps 503",
			initialStatus:    http.StatusServiceUnavailable,
			retryStatus:      http.StatusServiceUnavailable,
			wantSwitchStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newResp := func(status int) *http.Response {
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(bytes.NewReader(antigravityRetrySwitchStatusBody(status, modelName))),
				}
			}
			upstream := &mockSmartRetryUpstream{
				responses: []*http.Response{newResp(tc.initialStatus), newResp(tc.retryStatus)},
				errors:    []error{nil, nil},
			}

			svc := &AntigravityGatewayService{}
			result, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
				ctx:          context.Background(),
				prefix:       "[test]",
				account:      retrySwitchStatusAccount(),
				accessToken:  "token",
				action:       "generateContent",
				body:         []byte(`{"input":"test"}`),
				httpUpstream: upstream,
				accountRepo:  &stubAntigravityAccountRepo{},
				handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
					return nil
				},
			})

			require.Nil(t, result, "换号信号不应返回响应结果")
			var switchErr *AntigravityAccountSwitchError
			require.ErrorAs(t, err, &switchErr, "智能重试耗尽应返回换号信号")
			require.Len(t, upstream.calls, 2, "应有一次初始请求和一次同账号智能重试")
			require.Equal(t, modelName, switchErr.RateLimitedModel)

			require.Equal(t, tc.wantSwitchStatus, switchErr.UpstreamStatusCode,
				"换号信号必须携带最后一次上游尝试的状态码")
			require.Equal(t, tc.wantSwitchStatus, antigravitySwitchFailoverStatusCode(switchErr),
				"请求内 429 账号上限只统计透传为 429 的失败：终态 429 必须透传 429，终态 503 必须保持既有 503 语义")
		})
	}
}

func retrySwitchStatusAccount() *Account {
	return &Account{
		ID:          21,
		Name:        "acc-21",
		Type:        AccountTypeOAuth,
		Platform:    PlatformAntigravity,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}
}
