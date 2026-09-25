//go:build unit

package service

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/stretchr/testify/require"
)

// 本文件固定 429 头值留存的持久化边界（ADR 0005 的头值例外）：
//   - 只有 Messages + 恰好 429 在范围内，其它协议与状态一律「未采集」；
//   - 只有闭集白名单内的头名与有界取值能落库，凭据类头一个值都不留；
//   - 整份快照要么全部通过、要么整份丢弃，绝不部分写入；
//   - 正文开关、usage 关联、密钥可用性各自独立，互不代替。

func headerScopeAttempt(protocol string, status int) ErrorDiagnosticAttempt {
	return ErrorDiagnosticAttempt{
		Protocol:           protocol,
		Stage:              ErrorDiagnosticStageWire,
		AttemptIndex:       1,
		UpstreamStatusCode: status,
		HeaderVerdict:      ErrorDiagnosticHeaderVerdictCaptured,
		HeaderValues: ErrorDiagnosticHeaderValues{
			Response: map[string]string{"Retry-After": "42"},
		},
	}
}

func TestErrorDiagnosticHeaderDefaultClaudeHeadersSurviveStorageBoundary(t *testing.T) {
	requestHeaders := make(http.Header)
	for name, value := range claude.DefaultHeaders() {
		requestHeaders.Set(name, value)
	}
	responseHeaders := http.Header{"Retry-After": {"42"}, "Anthropic-Ratelimit-Requests-Reset": {"2026-09-24T14:01:30Z"}}
	values, ok := ErrorDiagnosticHeaderValuesFromSanitized(
		httpattempt.SanitizeClaudeRequestHeaderValues(requestHeaders),
		httpattempt.SanitizeClaudeResponseHeaderValues(responseHeaders),
	)
	require.True(t, ok)
	attempt := headerScopeAttempt(ErrorDiagnosticProtocolMessages, http.StatusTooManyRequests)
	attempt.HeaderValues = values
	decision := DecideErrorDiagnosticHeaderValues(attempt, true, true)
	require.Equal(t, ErrorDiagnosticHeaderStateStored, decision.State)
	require.Equal(t, "true", values.Request["Anthropic-Dangerous-Direct-Browser-Access"])
	require.Equal(t, "42", values.Response["Retry-After"])
}

// 范围内判定只认 Messages + 429，与正文诊断的覆盖范围（三协议全部 4xx/5xx）无关。
func TestErrorDiagnosticHeaderScopeIsMessagesOnlyAnd429Only(t *testing.T) {
	require.True(t, ErrorDiagnosticHeaderScopeApplies(ErrorDiagnosticProtocolMessages, http.StatusTooManyRequests))
	for _, tc := range []struct {
		name     string
		protocol string
		status   int
	}{
		{name: "chat completions 429", protocol: ErrorDiagnosticProtocolChatCompletions, status: http.StatusTooManyRequests},
		{name: "responses 429", protocol: ErrorDiagnosticProtocolResponses, status: http.StatusTooManyRequests},
		{name: "messages 500", protocol: ErrorDiagnosticProtocolMessages, status: http.StatusInternalServerError},
		{name: "messages 403", protocol: ErrorDiagnosticProtocolMessages, status: http.StatusForbidden},
		{name: "unknown protocol 429", protocol: "gemini", status: http.StatusTooManyRequests},
	} {
		require.False(t, ErrorDiagnosticHeaderScopeApplies(tc.protocol, tc.status), tc.name)

		// 越界时即使调用方夹带了值，也必须按未采集处理且一个字节都不保留。
		decision := DecideErrorDiagnosticHeaderValues(headerScopeAttempt(tc.protocol, tc.status), true, true)
		require.Equal(t, ErrorDiagnosticHeaderStateNotObserved, decision.State, tc.name)
		require.Equal(t, ErrorDiagnosticHeaderNotObserved, decision.Reason, tc.name)
		require.Nil(t, decision.Payload, tc.name)
	}
}

// 正文开关与密钥可用性各自独立地决定头值结论。
func TestErrorDiagnosticHeaderDecisionDependsOnItsOwnGateAndKey(t *testing.T) {
	attempt := headerScopeAttempt(ErrorDiagnosticProtocolMessages, http.StatusTooManyRequests)

	stored := DecideErrorDiagnosticHeaderValues(attempt, true, true)
	require.Equal(t, ErrorDiagnosticHeaderStateStored, stored.State)
	require.Equal(t, ErrorDiagnosticHeaderRetained, stored.Reason)
	require.Equal(t, 1, stored.EntryCount)
	require.True(t, stored.Retained())
	require.NotEmpty(t, stored.Payload)

	// 头值开关关闭：跳过，且不得留下载荷。
	disabled := DecideErrorDiagnosticHeaderValues(attempt, false, true)
	require.Equal(t, ErrorDiagnosticHeaderStateSkipped, disabled.State)
	require.Equal(t, ErrorDiagnosticHeaderSkippedRetentionDisabled, disabled.Reason)
	require.Nil(t, disabled.Payload)

	// 缺密钥：跳过，永不回退为明文。
	noKey := DecideErrorDiagnosticHeaderValues(attempt, true, false)
	require.Equal(t, ErrorDiagnosticHeaderStateSkipped, noKey.State)
	require.Equal(t, ErrorDiagnosticHeaderSkippedEncryptionUnavailable, noKey.Reason)
	require.Nil(t, noKey.Payload)

	// 封闭抑制结论（要求过、但绑定时无密钥）优先于「没要求」，因为它描述的是配置故障。
	suppressed := attempt
	suppressed.HeaderVerdict = ErrorDiagnosticHeaderVerdictSuppressedEncryptionUnavailable
	suppressed.HeaderValues = ErrorDiagnosticHeaderValues{}
	suppressedDecision := DecideErrorDiagnosticHeaderValues(suppressed, true, false)
	require.Equal(t, ErrorDiagnosticHeaderStateSkipped, suppressedDecision.State)
	require.Equal(t, ErrorDiagnosticHeaderSkippedEncryptionUnavailable, suppressedDecision.Reason)

	// 未知结论一律不合格，绝不默认留存。
	unknown := attempt
	unknown.HeaderVerdict = "something_new"
	unknownDecision := DecideErrorDiagnosticHeaderValues(unknown, true, true)
	require.Equal(t, ErrorDiagnosticHeaderStateSkipped, unknownDecision.State)
	require.Equal(t, ErrorDiagnosticHeaderSkippedInvalidValues, unknownDecision.Reason)
	require.Nil(t, unknownDecision.Payload)
}

// 「没要求」「范围内但一条都没有」是不同的事实：前者是 skipped，后者是未采集。
func TestErrorDiagnosticHeaderDecisionSeparatesNotRequestedFromEmpty(t *testing.T) {
	attempt := headerScopeAttempt(ErrorDiagnosticProtocolMessages, http.StatusTooManyRequests)

	notRequested := attempt
	notRequested.HeaderVerdict = ErrorDiagnosticHeaderVerdictNotRequested
	notRequested.HeaderValues = ErrorDiagnosticHeaderValues{}
	decision := DecideErrorDiagnosticHeaderValues(notRequested, true, true)
	require.Equal(t, ErrorDiagnosticHeaderStateSkipped, decision.State)
	require.Equal(t, ErrorDiagnosticHeaderSkippedRetentionDisabled, decision.Reason)

	empty := attempt
	empty.HeaderVerdict = ErrorDiagnosticHeaderVerdictEmpty
	empty.HeaderValues = ErrorDiagnosticHeaderValues{}
	emptyDecision := DecideErrorDiagnosticHeaderValues(empty, true, true)
	require.Equal(t, ErrorDiagnosticHeaderStateNotObserved, emptyDecision.State)
	require.Equal(t, ErrorDiagnosticHeaderNotObserved, emptyDecision.Reason)

	// 声称 captured 却一条都没通过：同样按未采集记，不谎称已跳过。
	capturedButEmpty := attempt
	capturedButEmpty.HeaderValues = ErrorDiagnosticHeaderValues{}
	capturedDecision := DecideErrorDiagnosticHeaderValues(capturedButEmpty, true, true)
	require.Equal(t, ErrorDiagnosticHeaderStateNotObserved, capturedDecision.State)
}

// 传输层因为净化器省略（未知头名或没通过取值校验的取值）而整份不交出取值时，
// 服务侧必须得到稳定原因码 skipped_invalid_values：既不是「未采集」，也绝不落任何取值。
func TestErrorDiagnosticHeaderOmittedTransportVerdictIsSkippedWhole(t *testing.T) {
	verdict := errorDiagnosticHeaderVerdict(httpattempt.DiagnosticHeaderOmitted)
	require.Equal(t, ErrorDiagnosticHeaderVerdictInvalidValues, verdict,
		"传输层无法为这批取值背书时，服务侧的结论是 invalid_values")
	require.True(t, ErrorDiagnosticHeaderVerdictAllowed(verdict))

	attempt := headerScopeAttempt(ErrorDiagnosticProtocolMessages, http.StatusTooManyRequests)
	attempt.HeaderVerdict = verdict
	attempt.HeaderValues = ErrorDiagnosticHeaderValues{}
	decision := DecideErrorDiagnosticHeaderValues(attempt, true, true)
	require.Equal(t, ErrorDiagnosticHeaderStateSkipped, decision.State)
	require.Equal(t, ErrorDiagnosticHeaderSkippedInvalidValues, decision.Reason)
	require.Nil(t, decision.Payload)

	// 即使调用方违规夹带了半份取值，也必须整份丢弃，绝不部分留存。
	smuggled := attempt
	smuggled.HeaderValues = ErrorDiagnosticHeaderValues{Response: map[string]string{"Retry-After": "42"}}
	smuggledDecision := DecideErrorDiagnosticHeaderValues(smuggled, true, true)
	require.Equal(t, ErrorDiagnosticHeaderStateSkipped, smuggledDecision.State)
	require.Equal(t, ErrorDiagnosticHeaderSkippedInvalidValues, smuggledDecision.Reason)
	require.Nil(t, smuggledDecision.Payload)
}

// 上游形态不在头值能力范围内时（例如 Bedrock），入站路由与状态码看起来符合也不算采集：
// 它按领域词汇记作**未采集**，不是跳过，也不新增「skipped_out_of_scope」这类原因码。
func TestErrorDiagnosticHeaderOutOfScopeUpstreamFormIsNotObserved(t *testing.T) {
	require.True(t, ErrorDiagnosticHeaderVerdictAllowed(ErrorDiagnosticHeaderVerdictOutOfScope))

	attempt := headerScopeAttempt(ErrorDiagnosticProtocolMessages, http.StatusTooManyRequests)
	attempt.HeaderVerdict = ErrorDiagnosticHeaderVerdictOutOfScope
	decision := DecideErrorDiagnosticHeaderValues(attempt, true, true)
	require.Equal(t, ErrorDiagnosticHeaderStateNotObserved, decision.State)
	require.Equal(t, ErrorDiagnosticHeaderNotObserved, decision.Reason)
	require.Nil(t, decision.Payload)

	// 出界结论同样整份丢弃夹带进来的取值：出界不是「可以少留一点」。
	smuggled := attempt
	smuggled.HeaderValues = ErrorDiagnosticHeaderValues{Response: map[string]string{"Retry-After": "42"}}
	smuggledDecision := DecideErrorDiagnosticHeaderValues(smuggled, true, true)
	require.Equal(t, ErrorDiagnosticHeaderStateNotObserved, smuggledDecision.State)
	require.Equal(t, ErrorDiagnosticHeaderNotObserved, smuggledDecision.Reason)
	require.Nil(t, smuggledDecision.Payload)
}

// 白名单是闭集：未知头名、凭据类头名与自由字段都不得落库。
func TestNormalizeErrorDiagnosticHeaderValuesKeepsOnlyAllowlistedNames(t *testing.T) {
	normalized, count, ok := NormalizeErrorDiagnosticHeaderValues(ErrorDiagnosticHeaderValues{
		Request: map[string]string{
			"Anthropic-Version": "2023-06-01",
			"User-Agent":        "claude-cli/2.1.78 (external, cli)",
			"X-App":             "claude-code",
		},
		Response: map[string]string{
			"Retry-After": "42",
			"Request-Id":  "req_123-abc",
		},
	})
	require.True(t, ok)
	require.Equal(t, 5, count)
	require.Equal(t, "2023-06-01", normalized.Request["Anthropic-Version"])
	require.Equal(t, "claude-cli/2.1.78 (external, cli)", normalized.Request["User-Agent"])
	require.Equal(t, "42", normalized.Response["Retry-After"])

	// 未知头名与身份／凭据字段名一律整份不合格（不是静默丢弃后照样落库）。
	for _, name := range []string{
		"x-custom-prompt", "authorization", "cookie", "set-cookie", "x-api-key",
		"proxy-authorization", "www-authenticate", "location", "anthropic-organization-id",
		"x-forwarded-for",
	} {
		_, _, ok := NormalizeErrorDiagnosticHeaderValues(ErrorDiagnosticHeaderValues{
			Response: map[string]string{name: "whatever"},
		})
		require.False(t, ok, "头名 %q 不得被白名单接受", name)
	}

	// 方向也要匹配：请求头不能借用响应侧的名字（反之亦然）。
	_, _, ok = NormalizeErrorDiagnosticHeaderValues(ErrorDiagnosticHeaderValues{
		Request: map[string]string{"Retry-After": "42"},
	})
	require.False(t, ok, "Retry-After 只属于响应侧")
	_, _, ok = NormalizeErrorDiagnosticHeaderValues(ErrorDiagnosticHeaderValues{
		Response: map[string]string{"Anthropic-Version": "2023-06-01"},
	})
	require.False(t, ok, "Anthropic-Version 只属于请求侧")
}

// 取值的形状与长度必须有界：数字、时间戳、媒体类型、ID 各有自己的闭集，其余一律不合格。
func TestNormalizeErrorDiagnosticHeaderValuesBoundsValueShapes(t *testing.T) {
	accept := func(request, response map[string]string) bool {
		_, _, ok := NormalizeErrorDiagnosticHeaderValues(ErrorDiagnosticHeaderValues{Request: request, Response: response})
		return ok
	}

	require.True(t, accept(map[string]string{"Anthropic-Version": "2023-06-01"}, nil))
	require.False(t, accept(map[string]string{"Anthropic-Version": "2023/06/01"}, nil))
	require.False(t, accept(map[string]string{"Anthropic-Version": "not-a-date"}, nil))

	require.True(t, accept(map[string]string{"Content-Type": "application/json"}, nil))
	require.False(t, accept(map[string]string{"Content-Type": `application/json; x="secret"`}, nil))
	require.False(t, accept(map[string]string{"Content-Type": "json"}, nil))

	require.True(t, accept(map[string]string{"X-Stainless-Retry-Count": "3"}, nil))
	require.False(t, accept(map[string]string{"X-Stainless-Retry-Count": "three"}, nil))
	require.False(t, accept(map[string]string{"X-Stainless-Retry-Count": "123456789012345678901234"}, nil))

	require.True(t, accept(nil, map[string]string{"Anthropic-Ratelimit-Requests-Remaining": "0"}))
	require.True(t, accept(nil, map[string]string{"Anthropic-Ratelimit-Requests-Reset": "2026-09-24T14:01:30Z"}))
	require.True(t, accept(nil, map[string]string{"Anthropic-Ratelimit-Requests-Reset": "1758722490"}))
	require.False(t, accept(nil, map[string]string{"Anthropic-Ratelimit-Requests-Reset": "in a while"}))
	require.True(t, accept(nil, map[string]string{"Retry-After": "42"}))
	require.True(t, accept(nil, map[string]string{"Retry-After": "Wed, 21 Oct 2026 07:28:00 GMT"}))
	require.False(t, accept(nil, map[string]string{"Retry-After": "-1"}))

	require.True(t, accept(map[string]string{"X-Request-Id": "550e8400-e29b-41d4-a716-446655440000"}, nil))
	require.True(t, accept(nil, map[string]string{"Request-Id": "req_123-abc"}))
	require.False(t, accept(nil, map[string]string{"Request-Id": "req 123 abc"}))

	require.True(t, accept(map[string]string{"Host": "api.anthropic.com:443"}, nil))
	require.False(t, accept(map[string]string{"Host": "api.anthropic.com/path"}, nil))

	// 控制字符（含 CRLF 注入）与超长取值一律不合格。
	require.False(t, accept(map[string]string{"Anthropic-Beta": "feature-2024-01-01\r\nCookie: leaked"}, nil))
	require.False(t, accept(map[string]string{"User-Agent": strings.Repeat("a", 129)}, nil))
	require.False(t, accept(map[string]string{"Accept": strings.Repeat("a", 257)}, nil))
}

// 值内的凭据形态是净化器之外的第二道防线；同时不得误伤正常值。
func TestNormalizeErrorDiagnosticHeaderValuesRejectsCredentialShapedValues(t *testing.T) {
	accept := func(request map[string]string) bool {
		_, _, ok := NormalizeErrorDiagnosticHeaderValues(ErrorDiagnosticHeaderValues{Request: request})
		return ok
	}
	for _, value := range []string{
		"Bearer sk-live-123",
		"Basic dXNlcjpwYXNz",
		"sk-ant-api03-secret",
		"ghp_abcdefghijklmnopqrstuvwxyz",
		"xoxb-1234-5678",
		"api_key=abc",
		"password=hunter2",
		"-----BEGIN PRIVATE KEY-----",
		"session=abc123",
	} {
		require.False(t, accept(map[string]string{"User-Agent": value}), "值 %q 含凭据形态，不得落库", value)
	}
	// 正常值不得被误伤：beta 特性名里的 token-counting 不是凭据形态标记。
	require.True(t, accept(map[string]string{"Anthropic-Beta": "token-counting-2024-11-01,prompt-caching-2024-07-31"}))
	require.True(t, accept(map[string]string{"Accept-Language": "en-US,en;q=0.9"}))
	require.True(t, accept(map[string]string{"User-Agent": "claude-cli/2.1.78 (external, cli)"}))
	require.True(t, accept(map[string]string{"X-App": "claude-code"}))
}

// 条目数有上限，超出即整份不合格（不做静默截断）。
func TestNormalizeErrorDiagnosticHeaderValuesCapsEntryCount(t *testing.T) {
	// 用真实存在的头名堆到上限之上：上限是「一条尝试能有多少个不同头」的健全性边界。
	request := map[string]string{
		"X-Request-Id": "a", "X-Client-Request-Id": "b", "X-Claude-Code-Session-Id": "c",
		"X-Stainless-Retry-Count": "1", "X-Stainless-Timeout": "2", "X-Stainless-Lang": "js",
		"X-Stainless-Package-Version": "1.2.3", "X-Stainless-OS": "linux", "X-Stainless-Arch": "x86_64",
		"X-Stainless-Runtime": "node", "X-Stainless-Runtime-Version": "1.2.3", "X-Stainless-Helper-Method": "stream",
		"User-Agent": "claude-cli/2.1.78", "X-App": "claude-code", "Host": "api.anthropic.com",
		"Anthropic-Version": "2023-06-01", "Anthropic-Beta": "prompt-caching-2024-07-31",
		"Accept": "application/json", "Accept-Encoding": "gzip", "Accept-Language": "en-US",
		"Content-Type": "application/json",
	}
	require.Len(t, request, 21)
	within := ErrorDiagnosticHeaderValues{Request: request, Response: map[string]string{"Retry-After": "42"}}
	require.Len(t, within.Response, 1)
	_, count, ok := NormalizeErrorDiagnosticHeaderValues(within)
	require.True(t, ok, "22 条仍在 24 条上限内")
	require.Equal(t, 22, count)

	// 超过上限：整份不合格，绝不截断成「看起来完整」的快照。
	tooMany := ErrorDiagnosticHeaderValues{Request: request, Response: map[string]string{
		"Retry-After":                                 "1",
		"Request-Id":                                  "req_a",
		"Cache-Control":                               "no-store",
		"Anthropic-Ratelimit-Requests-Limit":          "1",
		"Anthropic-Ratelimit-Requests-Remaining":      "1",
		"Anthropic-Ratelimit-Requests-Reset":          "1",
		"Anthropic-Ratelimit-Input-Tokens-Limit":      "1",
		"Anthropic-Ratelimit-Input-Tokens-Remaining":  "1",
		"Anthropic-Ratelimit-Input-Tokens-Reset":      "1",
		"Anthropic-Ratelimit-Output-Tokens-Limit":     "1",
		"Anthropic-Ratelimit-Output-Tokens-Remaining": "1",
		"Anthropic-Ratelimit-Output-Tokens-Reset":     "1",
	}}
	require.Greater(t, tooMany.EntryCount(), ErrorDiagnosticMaxHeaderEntries)
	_, _, ok = NormalizeErrorDiagnosticHeaderValues(tooMany)
	require.False(t, ok, "超过条目上限必须整份不合格")
}

// 净化器输出里只有「值」会落库：存在性标记与多值头都不构成可保存的值。
func TestErrorDiagnosticHeaderValuesFromSanitizedDropsPresenceMarkers(t *testing.T) {
	values, ok := ErrorDiagnosticHeaderValuesFromSanitized(
		map[string]any{
			"User-Agent":    "claude-cli/2.1.78",
			"Authorization": map[string]any{"present": true},
			"Cookie":        map[string]any{"present": true},
		},
		map[string]any{
			"Retry-After": "42",
			"Set-Cookie":  map[string]any{"present": true},
		},
	)
	require.True(t, ok)
	require.Equal(t, map[string]string{"User-Agent": "claude-cli/2.1.78"}, values.Request)
	require.Equal(t, map[string]string{"Retry-After": "42"}, values.Response)
	require.NotContains(t, values.Request, "Authorization")
	require.NotContains(t, values.Request, "Cookie")
	require.NotContains(t, values.Response, "Set-Cookie")

	// 契约之外的对象形状（不是存在性标记）一律不合格，绝不猜。
	_, ok = ErrorDiagnosticHeaderValuesFromSanitized(map[string]any{"User-Agent": map[string]any{"value": "x"}}, nil)
	require.False(t, ok)

	// 多行同名头按 HTTP 列表语义合并，绝不静默只留第一行。
	joined, ok := ErrorDiagnosticHeaderValuesFromSanitized(
		map[string]any{"Accept-Language": []string{"en-US", "zh-CN"}, "Anthropic-Beta": []any{"prompt-caching-2024-07-31", "token-counting-2024-11-01"}},
		nil,
	)
	require.True(t, ok)
	require.Equal(t, "en-US, zh-CN", joined.Request["Accept-Language"])
	require.Equal(t, "prompt-caching-2024-07-31, token-counting-2024-11-01", joined.Request["Anthropic-Beta"])

	// 单元素切片与字符串等价。
	single, ok := ErrorDiagnosticHeaderValuesFromSanitized(map[string]any{"Accept-Language": []any{"en-US"}}, nil)
	require.True(t, ok)
	require.Equal(t, map[string]string{"Accept-Language": "en-US"}, single.Request)

	// 结构化取值的头名出现多行时无法用一个取值表达：整份不合格，绝不只是丢掉一行。
	_, ok = ErrorDiagnosticHeaderValuesFromSanitized(map[string]any{"X-Request-Id": []string{"req_a", "req_b"}}, nil)
	require.False(t, ok, "不透明 ID 多行必须整份不合格")
	_, ok = ErrorDiagnosticHeaderValuesFromSanitized(nil, map[string]any{"Retry-After": []string{"1", "2"}})
	require.False(t, ok, "结构化响应头多行必须整份不合格")

	// 行数超过上限同样不合格。
	_, ok = ErrorDiagnosticHeaderValuesFromSanitized(
		map[string]any{"Accept-Language": []string{"a", "b", "c", "d", "e"}}, nil)
	require.False(t, ok)
}

// 载荷必须可确定性编码并能在读取时重新校验：这是「解密成功 ≠ 内容可信」的执行点。
func TestEncodeAndDecodeErrorDiagnosticHeaderValuesRoundTripIsValidated(t *testing.T) {
	values := ErrorDiagnosticHeaderValues{
		Request:  map[string]string{"Anthropic-Version": "2023-06-01"},
		Response: map[string]string{"Retry-After": "42", "Request-Id": "req_1"},
	}
	first, err := EncodeErrorDiagnosticHeaderValues(values)
	require.NoError(t, err)
	second, err := EncodeErrorDiagnosticHeaderValues(values)
	require.NoError(t, err)
	require.Equal(t, first, second, "同一份头值必须产生同一份载荷（编码是确定性的）")

	decoded, err := DecodeErrorDiagnosticHeaderValues(first)
	require.NoError(t, err)
	require.Equal(t, values.Request, decoded.Request)
	require.Equal(t, values.Response, decoded.Response)

	// 未知键、未知头名、凭据类头名与超界值都必须让解码失败，绝不返回部分结果。
	for _, payload := range []string{
		`{"request":{"x-custom":"a"}}`,
		`{"response":{"set-cookie":"a"}}`,
		`{"request":{"user-agent":"Bearer sk-live"}}`,
		`{"unexpected":{"a":"b"}}`,
		`{"request":{"user-agent":"ok"}} trailing`,
		`{}`,
	} {
		_, err := DecodeErrorDiagnosticHeaderValues([]byte(payload))
		require.Error(t, err, "载荷 %s 必须被拒绝", payload)
	}
	_, err = DecodeErrorDiagnosticHeaderValues(nil)
	require.Error(t, err)
}

// 头值状态与正文状态同构：已到期与已清除都不得被误报为仍可读。
func TestDescribeHeaderStateMapsExpiryAndPurge(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	require.Equal(t, ErrorDiagnosticHeaderStateStored,
		DescribeHeaderState(ErrorDiagnosticHeaderRetained, true, now.Add(time.Hour), now))
	require.Equal(t, ErrorDiagnosticHeaderStateExpired,
		DescribeHeaderState(ErrorDiagnosticHeaderRetained, true, now.Add(-time.Second), now))
	require.Equal(t, ErrorDiagnosticHeaderStatePurged,
		DescribeHeaderState(ErrorDiagnosticHeaderRetained, false, now.Add(-time.Hour), now))
	require.Equal(t, ErrorDiagnosticHeaderStateNotObserved,
		DescribeHeaderState(ErrorDiagnosticHeaderNotObserved, false, time.Time{}, now))
	require.Equal(t, ErrorDiagnosticHeaderStateSkipped,
		DescribeHeaderState(ErrorDiagnosticHeaderSkippedInvalidValues, false, time.Time{}, now))
}

// 载荷里的头值只以白名单名字出现，不得出现凭据类名字的键。
func TestEncodeErrorDiagnosticHeaderValuesNeverCarriesCredentialNames(t *testing.T) {
	payload, err := EncodeErrorDiagnosticHeaderValues(ErrorDiagnosticHeaderValues{
		Request: map[string]string{"User-Agent": "claude-cli/2.1.78"},
	})
	require.NoError(t, err)
	var decoded map[string]map[string]string
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Equal(t, map[string]string{"User-Agent": "claude-cli/2.1.78"}, decoded["request"])
	lower := strings.ToLower(string(payload))
	for _, forbidden := range []string{"authorization", "cookie", "set-cookie", "api-key", "token", "secret"} {
		require.NotContains(t, lower, forbidden, "载荷键名不得出现凭据类名字")
	}
}
