package httpattempt

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClaudeHeaderValuesKeepObservedSafeValuesWithoutCredentials(t *testing.T) {
	got := SanitizeClaudeRequestHeaderValues(http.Header{
		"User-Agent":               {"claude-cli/2.1.78 (external, cli)"},
		"Anthropic-Beta":           {"prompt-caching-2024-07-31,token-counting-2024-11-01"},
		"Accept-Language":          {"en-US,en;q=0.9"},
		"X-Stainless-Lang":         {"js"},
		"X-Claude-Code-Session-Id": {"550e8400-e29b-41d4-a716-446655440000"},
		"X-Client-Request-Id":      {"550e8400-e29b-41d4-a716-446655440001"},
		"Authorization":            {"Bearer top-secret"},
		"Cookie":                   {"session=secret"},
		"X-Api-Key":                {"sk-private"},
		"X-Custom-Auth":            {"secret"},
	})
	require.Equal(t, "claude-cli/2.1.78 (external, cli)", got["User-Agent"])
	require.Equal(t, "prompt-caching-2024-07-31,token-counting-2024-11-01", got["Anthropic-Beta"])
	require.Equal(t, "en-US,en;q=0.9", got["Accept-Language"])
	require.Equal(t, "js", got["X-Stainless-Lang"])
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440000", got["X-Claude-Code-Session-Id"])
	require.Equal(t, map[string]any{"present": true}, got["Authorization"])
	require.Equal(t, map[string]any{"present": true}, got["Cookie"])
	require.Equal(t, map[string]any{"present": true}, got["X-Api-Key"])
	require.NotContains(t, got, "X-Custom-Auth")
}

func TestClaudeHeaderValuesNormalizeStainlessHelperCasingForDisclosure(t *testing.T) {
	got := SanitizeClaudeRequestHeaderValues(http.Header{"x-stainless-helper-method": {"stream"}})
	require.Equal(t, "stream", got["X-Stainless-Helper-Method"])
	require.Equal(t, got, SanitizeClaudeRequestHeaderValueMap(got))
}

func TestClaudeHeaderValuesIncludeDefaultMimicHeaders(t *testing.T) {
	got := SanitizeClaudeRequestHeaderValues(http.Header{
		"User-Agent":     {"claude-cli/2.1.258 (external, cli)"},
		"Anthropic-Beta": {"claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14"},
		"Anthropic-Dangerous-Direct-Browser-Access": {"true"},
	})
	require.Equal(t, "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14", got["Anthropic-Beta"])
	require.Equal(t, "claude-cli/2.1.258 (external, cli)", got["User-Agent"])
	require.Equal(t, "true", got["Anthropic-Dangerous-Direct-Browser-Access"])
}

func TestClaude429ResponseHeadersIncludeResetTimeAndRetryAfter(t *testing.T) {
	got := SanitizeClaudeResponseHeaderValues(http.Header{
		"Retry-After":                            {"42"},
		"Anthropic-Ratelimit-Requests-Remaining": {"0"},
		"Anthropic-Ratelimit-Requests-Reset":     {"2026-09-24T14:01:30Z"},
		"Request-Id":                             {"req_123-abc"},
		"Set-Cookie":                             {"session=secret"},
		"WWW-Authenticate":                       {"Bearer realm=secret"},
	})
	require.Equal(t, "42", got["Retry-After"])
	require.Equal(t, "0", got["Anthropic-Ratelimit-Requests-Remaining"])
	require.Equal(t, "2026-09-24T14:01:30Z", got["Anthropic-Ratelimit-Requests-Reset"])
	require.Equal(t, "req_123-abc", got["Request-Id"])
	require.Equal(t, map[string]any{"present": true}, got["Set-Cookie"])
	require.Equal(t, map[string]any{"present": true}, got["Www-Authenticate"])
}

// Retry-After 与 Anthropic-Ratelimit-*-Reset 的 HTTP-date 形态都是 RFC 9110 允许的合法取值，
// 上游确实会这么发；它们必须被收下，否则（整份或全无之下）合法的 HTTP-date 会让整份快照被丢弃。
func TestClaude429ResponseHeadersAcceptHTTPDateRetryAfterAndReset(t *testing.T) {
	httpDate := "Wed, 21 Oct 2026 07:28:00 GMT"
	values, omission := SanitizeClaudeResponseHeaderValuesWithOmission(http.Header{
		"Retry-After":                        {httpDate},
		"Anthropic-Ratelimit-Requests-Reset": {httpDate},
		"Anthropic-Ratelimit-Requests-Limit": {"10"},
	})
	require.Equal(t, httpDate, values["Retry-After"])
	require.Equal(t, httpDate, values["Anthropic-Ratelimit-Requests-Reset"])
	require.Equal(t, "10", values["Anthropic-Ratelimit-Requests-Limit"])
	require.False(t, omission.Any(), "合法取值不得被算成省略")

	// 形状之外的值仍然不合格：bound 仍然是闭集，不是「看起来像时间就放行」。
	_, omission = SanitizeClaudeResponseHeaderValuesWithOmission(http.Header{
		"Retry-After":                        {"in a while"},
		"Anthropic-Ratelimit-Requests-Reset": {"sometime soon"},
	})
	require.Equal(t, 2, omission.RejectedValues)
}

func TestClaudeHeaderValuesRejectUnknownAndSecretLikeValues(t *testing.T) {
	got := SanitizeClaudeRequestHeaderValues(http.Header{
		"User-Agent":          {"Bearer sk-sensitive"},
		"Anthropic-Beta":      {"feature-2024-01-01\r\nCookie: leaked"},
		"X-App":               {"sk-secret"},
		"X-Client-Request-Id": {"Bearer-secret"},
		"X-Custom-Prompt":     {"a private message"},
	})
	require.NotContains(t, got, "User-Agent")
	require.NotContains(t, got, "Anthropic-Beta")
	require.NotContains(t, got, "X-App")
	require.NotContains(t, got, "X-Client-Request-Id")
	require.NotContains(t, got, "X-Custom-Prompt")
}

// 省略摘要只回答「有多少被观察到的头没进快照」，绝不携带头名或取值：
// 未知头可能承载认证通道，因此「被省略」这个事实可传达，「省略的是什么」不可传达。
func TestClaudeHeaderValueOmissionCountsWithoutRecordingNames(t *testing.T) {
	values, omission := SanitizeClaudeRequestHeaderValuesWithOmission(http.Header{
		"User-Agent":     {"claude-cli/2.1.258 (external, cli)"},
		"X-Unknown-Priv": {"private-unknown-value"},
		"X-Custom-Auth":  {"private-second-value"},
		"Authorization":  {"Bearer top-secret"},
		"Cookie":         {"session=secret"},
		"Anthropic-Beta": {"not-a-beta-feature"},
	})
	require.Equal(t, "claude-cli/2.1.258 (external, cli)", values["User-Agent"])
	require.Equal(t, map[string]any{"present": true}, values["Authorization"])
	require.Equal(t, 2, omission.OmittedNames, "只有闭集外的名字算省略")
	require.Equal(t, 1, omission.RejectedValues, "没通过取值校验的取值算省略")
	require.True(t, omission.Any())

	encoded, err := json.Marshal(omission)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private", "摘要里不得出现任何取值")
	require.NotContains(t, string(encoded), "Unknown", "摘要里不得出现任何头名")
}

// 凭据类头得到的存在性标记是**设计内**的排除：正常 Claude Code 请求每个都带 Authorization，
// 因此它们绝不能把快照标成「不完整」。
func TestClaudeHeaderValueOmissionIgnoresIntentionallyExcludedCredentials(t *testing.T) {
	values, omission := SanitizeClaudeRequestHeaderValuesWithOmission(http.Header{
		"Authorization":       {"Bearer top-secret"},
		"X-Api-Key":           {"sk-private"},
		"Cookie":              {"session=secret"},
		"Proxy-Authorization": {"Basic abc"},
	})
	require.Equal(t, map[string]any{"present": true}, values["Authorization"])
	require.False(t, omission.Any(), "凭据头的存在性标记不算省略")

	_, omission = SanitizeClaudeResponseHeaderValuesWithOmission(http.Header{
		"Set-Cookie":       {"session=secret"},
		"WWW-Authenticate": {"Bearer realm=secret"},
	})
	require.False(t, omission.Any(), "响应侧凭据头同样不算省略")
}

// 「哪些被观察到的头算省略」按消费方口径分别解释，且两者**只在闭集外的头名上不同**：
// 旁路（sidecar）口径把它们也算成省略；429 错误诊断的口径只数闭集内、有资格进快照的名字。
// 净化结果本身（可落库的取值）两种口径完全相同。
func TestClaudeHeaderValueOmissionScopesDifferOnlyInUnlistedNames(t *testing.T) {
	headers := http.Header{
		"User-Agent":     {"claude-cli/2.1.258 (external, cli)"},
		"Date":           {"Fri, 25 Sep 2026 00:05:49 GMT"},
		"X-Unknown":      {"private-unlisted"},
		"Anthropic-Beta": {"not-a-beta-feature"},
	}
	values, sidecar := SanitizeClaudeRequestHeaderValuesWithOmission(headers)
	eligibleValues, eligible := SanitizeClaudeRequestHeaderValuesWithEligibleOmission(headers)
	require.Equal(t, values, eligibleValues, "两种口径下可落库的取值必须一致")
	require.Equal(t, "claude-cli/2.1.258 (external, cli)", eligibleValues["User-Agent"])

	require.Equal(t, 2, sidecar.OmittedNames, "旁路口径把闭集外的两个名字也算成省略")
	require.Zero(t, eligible.OmittedNames, "有资格口径不数闭集外的名字")
	require.Equal(t, 1, sidecar.RejectedValues)
	require.Equal(t, 1, eligible.RejectedValues, "闭集内取值被拒在两种口径下都算省略")

	// 闭集内的名字无法唯一表示（行数超限）时，有资格口径同样算省略。
	_, multiLine := SanitizeClaudeResponseHeaderValuesWithEligibleOmission(http.Header{"Retry-After": {"1", "2", "3", "4", "5"}})
	require.Equal(t, 1, multiLine.OmittedNames)

	// 闭集外的名字即使行数超限也不是「有资格的头丢了」；旁路口径照旧把它算成省略。
	_, unlistedMultiLine := SanitizeClaudeRequestHeaderValuesWithEligibleOmission(http.Header{"X-Unknown": {"1", "2", "3", "4", "5"}})
	require.False(t, unlistedMultiLine.Any())
	_, sidecarUnlistedMultiLine := SanitizeClaudeRequestHeaderValuesWithOmission(http.Header{"X-Unknown": {"1", "2", "3", "4", "5"}})
	require.Equal(t, 1, sidecarUnlistedMultiLine.OmittedNames,
		"旁路口径的既有语义不变：任何超限条目都算省略")

	// 凭据头是设计内的排除：有资格口径下即使多行也不算省略。
	_, credential := SanitizeClaudeRequestHeaderValuesWithEligibleOmission(http.Header{
		"Cookie":        {"1", "2", "3", "4", "5"},
		"Authorization": {"Bearer top-secret"},
	})
	require.False(t, credential.Any())
}

// 带摘要的变体与既有净化器**同源**：可落库的取值完全一致，只是在旁多给一份计数。
// 429 错误诊断等既有调用方继续用原函数，语义不变。
func TestClaudeHeaderValuesOmissionVariantKeepsSanitizerOutput(t *testing.T) {
	headers := http.Header{
		"User-Agent":     {"claude-cli/2.1.258 (external, cli)"},
		"Host":           {"api.anthropic.com"},
		"X-Unknown-Priv": {"private-unknown-value"},
	}
	require.Equal(t, SanitizeClaudeRequestHeaderValues(headers), mustRequestHeaderValues(headers))
	values, omission := SanitizeClaudeRequestHeaderValuesWithOmission(headers)
	require.Equal(t, SanitizeClaudeRequestHeaderValues(headers), values)
	require.Equal(t, 1, omission.OmittedNames)

	responseHeaders := http.Header{
		"Content-Type":                           {"application/json"},
		"Retry-After":                            {"99999999999"},
		"Anthropic-Ratelimit-Requests-Limit":     {"10"},
		"Anthropic-Ratelimit-Requests-Remaining": {"not-a-number"},
	}
	require.Equal(t, SanitizeClaudeResponseHeaderValues(responseHeaders), mustResponseHeaderValues(responseHeaders))
	responseValues, responseOmission := SanitizeClaudeResponseHeaderValuesWithOmission(responseHeaders)
	require.Equal(t, SanitizeClaudeResponseHeaderValues(responseHeaders), responseValues)
	require.Equal(t, 2, responseOmission.RejectedValues)
}

func mustRequestHeaderValues(headers http.Header) map[string]any {
	values, _ := SanitizeClaudeRequestHeaderValuesWithOmission(headers)
	return values
}

func mustResponseHeaderValues(headers http.Header) map[string]any {
	values, _ := SanitizeClaudeResponseHeaderValuesWithOmission(headers)
	return values
}

// 条目数超上限时整份头都不收下：此时只记「有多少个被观察到的头没进快照」，不记名字。
func TestClaudeHeaderValueOmissionSaturatesWhenWholeSnapshotIsDropped(t *testing.T) {
	headers := http.Header{}
	for i := 0; i <= claudeHeaderMaxEntries; i++ {
		headers[fmt.Sprintf("X-Fill-%d", i)] = []string{"value"}
	}
	values, omission := SanitizeClaudeRequestHeaderValuesWithOmission(headers)
	require.Empty(t, values)
	require.Equal(t, claudeHeaderMaxEntries+1, omission.OmittedNames)
	require.True(t, omission.Any())
}

// 空头集是「没有观察」而不是「观察到了但没收下」。
func TestClaudeHeaderValueOmissionIsEmptyWhenNothingObserved(t *testing.T) {
	values, omission := SanitizeClaudeRequestHeaderValuesWithOmission(nil)
	require.Empty(t, values)
	require.False(t, omission.Any())
}

func TestClaudeHeaderValuesAreBoundedAndResanitizable(t *testing.T) {
	got := SanitizeClaudeRequestHeaderValues(http.Header{"User-Agent": {"claude-cli/2.1.78"}, "Authorization": {"Bearer secret"}})
	require.Equal(t, got, SanitizeClaudeRequestHeaderValueMap(got))
	got["User-Agent"] = "Bearer secret"
	got["Authorization"] = "Bearer secret"
	got["X-Untrusted"] = "private"
	require.Equal(t, map[string]any{"Authorization": map[string]any{"present": true}}, SanitizeClaudeRequestHeaderValueMap(got))
	require.Empty(t, SanitizeClaudeResponseHeaderValues(http.Header{"Retry-After": {"99999999999"}, "Anthropic-Ratelimit-Requests-Reset": {"secret"}}))
}
