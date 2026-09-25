package httpattempt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCounterSurvivesContextDerivation(t *testing.T) {
	counter := NewCounter()
	ctx := WithCounter(context.Background(), counter)

	Increment(ctx)
	Increment(context.WithoutCancel(ctx))

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	Increment(cancelCtx)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	Increment(req.Clone(req.Context()).Context())

	if got := counter.Load(); got != 4 {
		t.Fatalf("expected 4 attempts, got %d", got)
	}
}

func TestIncrementRecordsBoundMetadataInTransportOrder(t *testing.T) {
	counter := NewCounter()
	ctx := WithCounter(context.Background(), counter)
	first := Metadata{AccountID: 11, Model: "model-a", Protocol: "openai.responses"}
	second := Metadata{AccountID: 22, Model: "model-b", Protocol: "anthropic.messages"}

	Increment(WithMetadata(ctx, first))
	Increment(ctx)
	Increment(WithMetadata(ctx, second))

	if got := counter.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
	got := counter.Metadata()
	want := []Metadata{first, second}
	if len(got) != len(want) {
		t.Fatalf("expected %d metadata entries, got %#v", len(want), got)
	}
	for i := range want {
		if got[i].AccountID != want[i].AccountID || got[i].Model != want[i].Model || got[i].Protocol != want[i].Protocol {
			t.Fatalf("entry %d: got %#v, want %#v", i, got[i], want[i])
		}
	}
	got[0].Model = "mutated"
	if counter.Metadata()[0].Model != first.Model {
		t.Fatalf("metadata snapshot must be isolated")
	}
}

func TestBeforeBlocksForcedAttemptAndAllowsOrdinaryFailure(t *testing.T) {
	forcedErr := errors.New("audit store down")
	forced := NewCounter()
	forcedCtx := WithCounter(context.Background(), forced)
	forced.SetBeforeAttempt(func(context.Context, Metadata) error { return forcedErr }, true)
	forcedCtx = WithMetadata(forcedCtx, Metadata{AccountID: 11, Model: "gpt-5.4", Protocol: "openai.responses"})
	require.ErrorIs(t, Before(forcedCtx), forcedErr)
	require.Zero(t, forced.Load())

	ordinary := NewCounter()
	ordinaryCtx := WithCounter(context.Background(), ordinary)
	ordinary.SetBeforeAttempt(func(context.Context, Metadata) error { return forcedErr }, false)
	ordinaryCtx = WithMetadata(ordinaryCtx, Metadata{AccountID: 22, Model: "gpt-5.4", Protocol: "openai.responses"})
	require.NoError(t, Before(ordinaryCtx))
	Increment(ordinaryCtx)
	require.Equal(t, uint64(1), ordinary.Load())
}

func TestPluginHandledMarkerIsClearedByRealHTTPAttempt(t *testing.T) {
	counter := NewCounter()
	ctx := WithCounter(context.Background(), counter)

	MarkPluginHandled(ctx)
	if !counter.LastPluginHandled() {
		t.Fatalf("expected plugin marker")
	}

	Increment(ctx)
	if counter.LastPluginHandled() {
		t.Fatalf("real HTTP attempt must clear plugin marker")
	}
}

func TestIncrementWithoutCounterIsNoOp(t *testing.T) {
	Increment(context.Background())
	MarkPluginHandled(context.Background())
}

func TestSanitizedHeaderMapReconstructionKeepsOnlyPresenceMarkers(t *testing.T) {
	got := HeaderFromSanitizedMap(map[string]any{
		"Authorization":    map[string]any{"present": true},
		"API-Key":          "historical-secret",
		"X-Api-Key":        map[string]any{"present": true},
		"Cookie":           "historical-secret",
		"X-Stainless-Lang": "go",
		"X-Request-Id":     "attacker-value",
	})
	require.Equal(t, http.Header{
		"Authorization":    {"present"},
		"API-Key":          {"present"},
		"Cookie":           {"present"},
		"X-Api-Key":        {"present"},
		"X-Request-ID":     {"present"},
		"X-Stainless-Lang": {"go"},
	}, got)
	sanitized := SanitizeRequestHeaders(got)
	require.Equal(t, map[string]any{
		"Authorization":    map[string]any{"present": true},
		"API-Key":          map[string]any{"present": true},
		"Cookie":           map[string]any{"present": true},
		"X-Api-Key":        map[string]any{"present": true},
		"X-Request-ID":     map[string]any{"present": true},
		"X-Stainless-Lang": "go",
	}, sanitized)
}

func TestResponseHeaderSanitizationIsClosedAndBoundsMultiValues(t *testing.T) {
	require.Equal(t, map[string]any{
		"Content-Type":             "application/json",
		"Set-Cookie":               map[string]any{"present": true},
		"X-Request-ID":             map[string]any{"present": true},
		"Sensitive-Header-Present": map[string]any{"present": true},
	}, SanitizeResponseHeaders(http.Header{
		"Content-Type":  {"application/json; charset=utf-8"},
		"Set-Cookie":    {"session=do-not-store"},
		"X-Request-Id":  {"untrusted-id"},
		"X-Api-Secret":  {"secret"},
		"X-Cache-Token": {"secret"},
	}))

	require.Empty(t, SanitizeRequestHeaders(http.Header{
		"X-Stainless-Lang": {"go", "rust", "java", "python", "php"},
	}))
	require.Empty(t, SanitizeRequestHeaders(http.Header{
		"X-Stainless-Retry-Count": {"101"},
	}))
	require.Equal(t, map[string]any{
		"Content-Type":         "application/json",
		"Other-Header-Present": map[string]any{"present": true},
	}, SanitizeResponseHeaders(http.Header{
		"Content-Type":   {"application/json; token=leak"},
		"Content-Length": {"123"},
	}))
}

func TestRequestHeadersAreSanitizedAndMetadataSnapshotsAreIsolated(t *testing.T) {
	counter := NewCounter()
	ctx := WithCounter(context.Background(), counter)
	ctx = WithMetadata(ctx, Metadata{AccountID: 11, Model: "model-a", Protocol: "openai.responses"})
	headers := http.Header{
		"X-Stainless-Lang":        {"go", "rust"},
		"X-Stainless-Retry-Count": {"2"},
		"X-Stainless-OS":          {"Windows\\r\\nAuthorization: Bearer leaked"},
		"Authorization":           {"Bearer top-secret"},
		"X-Api-Key":               {"api-secret"},
		"X-Internal-Debug-Token":  {"must-not-be-recorded"},
		"Header-Name-Canary":      {"Header-Value-Canary"},
	}

	ctx = WithRequestHeaders(ctx, headers)
	Increment(ctx)
	got := counter.Metadata()
	require.Len(t, got, 1)
	require.Equal(t, int64(11), got[0].AccountID)
	require.Equal(t, map[string]any{
		"X-Stainless-Lang":         []string{"go", "rust"},
		"X-Stainless-Retry-Count":  "2",
		"Authorization":            map[string]any{"present": true},
		"Sensitive-Header-Present": map[string]any{"present": true},
		"Other-Header-Present":     map[string]any{"present": true},
		"X-Api-Key":                map[string]any{"present": true},
	}, got[0].RequestHeaders)

	headers.Set("X-Stainless-Lang", "changed")
	lang, ok := got[0].RequestHeaders["X-Stainless-Lang"].([]string)
	require.True(t, ok, "X-Stainless-Lang must be recorded as []string")
	lang[0] = "changed"
	require.Equal(t, []string{"go", "rust"}, counter.Metadata()[0].RequestHeaders["X-Stainless-Lang"])
}

func TestRequestHeaderSanitizationExpandsFixedSafeNamesAndRedactsFreeText(t *testing.T) {
	got := SanitizeRequestHeaders(http.Header{
		"Host":                        {"api.anthropic.com"},
		"User-Agent":                  {"ua-value-canary"},
		"Content-Type":                {"application/json; charset=utf-8"},
		"Accept":                      {"application/json, */*"},
		"Accept-Encoding":             {"gzip", "br"},
		"Anthropic-Version":           {"2023-06-01"},
		"Anthropic-Beta":              {"prompt-caching-2024-07-31"},
		"X-App":                       {"app-value-canary"},
		"X-Request-ID":                {"request-id-canary"},
		"X-Client-Request-ID":         {"client-id-canary"},
		"X-Header-Name-Canary":        {"header-value-canary"},
		"X-Stainless-Retry-Count":     {"100"},
		"X-Stainless-Timeout":         {"120000"},
		"X-Stainless-Lang":            {"go", "rust"},
		"X-Stainless-Package-Version": {"1.2.3"},
		"X-Stainless-OS":              {"windows"},
		"X-Stainless-Arch":            {"x86_64"},
		"X-Stainless-Runtime":         {"node"},
		"X-Stainless-Runtime-Version": {"20.1.0"},
		"x-stainless-helper-method":   {"stream"},
	})

	require.Equal(t, map[string]any{
		"Host":                        "api.anthropic.com",
		"User-Agent":                  map[string]any{"present": true},
		"Content-Type":                "application/json",
		"Accept":                      "application/json, */*",
		"Accept-Encoding":             []string{"gzip", "br"},
		"Anthropic-Version":           "2023-06-01",
		"Anthropic-Beta":              map[string]any{"present": true},
		"X-App":                       map[string]any{"present": true},
		"X-Request-ID":                map[string]any{"present": true},
		"X-Client-Request-ID":         map[string]any{"present": true},
		"Other-Header-Present":        map[string]any{"present": true},
		"X-Stainless-Retry-Count":     "100",
		"X-Stainless-Timeout":         "120000",
		"X-Stainless-Lang":            []string{"go", "rust"},
		"X-Stainless-Package-Version": "1.2.3",
		"X-Stainless-OS":              "windows",
		"X-Stainless-Arch":            "x86_64",
		"X-Stainless-Runtime":         "node",
		"X-Stainless-Runtime-Version": "20.1.0",
		"x-stainless-helper-method":   "stream",
	}, got)
}

func TestRequestHeaderSanitizationEnforcesValueKindsAndNumericThresholds(t *testing.T) {
	require.Equal(t, map[string]any{
		"X-Stainless-Retry-Count": "100",
		"X-Stainless-Timeout":     "120000",
		"X-App":                   map[string]any{"present": true},
	}, SanitizeRequestHeaders(http.Header{
		"X-Stainless-Retry-Count": {"100"},
		"X-Stainless-Timeout":     {"120000"},
		"X-App":                   {"not-an-enum-canary"},
		"Anthropic-Version":       {"token-shaped-canary"},
		"Accept-Encoding":         {"gzip", "unknown-encoding-canary"},
	}))

	require.Empty(t, SanitizeRequestHeaders(http.Header{
		"X-Stainless-Retry-Count": {"101"},
		"X-Stainless-Timeout":     {"120001"},
	}))
}

func TestResponseHeaderSanitizationAddsIDsAndNumericRateLimits(t *testing.T) {
	got := SanitizeResponseHeaders(http.Header{
		"Content-Type":                       {"application/json; charset=utf-8"},
		"Cache-Control":                      {"no-cache, private"},
		"X-Request-ID":                       {"response-id-canary"},
		"Request-ID":                         {"request-id-canary"},
		"Anthropic-Ratelimit-Requests-Limit": {"1000", "2000"},
		"Anthropic-Ratelimit-Input-Tokens-Remaining": {"9000"},
		"Anthropic-Ratelimit-Output-Tokens-Limit":    {"18446744073709551615"},
		"Anthropic-Ratelimit-Requests-Reset":         {"reset-value-canary"},
		"Anthropic-Ratelimit-Unknown-Canary":         {"ratelimit-value-canary"},
		"X-Response-Name-Canary":                     {"response-value-canary"},
	})

	require.Equal(t, map[string]any{
		"Content-Type":                       "application/json",
		"Cache-Control":                      "no-cache, private",
		"X-Request-ID":                       map[string]any{"present": true},
		"Request-ID":                         map[string]any{"present": true},
		"Anthropic-Ratelimit-Requests-Limit": []string{"1000", "2000"},
		"Anthropic-Ratelimit-Input-Tokens-Remaining": "9000",
		"Anthropic-Ratelimit-Output-Tokens-Limit":    "18446744073709551615",
		"Other-Header-Present":                       map[string]any{"present": true},
	}, got)
}

func TestRequestHeaderSnapshotRoundTripKeepsAggregatePresenceMarkers(t *testing.T) {
	const (
		secretNameCanary = "x-auth-token-canary"
		otherNameCanary  = "x-custom-header-canary"
	)
	sanitized := SanitizeRequestHeaders(http.Header{
		"X-Auth-Token":    {secretNameCanary},
		"X-Custom-Header": {otherNameCanary},
		"Accept":          {"application/json"},
	})
	require.Equal(t, map[string]any{
		"Accept":                   "application/json",
		"Sensitive-Header-Present": map[string]any{"present": true},
		"Other-Header-Present":     map[string]any{"present": true},
	}, sanitized)

	snapshot := HeaderFromSanitizedMap(sanitized)
	require.Equal(t, http.Header{
		"Accept":                   {"application/json"},
		"Sensitive-Header-Present": {"present"},
		"Other-Header-Present":     {"present"},
	}, snapshot)

	resanitized := SanitizeRequestHeaders(snapshot)
	require.Equal(t, sanitized, resanitized, "snapshot revalidation must keep the same aggregate presence markers")
	require.Equal(t, snapshot, HeaderFromSanitizedMap(resanitized), "snapshot round trips must be idempotent")

	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), secretNameCanary)
	require.NotContains(t, string(encoded), otherNameCanary)
}

func TestResponseHeaderSnapshotRoundTripKeepsAggregatePresenceMarkers(t *testing.T) {
	const secretCanary = "response-secret-canary"
	sanitized := SanitizeResponseHeaders(http.Header{
		"Content-Type": {"application/json"},
		"X-Api-Secret": {secretCanary},
	})
	require.Equal(t, map[string]any{
		"Content-Type":             "application/json",
		"Sensitive-Header-Present": map[string]any{"present": true},
	}, sanitized)

	snapshot := ResponseHeaderFromSanitizedMap(sanitized)
	require.Equal(t, http.Header{
		"Content-Type":             {"application/json"},
		"Sensitive-Header-Present": {"present"},
	}, snapshot, "a sensitive marker must never degrade into the other aggregate marker")

	resanitized := SanitizeResponseHeaders(snapshot)
	require.Equal(t, sanitized, resanitized, "snapshot revalidation must keep the same aggregate presence markers")
	require.Equal(t, snapshot, ResponseHeaderFromSanitizedMap(resanitized), "snapshot round trips must be idempotent")

	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), secretCanary)
}

func TestHeaderReconstructionAcceptsOnlyExactAggregatePresenceMarkers(t *testing.T) {
	require.Equal(t, http.Header{
		"Other-Header-Present": {"present"},
	}, HeaderFromSanitizedMap(map[string]any{
		"other-header-present": map[string]any{"present": true},
	}))

	for _, rejected := range []any{
		false,
		true,
		"raw-marker-canary",
		[]string{"present"},
		[]any{"present"},
		map[string]any{"present": false},
		map[string]any{"present": true, "name": "raw-marker-canary"},
		map[string]any{"name": "raw-marker-canary"},
	} {
		require.Empty(t, HeaderFromSanitizedMap(map[string]any{"Other-Header-Present": rejected}), "%#v", rejected)
		require.Empty(t, ResponseHeaderFromSanitizedMap(map[string]any{"Sensitive-Header-Present": rejected}), "%#v", rejected)
	}
}

func TestSanitizedHeaderMapRevalidationRejectsRawAggregateMarkerValues(t *testing.T) {
	require.Equal(t, map[string]any{
		"Sensitive-Header-Present": map[string]any{"present": true},
	}, SanitizeRequestHeaders(http.Header{
		"sensitive-header-present": {"present"},
	}))
	require.Empty(t, SanitizeRequestHeaders(http.Header{
		"sensitive-header-present": {"false"},
	}))
	require.Empty(t, SanitizeRequestHeaders(http.Header{
		"other-header-present": {"raw-marker-canary"},
	}))
	require.Empty(t, SanitizeRequestHeaders(http.Header{
		"other-header-present": {"present", "present"},
	}))
	require.Empty(t, SanitizeResponseHeaderMap(map[string]any{
		"Other-Header-Present":     "raw-marker-canary",
		"Sensitive-Header-Present": false,
	}))
	require.Empty(t, SanitizeRequestHeaderMap(map[string]any{
		"Other-Header-Present":     "raw-marker-canary",
		"Sensitive-Header-Present": false,
	}))
}

func TestResponseHeaderMapReconstructionUsesResponseContentTypePolicy(t *testing.T) {
	got := ResponseHeaderFromSanitizedMap(map[string]any{
		"Content-Type":          []string{"text/event-stream", "application/json"},
		"Set-Cookie":            "session=historical-secret",
		"Authorization":         map[string]any{"present": true},
		"Unknown-Header-Canary": "must-not-survive",
	})
	require.Equal(t, http.Header{
		"Content-Type": {"text/event-stream", "application/json"},
		"Set-Cookie":   {"present"},
	}, got)
}

func TestSanitizedHeaderMapReconstructionRedactsHistoricalFreeText(t *testing.T) {
	got := HeaderFromSanitizedMap(map[string]any{
		"User-Agent":           "historical-ua-canary",
		"Anthropic-Beta":       "historical-beta-canary",
		"X-App":                []string{"historical-app-canary"},
		"X-Request-ID":         "historical-request-id-canary",
		"X-Client-Request-ID":  map[string]any{"present": true},
		"X-Header-Name-Canary": "historical-header-value-canary",
	})
	require.Equal(t, http.Header{
		"User-Agent":          {"present"},
		"Anthropic-Beta":      {"present"},
		"X-App":               {"present"},
		"X-Request-ID":        {"present"},
		"X-Client-Request-ID": {"present"},
	}, got)
	require.Equal(t, map[string]any{
		"User-Agent":          map[string]any{"present": true},
		"Anthropic-Beta":      map[string]any{"present": true},
		"X-App":               map[string]any{"present": true},
		"X-Request-ID":        map[string]any{"present": true},
		"X-Client-Request-ID": map[string]any{"present": true},
	}, SanitizeRequestHeaders(got))
}

// 上游在完整刷出终态事件后拖延关闭连接时，服务层会提前停止读取并 Close。这次 Close
// 不代表读取不完整，因此只能补记最近一次（仍打开的那次）尝试，不能把更早的尝试追认为
// 完整读取；序列语义下一次尝试在时间上总是最新的。
func TestMarkLastResponseReadCompleteIsScopedToMostRecentAttempt(t *testing.T) {
	counter := NewCounter()
	ctx := WithCounter(context.Background(), counter)

	first := StartAttempt(WithMetadata(ctx, Metadata{AccountID: 11, Model: "model-a", Protocol: "openai.responses"}))
	require.NotNil(t, first)
	first.SetResponse(http.StatusBadGateway, http.Header{"Content-Type": {"application/json"}}, true)

	second := StartAttempt(WithMetadata(ctx, Metadata{AccountID: 22, Model: "model-b", Protocol: "openai.responses"}))
	require.NotNil(t, second)
	second.SetResponse(http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}}, true)

	require.Len(t, counter.Metadata(), 2)
	require.False(t, *counter.Metadata()[0].ResponseReadComplete)
	require.False(t, *counter.Metadata()[1].ResponseReadComplete)

	counter.MarkLastResponseReadComplete()

	got := counter.Metadata()
	require.False(t, *got[0].ResponseReadComplete,
		"标记最新一次尝试的终态不得把更早的尝试追认为已完整读取")
	require.True(t, *got[1].ResponseReadComplete)
}

// 插件接管的上游尝试不进传输尝试元数据：最新一次尝试没有响应事实，更早一次尝试的
// 读取结论也不能被追认为完整。
func TestMarkLastResponseReadCompleteSkipsPluginHandledAttempt(t *testing.T) {
	counter := NewCounter()
	ctx := WithCounter(context.Background(), counter)

	first := StartAttempt(ctx)
	require.NotNil(t, first)
	first.SetResponse(http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}}, true)
	require.False(t, *counter.Metadata()[0].ResponseReadComplete)

	// 后一次尝试由插件接管：只留下插件标记，不会追加传输元数据。
	MarkPluginHandled(ctx)
	counter.MarkLastResponseReadComplete()

	require.False(t, *counter.Metadata()[0].ResponseReadComplete,
		"插件接管的最新尝试没有响应事实，不得把更早一次尝试追认为已完整读取")
}

// 没有登记的响应（无尝试，或传输层从未拿到 HTTP 响应）就没有可补记的读取结论，
// 不能凭空造出一条尝试。
func TestMarkLastResponseReadCompleteNeedsRecordedResponse(t *testing.T) {
	counter := NewCounter()
	ctx := WithCounter(context.Background(), counter)

	counter.MarkLastResponseReadComplete()
	require.Empty(t, counter.Metadata())

	attempt := StartAttempt(ctx)
	require.NotNil(t, attempt)
	counter.MarkLastResponseReadComplete()

	require.Len(t, counter.Metadata(), 1)
	require.Nil(t, counter.Metadata()[0].ResponseReadComplete,
		"没有 HTTP 响应事实的尝试没有可补记的读取完整性")
}

// 终态事件后提前 Close 不得覆盖已经记下的「已完整读取」，也不得顺手落正文。
func TestResponseBodyCloseDoesNotDowngradeCompletedRead(t *testing.T) {
	counter := NewCounter()
	ctx := WithCounter(context.Background(), counter)
	attempt := StartAttempt(WithMetadata(ctx, Metadata{AccountID: 33, Protocol: "openai.responses"}))
	require.NotNil(t, attempt)
	attempt.SetResponse(http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}}, true)

	payload := "data: {\"type\":\"response.completed\"}\n\n"
	body := NewResponseBody(io.NopCloser(strings.NewReader(payload)), attempt)

	buf := make([]byte, 64)
	n, err := body.Read(buf)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)
	require.Equal(t, int64(len(payload)), *counter.Metadata()[0].ResponseBytes)

	before := counter.Metadata()[0]
	counter.MarkLastResponseReadComplete()

	after := counter.Metadata()[0]
	require.True(t, *after.ResponseReadComplete)
	before.ResponseReadComplete = after.ResponseReadComplete
	require.Equal(t, before, after, "补记终态只能改读取结论，不得保存正文或改动体量")

	require.NoError(t, body.Close())
	require.True(t, *counter.Metadata()[0].ResponseReadComplete,
		"终态之后提前 Close 属于正常收尾，不得记成读取不完整")
}

// 没有终态标记时，EOF 之前 Close 仍然是「读取不完整」。
func TestResponseBodyCloseWithoutTerminalRecordsIncompleteRead(t *testing.T) {
	counter := NewCounter()
	ctx := WithCounter(context.Background(), counter)
	attempt := StartAttempt(ctx)
	require.NotNil(t, attempt)
	attempt.SetResponse(http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}}, true)

	body := NewResponseBody(io.NopCloser(strings.NewReader("data: {}\n\n")), attempt)
	require.NoError(t, body.Close())

	got := counter.Metadata()[0]
	require.NotNil(t, got.ResponseReadComplete)
	require.False(t, *got.ResponseReadComplete, "没有终态的提前 Close 必须记成读取不完整")
}

// 读到 EOF 仍是「已完整读取」的唯一传输证据，不能被后续 Close 或补记改动。
func TestResponseBodyReadToEOFMarksReadComplete(t *testing.T) {
	counter := NewCounter()
	ctx := WithCounter(context.Background(), counter)
	attempt := StartAttempt(ctx)
	require.NotNil(t, attempt)
	attempt.SetResponse(http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}}, true)

	body := NewResponseBody(io.NopCloser(strings.NewReader("data: {}\n\n")), attempt)
	_, err := io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())

	require.True(t, *counter.Metadata()[0].ResponseReadComplete)
}

// 值快照的省略摘要必须由传输层在**采集那一刻**记在尝试元数据上：服务层看到的
// 已经是净化器筛过的取值，未知头名与没通过校验的取值在那里根本不存在，
// 因此「有条目没被收下」这个事实只能在入口处记下来才能传到载荷里的 truncated。
func TestClaudeValueSnapshotOmissionIsCarriedByAttemptMetadata(t *testing.T) {
	counter := NewCounter()
	ctx := WithClaudeHeaderValueCapture(WithCounter(context.Background(), counter), true)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	require.NoError(t, err)
	req.Header.Set("User-Agent", "claude-cli/2.1.258 (external, cli)")
	req.Header.Set("X-Unknown-Private", "private-unknown-value")
	req.Header.Set("Authorization", "Bearer top-secret")
	attempt := StartRequestAttempt(req)
	require.NotNil(t, attempt)
	attempt.SetResponse(429, http.Header{
		"Retry-After":        {"42"},
		"X-Upstream-Private": {"private-response-value"},
		"WWW-Authenticate":   {"Bearer realm=secret"},
	}, false)

	metadata := counter.Metadata()
	require.Len(t, metadata, 1)
	require.Equal(t, 1, metadata[0].RequestHeaderValueOmission.OmittedNames, "闭集外的入站头名算省略")
	require.Equal(t, 1, metadata[0].ResponseHeaderValueOmission.OmittedNames, "闭集外的响应头名算省略")
	require.True(t, metadata[0].RequestHeaderValueOmission.Any())
	require.True(t, metadata[0].ResponseHeaderValueOmission.Any())
	require.Equal(t, map[string]any{"present": true}, metadata[0].RequestHeaderValues["Authorization"])
	require.Equal(t, map[string]any{"present": true}, metadata[0].ResponseHeaderValues["Www-Authenticate"])

	// 摘要跨上下文派生与元数据快照原样保留。
	roundTrip, ok := MetadataFromContext(WithMetadata(context.Background(), metadata[0]))
	require.True(t, ok)
	require.Equal(t, metadata[0].RequestHeaderValueOmission, roundTrip.RequestHeaderValueOmission)
	require.Equal(t, metadata[0].ResponseHeaderValueOmission, roundTrip.ResponseHeaderValueOmission)

	// 名字与取值都不进元数据：摘要只有计数。
	encoded, err := json.Marshal(metadata[0].RequestHeaderValueOmission)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private")
	require.NotContains(t, string(encoded), "Unknown")
}

// 未打开值快照标记时一切保持零值：关闭的部署在热路径上既不复制取值，
// 也不会声称「有条目没被收下」。
func TestClaudeValueSnapshotOmissionStaysZeroWhenCaptureIsOff(t *testing.T) {
	counter := NewCounter()
	ctx := WithCounter(context.Background(), counter)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	require.NoError(t, err)
	req.Header.Set("User-Agent", "claude-cli/2.1.258 (external, cli)")
	req.Header.Set("X-Unknown-Private", "private-unknown-value")
	attempt := StartRequestAttempt(req)
	require.NotNil(t, attempt)
	attempt.SetResponse(200, http.Header{"X-Upstream-Private": {"private-response-value"}}, false)

	metadata := counter.Metadata()
	require.Len(t, metadata, 1)
	require.False(t, metadata[0].RequestHeaderValueOmission.Any())
	require.False(t, metadata[0].ResponseHeaderValueOmission.Any())
	require.Empty(t, metadata[0].RequestHeaderValues)
	require.Empty(t, metadata[0].ResponseHeaderValues)
}
