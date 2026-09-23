package httpattempt

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
	got[0].RequestHeaders["X-Stainless-Lang"].([]string)[0] = "changed"
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
