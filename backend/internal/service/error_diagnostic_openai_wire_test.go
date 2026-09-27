//go:build unit

package service

// 本文件固定 F2 的范围收口：**入站路由是 /v1/messages，不等于真实上游是 Claude Messages**。
//
// ADR 0005 的 429 头值例外是「Claude Messages 上游 + /v1/messages 路由 + 恰好 429」三个条件
// 缺一不可，而 `/v1/messages` 只是入站路由：同样经它入站的 OpenAI 形态上游（Responses 转换、
// 原始 Chat Completions 兜底）请求头与响应头都是 OpenAI 形态，不满足「Claude Messages 上游」。
// 把 Claude Messages 的闭集白名单套到不相符的 wire 上（Retry-After／X-Request-Id 这类**同名**
// 头在两边都存在）会留下一份被管理员读成「Claude 限流事实」的快照——错误的协议标签比没有标签
// 更危险，因此这一层必须按**未采集**记录。
//
// 出界只影响头值这一层：协议元数据仍按入站路由（/v1/messages）记账，4xx/5xx 正文诊断照旧。

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// openAIChatFallbackWireHeaders 是 OpenAI Chat Completions 上游 429 的真实形态：
// 与 Claude Messages 的白名单**撞名**（Retry-After、X-Request-Id、Content-Type 都在闭集内），
// 因此这条用例才会在修复前真的落下一份「Claude 形态」的头值快照，而不是空快照式的假通过。
func openAIChatFallbackWireHeaders() http.Header {
	return http.Header{
		"Content-Type":                   {"application/json"},
		"Retry-After":                    {"30"},
		"X-Request-Id":                   {"req_openai_wire_canary"},
		"X-Ratelimit-Remaining-Requests": {"0"},
		"X-Ratelimit-Reset-Requests":     {"1s"},
	}
}

// openAIWireDiagnosticRateLimitRepo 只实现本用例的 429 真正会走到的两处写入；
// 其余方法由嵌入的接口占位（不会走到）。
type openAIWireDiagnosticRateLimitRepo struct {
	AccountRepository
	rateLimited    int
	sessionWindows int
}

func (r *openAIWireDiagnosticRateLimitRepo) SetRateLimited(_ context.Context, _ int64, _ time.Time) error {
	r.rateLimited++
	return nil
}

func (r *openAIWireDiagnosticRateLimitRepo) UpdateSessionWindow(_ context.Context, _ int64, _, _ *time.Time, _ string) error {
	r.sessionWindows++
	return nil
}

// /v1/messages 的原始 Chat Completions 兜底：入站是 Messages，真实上游是 OpenAI Chat
// Completions（响应经 ResponsesSupported=false 强制走 raw chat）。429 头值必须记成未采集，
// 且一个 OpenAI 头值都不得进入诊断；正文诊断照旧等于该次真实发送的字节。
func TestMessagesErrorDiagnosticLeavesOpenAIChatFallback429HeaderValuesUnobserved(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 头值与正文两层都开着：修复前正文与头值都会真的落库，因此「头值未采集」的断言
	// 只能由范围判定成立，不能靠开关关着蒙过去。
	settings := newMessagesDiagnosticSettingService(
		errorDiagnosticHeaderValuesSettingsJSON(true, true), nil,
	)
	recorder := &errorDiagnosticRecorderStub{}
	upstream := newMessagesDiagnosticUpstreamStub(messagesDiagnosticUpstreamResponse{
		status: http.StatusTooManyRequests,
		body:   `{"error":{"message":"rate limited","type":"rate_limit_error"}}`,
		header: openAIChatFallbackWireHeaders(),
	})
	svc := messagesDiagnosticNativeService(t, settings, recorder, upstream)
	// 429 会落到限流服务的错误策略判定：替身只需承接它真正会写的两处。
	svc.rateLimitService = &RateLimitService{
		accountRepo:    &openAIWireDiagnosticRateLimitRepo{},
		settingService: settings,
	}

	// 显式关闭 Responses 支持：/v1/messages 走原始 Chat Completions 兜底（真实上游形态是
	// OpenAI Chat Completions，而不是 Claude Messages）。
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: false}

	body := messagesDiagnosticJSONBody()
	_, err := svc.ForwardAsAnthropic(
		context.Background(), adaptiveProtocolTestContext("/v1/messages", body), account, body, "", "",
	)
	require.Error(t, err)

	require.Equal(t, []bool{true}, upstream.observers(), "共享发送器由本分支显式绑定观察者")

	sent := upstream.sent()
	require.Len(t, sent, 1)
	require.Contains(t, string(sent[0]), messagesDiagnosticBodySentinel,
		"上游收到的是转换后的 Chat Completions 正文")

	records := requireMessagesDiagnosticRecords(t, recorder, 1)
	attempt := records[0].attempt

	// 协议元数据仍按入站路由记账：出界只影响头值这一层（与 Bedrock 同一条边界）。
	require.Equal(t, ErrorDiagnosticProtocolMessages, attempt.Protocol,
		"协议元数据按入站 /v1/messages 记账，不跟随上游 wire 形态漂移")
	require.Equal(t, http.StatusTooManyRequests, attempt.UpstreamStatusCode)

	// 真实上游形态不是 Claude Messages：这一层必须是未采集，而不是按 Claude 头值契约采集。
	require.Equal(t, ErrorDiagnosticHeaderVerdictOutOfScope, attempt.HeaderVerdict,
		"OpenAI 形态上游不在 Claude Messages 头值能力范围内，必须记成未采集而不是采集失败")
	require.True(t, attempt.HeaderValues.Empty(), "OpenAI 形态的头值一个都不得进入诊断")
	require.NotContains(t, fmt.Sprint(attempt.HeaderValues), "canary")

	// 出界只影响头值：正文诊断照旧等于该次真实发送的 4xx 字节。
	require.Equal(t, ErrorDiagnosticBodyVerdictComplete, attempt.BodyVerdict)
	require.Equal(t, string(sent[0]), string(records[0].body),
		"诊断正文必须等于该次发送实际发出的字节")
}

// /v1/messages 的 Responses 转换分支（OpenAI 账号的默认路径）：入站同样是 Messages，
// 真实上游却是 OpenAI Responses——请求头与响应头都是 OpenAI 形态。同一条边界：
// 协议元数据按入站路由记 messages，429 头值按未采集记录，正文诊断照旧。
func TestMessagesErrorDiagnosticLeavesOpenAIResponsesWire429HeaderValuesUnobserved(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := newMessagesDiagnosticSettingService(
		errorDiagnosticHeaderValuesSettingsJSON(true, true), nil,
	)
	recorder := &errorDiagnosticRecorderStub{}
	upstream := newMessagesDiagnosticUpstreamStub(messagesDiagnosticUpstreamResponse{
		status: http.StatusTooManyRequests,
		body:   `{"error":{"message":"rate limited","type":"rate_limit_error"}}`,
		header: openAIChatFallbackWireHeaders(),
	})
	svc := messagesDiagnosticNativeService(t, settings, recorder, upstream)
	svc.rateLimitService = &RateLimitService{
		accountRepo:    &openAIWireDiagnosticRateLimitRepo{},
		settingService: settings,
	}

	// 不带 ResponsesSupported=false：OpenAI 账号默认走 Responses 转换分支。
	account := rawChatCompletionsTestAccount()

	body := messagesDiagnosticJSONBody()
	_, err := svc.ForwardAsAnthropic(
		context.Background(), adaptiveProtocolTestContext("/v1/messages", body), account, body, "", "",
	)
	require.Error(t, err)

	sent := upstream.sent()
	require.Len(t, sent, 1)
	// 传输事实先钉住：这条用例覆盖的确实是 Responses wire（body 是 input 形状，
	// 而不是 Chat Completions 的 messages 形状）。
	require.True(t, gjson.GetBytes(sent[0], "input").Exists(), "上游收到的是 Responses 形状正文")
	require.False(t, gjson.GetBytes(sent[0], "messages").Exists())

	records := requireMessagesDiagnosticRecords(t, recorder, 1)
	attempt := records[0].attempt
	require.Equal(t, ErrorDiagnosticProtocolMessages, attempt.Protocol,
		"协议元数据按入站 /v1/messages 记账，不跟随上游 wire 形态漂移")
	require.Equal(t, http.StatusTooManyRequests, attempt.UpstreamStatusCode)
	require.Equal(t, ErrorDiagnosticHeaderVerdictOutOfScope, attempt.HeaderVerdict,
		"OpenAI 形态上游不在 Claude Messages 头值能力范围内，必须记成未采集")
	require.True(t, attempt.HeaderValues.Empty(), "OpenAI 形态的头值一个都不得进入诊断")
	require.NotContains(t, fmt.Sprint(attempt.HeaderValues), "canary")

	require.Equal(t, ErrorDiagnosticBodyVerdictComplete, attempt.BodyVerdict)
	require.Equal(t, string(sent[0]), string(records[0].body),
		"诊断正文必须等于该次发送实际发出的字节")
}
