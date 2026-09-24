//go:build unit

package service

// /v1/responses 入站两条「原生发送」路径的上游错误诊断接缝测试（契约见
// docs/adr/0005-short-lived-error-body-diagnostics-scope.md）。
//
// 与同目录 error_diagnostic_responses_branch_test.go 互补而不重复：那边覆盖 Forward 的
// Anthropic 交叉协议转换、raw chat 回退与 Grok 子分支，本文件只覆盖两条**不做协议转换**的
// 发送路径：
//
//   - Forward 的原生 Responses 循环（openai_gateway_forward.go 里带内部重试的 for{}），
//     上游真的就是 /responses，出站正文即入站 Responses 形态。
//   - Forward 的自动透传分支（forwardOpenAIPassthrough），出站正文基本就是入站报文本身。
//
// 这两条路径都不在发送点绑定观察者，而是在 Forward 的分流点把观察者绑在**局部 ctx** 上
// （bindResponsesErrorDiagnosticBranch），由 buildUpstreamRequest /
// buildUpstreamRequestOpenAIPassthrough 经 http.NewRequestWithContext 带下去，
// detachUpstreamContext 用 context.WithoutCancel 保留该值。因此本文件断言的是「ctx 级绑定
// 确实穿过了这两条发送」，而不是某个发送点的私有绑定；同时断言观察者没有落到 gin 请求上下文
// 上（否则同进程里的 WS／插件／辅助请求会被误采集）。
//
// 正文比对针对替身按 repository/http_upstream.go 真实接缝顺序交给观察者的字节（即传输层
// 真正读走的出站正文），不是入站 JSON。默认关闭的断言同样覆盖这两条路径：门控未开启或接缝
// 未注入时既不写诊断，也不在出站请求上留下观察者。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// errorDiagnosticPassthroughMarkerKey 是 forwardOpenAIPassthrough 标记「本次发送走了自动透传」
// 的 gin 上下文键（见 openai_gateway_passthrough.go: c.Set("openai_passthrough", true)）。
//
// 断言它而不是只靠账号 Extra 推断，是为了让「确实覆盖到透传分支」成为事实：若 Forward 的分流
// 被改动（例如落回原生 Responses 循环），这条用例会立刻失败，而不是静默变成一条重复用例。
const errorDiagnosticPassthroughMarkerKey = "openai_passthrough"

// newErrorDiagnosticResponsesForwardContext 造一个 /openai/v1/responses 的入站请求。
// 路径与真实 OpenAI 路由一致（非 compact 形态），使 Forward 走正常分流而不是 compact 分支。
func newErrorDiagnosticResponsesForwardContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	out := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(out)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	return c, out
}

// errorDiagnosticNativeResponsesAccount 是走 Forward 原生 Responses 循环的账号：
// 国产供应商 + 显式 responses 协议，因此 shouldForwardOpenAIResponsesViaRawChatCompletions
// 为 false、IsAnthropicProtocol 为 false，落点就是带内部重试的原生 /responses 发送点。
func errorDiagnosticNativeResponsesAccount() *Account {
	return &Account{
		ID:       61,
		Name:     "cn-native-responses-account",
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "deepseek-diagnostic-test",
			"api_protocol": APIProtocolResponses,
			"base_url":     "https://api.deepseek.test",
		},
	}
}

// errorDiagnosticPassthroughAccount 是开启「自动透传（仅替换认证）」的 OpenAI API Key 账号：
// Forward 因此走 forwardOpenAIPassthrough，出站正文按原样代理。
func errorDiagnosticPassthroughAccount() *Account {
	return &Account{
		ID:       62,
		Name:     "openai-passthrough-account",
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"openai_passthrough": true},
		Credentials: map[string]any{
			"api_key":  "sk-passthrough-diagnostic-test",
			"base_url": "https://api.openai.test",
		},
	}
}

// TestResponsesNativeLoopErrorDiagnostic_ForwardBindsObserverOnBranchContext 固定原生 Responses
// 循环的接缝事实：Forward 的分流点绑定的观察者必须穿过 buildUpstreamRequest 到达真实发送，
// 上游 5xx 因此留下一条 responses 诊断，且管理员取到的正文逐字节等于传输层读走的出站字节。
func TestResponsesNativeLoopErrorDiagnostic_ForwardBindsObserverOnBranchContext(t *testing.T) {
	upstream := newErrorDiagnosticResponsesUpstream(
		errorDiagnosticResponsesFailure(http.StatusInternalServerError, "native-loop-fail"),
	)
	gateway, recorder, service := newErrorDiagnosticOpenAIGatewayHarness(t, enabledErrorDiagnostics(), upstream)
	c, _ := newErrorDiagnosticResponsesForwardContext(t)

	body := []byte(`{"model":"deepseek-chat","input":"native responses loop"}`)

	_, err := gateway.Forward(context.Background(), c, errorDiagnosticNativeResponsesAccount(), body)
	require.Error(t, err)
	require.Equal(t, 1, upstream.callCount(), "the native responses loop must really reach the upstream")
	require.Equal(t, openAIResponsesUpstreamEndpoint, GetActualOpenAIUpstreamEndpoint(c),
		"the fixture must be routed to the native /responses send point, not the raw chat fallback")

	attempts := recorder.waitForAttempts(t, 1)
	require.Len(t, attempts, 1, "only the real 5xx send may produce a diagnostic")
	attempt := attempts[0]
	require.Equal(t, ErrorDiagnosticProtocolResponses, attempt.Protocol,
		"the diagnostic protocol is the inbound branch, not derived from the wire path")
	require.Equal(t, ErrorDiagnosticStageWire, attempt.Stage)
	require.Equal(t, http.StatusInternalServerError, attempt.UpstreamStatusCode)
	require.Equal(t, 1, attempt.AttemptIndex)
	require.True(t, attempt.BodyReadComplete)
	require.Equal(t, ErrorDiagnosticBodyVerdictComplete, attempt.BodyVerdict)
	require.Zero(t, attempt.UsageLogID, "phase 1 keeps diagnostics independent of usage rows")
	require.True(t, upstream.boundObservers()[0],
		"the branch-scoped observer must be carried onto the native /responses send")

	records := recorder.recordsSnapshot()
	require.Len(t, records, 1)
	require.Equal(t, ErrorDiagnosticBodyStateStored, records[0].BodyState)
	require.Equal(t, ErrorDiagnosticBodyRetained, records[0].BodyReason)

	sent := upstream.sentBodies()
	require.Len(t, sent, 1)
	stored, readErr := service.ReadErrorDiagnosticBody(context.Background(), records[0].ID)
	require.NoError(t, readErr)
	require.Equal(t, string(sent[0]), string(stored),
		"diagnostic body must be the bytes the transport consumed")
	require.NotEqual(t, string(body), string(stored),
		"the native CN responses send is not a byte-for-byte copy of the inbound body")
	require.Contains(t, string(stored), "native responses loop")

	_, leaked := httpattempt.DiagnosticObserverFromContext(c.Request.Context())
	require.False(t, leaked, "the observer must stay on the branch context, never on the gin request context")
	recorder.requireNoAttemptsAfter(t, 1)
	require.Equal(t, 1, upstream.callCount(), "no auxiliary send may be introduced by the diagnostic seam")
}

// TestResponsesPassthroughErrorDiagnostic_ForwardBindsObserverOnBranchContext 固定自动透传分支的
// 接缝事实：透传在分流点之后才被派发，因此它同样命中 ctx 级绑定；上游 4xx 留下一条 responses
// 诊断，正文逐字节等于透传实际发出的报文。
func TestResponsesPassthroughErrorDiagnostic_ForwardBindsObserverOnBranchContext(t *testing.T) {
	upstream := newErrorDiagnosticResponsesUpstream(
		errorDiagnosticResponsesFailure(http.StatusBadRequest, "passthrough-fail"),
	)
	gateway, recorder, service := newErrorDiagnosticOpenAIGatewayHarness(t, enabledErrorDiagnostics(), upstream)
	c, _ := newErrorDiagnosticResponsesForwardContext(t)

	body := []byte(`{"model":"gpt-5.4","input":"responses passthrough"}`)

	_, err := gateway.Forward(context.Background(), c, errorDiagnosticPassthroughAccount(), body)
	require.Error(t, err)
	require.Equal(t, 1, upstream.callCount(), "the passthrough branch must really reach the upstream")
	require.Equal(t, openAIResponsesUpstreamEndpoint, GetActualOpenAIUpstreamEndpoint(c),
		"the fixture must be routed to the /responses send point, not the raw chat fallback")
	marked, hasMarker := c.Get(errorDiagnosticPassthroughMarkerKey)
	require.True(t, hasMarker, "Forward must dispatch the passthrough account to forwardOpenAIPassthrough")
	require.Equal(t, true, marked)

	attempts := recorder.waitForAttempts(t, 1)
	require.Len(t, attempts, 1, "only the real 4xx send may produce a diagnostic")
	attempt := attempts[0]
	require.Equal(t, ErrorDiagnosticProtocolResponses, attempt.Protocol,
		"passthrough is dispatched after the branch bind, so it keeps the inbound protocol")
	require.Equal(t, http.StatusBadRequest, attempt.UpstreamStatusCode)
	require.Equal(t, 1, attempt.AttemptIndex)
	require.Equal(t, ErrorDiagnosticBodyVerdictComplete, attempt.BodyVerdict)
	require.True(t, upstream.boundObservers()[0],
		"the branch-scoped observer must be carried onto the passthrough send")

	records := recorder.recordsSnapshot()
	require.Len(t, records, 1)
	require.False(t, records[0].HasUsage)

	sent := upstream.sentBodies()
	require.Len(t, sent, 1)
	stored, readErr := service.ReadErrorDiagnosticBody(context.Background(), records[0].ID)
	require.NoError(t, readErr)
	require.Equal(t, string(sent[0]), string(stored),
		"diagnostic body must be the bytes the transport consumed")
	require.Contains(t, string(stored), "responses passthrough")

	_, leaked := httpattempt.DiagnosticObserverFromContext(c.Request.Context())
	require.False(t, leaked, "the observer must stay on the branch context, never on the gin request context")
	recorder.requireNoAttemptsAfter(t, 1)
	require.Equal(t, 1, upstream.callCount(), "no auxiliary send may be introduced by the diagnostic seam")
}

// TestResponsesNativeAndPassthroughDiagnostics_AreDefaultOff 固定默认关闭：这两条路径都必须
// 显式 opt-in。门控未开启（ErrorDiagnosticSettings 零值）或接缝未注入时，既不写任何诊断，
// 也不在出站请求上留下观察者——上游照常收到请求并返回 5xx。
func TestResponsesNativeAndPassthroughDiagnostics_AreDefaultOff(t *testing.T) {
	disabled := ErrorDiagnosticSettings{}

	cases := []struct {
		name    string
		account *Account
		body    []byte
	}{
		{
			name:    "native responses loop",
			account: errorDiagnosticNativeResponsesAccount(),
			body:    []byte(`{"model":"deepseek-chat","input":"native loop default off"}`),
		},
		{
			name:    "responses passthrough",
			account: errorDiagnosticPassthroughAccount(),
			body:    []byte(`{"model":"gpt-5.4","input":"passthrough default off"}`),
		},
	}

	for _, tc := range cases {
		t.Run("capture gate disabled/"+tc.name, func(t *testing.T) {
			upstream := newErrorDiagnosticResponsesUpstream(
				errorDiagnosticResponsesFailure(http.StatusInternalServerError, "default-off-fail"),
			)
			gateway, recorder, _ := newErrorDiagnosticOpenAIGatewayHarness(t, disabled, upstream)
			c, _ := newErrorDiagnosticResponsesForwardContext(t)

			_, err := gateway.Forward(context.Background(), c, tc.account, tc.body)
			require.Error(t, err)
			require.Equal(t, 1, upstream.callCount(), "the gate must not change the upstream traffic")

			recorder.requireNoAttempts(t)
			for i, bound := range upstream.boundObservers() {
				require.False(t, bound, "attempt %d must not carry an observer while the gate is closed", i)
			}
		})

		t.Run("no recorder injected/"+tc.name, func(t *testing.T) {
			upstream := newErrorDiagnosticResponsesUpstream(
				errorDiagnosticResponsesFailure(http.StatusInternalServerError, "no-seam-fail"),
			)
			// 门控打开但接缝未注入：这是生产默认态，绑定必须 fail closed。
			gateway, _, _ := newErrorDiagnosticOpenAIGatewayHarness(t, enabledErrorDiagnostics(), upstream)
			gateway.SetErrorDiagnosticRecorder(nil)
			require.Nil(t, gateway.errorDiagnostics, "no recorder means the seam stays closed")

			c, _ := newErrorDiagnosticResponsesForwardContext(t)
			_, err := gateway.Forward(context.Background(), c, tc.account, tc.body)
			require.Error(t, err)
			require.Equal(t, 1, upstream.callCount())

			for i, bound := range upstream.boundObservers() {
				require.False(t, bound, "attempt %d must not carry an observer without an injected seam", i)
			}
			_, leaked := httpattempt.DiagnosticObserverFromContext(c.Request.Context())
			require.False(t, leaked)
		})
	}
}
