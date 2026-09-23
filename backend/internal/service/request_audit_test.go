//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/stretchr/testify/require"
)

func TestSanitizeRequestAuditHeadersKeepsStainlessClosedList(t *testing.T) {
	in := http.Header{}
	in.Set("X-Stainless-Retry-Count", "0")
	in.Set("X-Stainless-Timeout", "600")
	in.Set("X-Stainless-Lang", "js")
	in.Set("X-Stainless-Package-Version", "0.39.0")
	in.Set("X-Stainless-OS", "MacOS")
	in.Set("X-Stainless-Arch", "arm64")
	in.Set("X-Stainless-Runtime", "node")
	in.Set("X-Stainless-Runtime-Version", "20.0.0")
	in.Set("x-stainless-helper-method", "stream")
	in.Set("Authorization", "Bearer sk-secret-value")
	in.Set("Cookie", "session=abc")
	in.Set("X-Api-Key", "sk-other")
	in.Set("X-Custom-Debug", "leak-me")
	in.Set("Accept", "application/json")

	out := SanitizeRequestAuditHeaders(in)

	require.Equal(t, "0", out["X-Stainless-Retry-Count"])
	require.Equal(t, "600", out["X-Stainless-Timeout"])
	require.Equal(t, "js", out["X-Stainless-Lang"])
	require.Equal(t, "0.39.0", out["X-Stainless-Package-Version"])
	require.Equal(t, "MacOS", out["X-Stainless-OS"])
	require.Equal(t, "arm64", out["X-Stainless-Arch"])
	require.Equal(t, "node", out["X-Stainless-Runtime"])
	require.Equal(t, "20.0.0", out["X-Stainless-Runtime-Version"])
	require.Equal(t, "stream", out["x-stainless-helper-method"])

	encoded, err := json.Marshal(out)
	require.NoError(t, err)
	dump := string(encoded)
	require.NotContains(t, dump, "sk-secret-value")
	require.NotContains(t, dump, "session=abc")
	require.NotContains(t, dump, "sk-other")
	require.NotContains(t, dump, "leak-me")
	require.Equal(t, "application/json", out["Accept"])
	require.Equal(t, map[string]any{"present": true}, out["Other-Header-Present"])

	require.Equal(t, map[string]any{"present": true}, out["Authorization"])
	require.Equal(t, map[string]any{"present": true}, out["Cookie"])
	require.Equal(t, map[string]any{"present": true}, out["X-Api-Key"])
}

func TestSanitizeRequestAuditResponseHeaderMapPreservesRoundTrippedSSEContentType(t *testing.T) {
	const secret = "response-secret-canary"

	upstream := httpattempt.SanitizeResponseHeaders(http.Header{
		"Content-Type": {"text/event-stream; charset=utf-8"},
		"Set-Cookie":   {"session=" + secret},
		"X-Api-Secret": {secret},
	})
	persisted := SanitizeRequestAuditResponseHeaderMap(upstream)

	require.Equal(t, map[string]any{
		"Content-Type":             "text/event-stream",
		"Set-Cookie":               map[string]any{"present": true},
		"Sensitive-Header-Present": map[string]any{"present": true},
	}, persisted)
	encoded, err := json.Marshal(persisted)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), secret)
}

func TestSanitizeRequestAuditHeadersSnapshotKeepsAggregatePresenceMarkers(t *testing.T) {
	const (
		tokenCanary = "auth-token-canary"
		valueCanary = "custom-header-value-canary"
	)
	in := http.Header{
		"X-Auth-Token":    {tokenCanary},
		"X-Custom-Header": {valueCanary},
		"Authorization":   {"Bearer " + tokenCanary},
		"Accept":          {"application/json"},
	}
	require.Equal(t, map[string]any{
		"Accept":                   "application/json",
		"Authorization":            map[string]any{"present": true},
		"Sensitive-Header-Present": map[string]any{"present": true},
		"Other-Header-Present":     map[string]any{"present": true},
	}, SanitizeRequestAuditHeaders(in))

	snapshot := SanitizeRequestAuditHeadersSnapshot(in)
	require.Equal(t, http.Header{
		"Accept":                   {"application/json"},
		"Authorization":            {"present"},
		"Sensitive-Header-Present": {"present"},
		"Other-Header-Present":     {"present"},
	}, snapshot)
	require.Equal(t, SanitizeRequestAuditHeaders(in), SanitizeRequestAuditHeaders(snapshot),
		"the async snapshot must not drop aggregate presence that wire attempts retain")
	require.Equal(t, snapshot, SanitizeRequestAuditHeadersSnapshot(snapshot), "snapshot round trips must be idempotent")

	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	for _, canary := range []string{tokenCanary, valueCanary, "X-Auth-Token", "X-Custom-Header"} {
		require.NotContains(t, string(encoded), canary)
	}
}

func TestRequestAuditRecordKeepsAggregatePresenceMarkersFromClientSnapshot(t *testing.T) {
	const (
		tokenCanary = "auth-token-canary"
		nameCanary  = "x-custom-header-name-canary"
		valueCanary = "custom-header-value-canary"
	)
	snapshot := SanitizeRequestAuditHeadersSnapshot(http.Header{
		"X-Auth-Token":     {tokenCanary},
		"X-Custom-Header":  {valueCanary},
		"X-Stainless-Lang": {"go"},
	})
	require.Equal(t, http.Header{
		"X-Stainless-Lang":         {"go"},
		"Sensitive-Header-Present": {"present"},
		"Other-Header-Present":     {"present"},
	}, snapshot)

	wantHeaders := map[string]any{
		"X-Stainless-Lang":         "go",
		"Sensitive-Header-Present": map[string]any{"present": true},
		"Other-Header-Present":     map[string]any{"present": true},
	}
	rec := BuildRequestAuditRecord(RequestAuditInput{UsageLogID: 7, Headers: snapshot})
	require.Equal(t, wantHeaders, rec.Headers)

	// Wire attempts keep these markers; the top-level record must not disagree with them.
	attempt := SanitizeRequestAuditAttempt(RequestAuditAttempt{
		Stage:              RequestAuditStageWire,
		WireRequestHeaders: SanitizeRequestAuditHeaders(snapshot),
	})
	require.Equal(t, rec.Headers, attempt.WireRequestHeaders)

	persisted := SanitizeRequestAuditRecord(rec)
	require.Equal(t, wantHeaders, persisted.Headers, "the persistence boundary must keep the same markers")

	for label, value := range map[string]any{"record": rec, "persisted": persisted, "attempt": attempt} {
		encoded, err := json.Marshal(value)
		require.NoError(t, err)
		for _, canary := range []string{tokenCanary, nameCanary, valueCanary, "X-Auth-Token", "X-Custom-Header"} {
			require.NotContains(t, string(encoded), canary, "%s must not carry raw header names or values", label)
		}
	}
}

func TestSanitizeRequestAuditHeaderMapRejectsRawAggregateMarkerValues(t *testing.T) {
	require.Equal(t, map[string]any{
		"Other-Header-Present": map[string]any{"present": true},
	}, SanitizeRequestAuditHeaderMap(map[string]any{
		"Other-Header-Present": map[string]any{"present": true},
	}))
	require.Empty(t, SanitizeRequestAuditHeaderMap(map[string]any{
		"Other-Header-Present":     "raw-marker-canary",
		"Sensitive-Header-Present": false,
	}))
	require.Empty(t, SanitizeRequestAuditHeaderMap(map[string]any{
		"Sensitive-Header-Present": map[string]any{"present": true, "raw": "raw-marker-canary"},
	}))
	require.Empty(t, SanitizeRequestAuditResponseHeaderMap(map[string]any{
		"Other-Header-Present": "raw-marker-canary",
	}))
}

func TestAddRequestAuditUsageTokensUsesPositiveUsageCountersOnly(t *testing.T) {
	metadata := AddRequestAuditUsageTokens(RequestAuditMetadata{
		Tokens: map[string]int{"input_tokens": 999, "output_tokens": 999, "custom": 99},
	}, &UsageLog{InputTokens: 123, OutputTokens: 45})
	require.Equal(t, map[string]int{
		"input_tokens":  123,
		"output_tokens": 45,
	}, metadata.Tokens, "caller-supplied token facts are replaced with linked usage counters")

	metadata = AddRequestAuditUsageTokens(RequestAuditMetadata{}, &UsageLog{InputTokens: 0, OutputTokens: -1})
	require.Empty(t, metadata.Tokens, "unknown or invalid non-positive source counts are omitted")
}

func TestBuildRequestAuditRecordSanitizesTokenCountMetadata(t *testing.T) {
	rec := BuildRequestAuditRecord(RequestAuditInput{
		UsageLogID: 42,
		Metadata: RequestAuditMetadata{
			Status: map[string]int{"upstream": 200, "untrusted": 418},
			Bytes:  map[string]int64{"request": 12, "untrusted": 999},
			Tokens: map[string]int{
				"input_tokens":  12,
				"output_tokens": 0,
				"negative":      -1,
				"prompt_text":   777,
			},
		},
	})

	require.Empty(t, rec.Metadata.Status)
	require.Empty(t, rec.Metadata.Bytes)
	require.Equal(t, map[string]int{"input_tokens": 12}, rec.Metadata.Tokens, "zero and negative token counts are not known usage facts")
}

func TestSanitizeRequestAuditMetadataKeepsClientResponseStatusAndBytesOnly(t *testing.T) {
	metadata := SanitizeRequestAuditMetadata(RequestAuditMetadata{
		Routes: map[string]string{"inbound": "/v1/messages", "upstream": "/v1/messages"},
		Status: map[string]int{
			RequestAuditClientResponseKey: 599,
			"upstream":                    200,
			"untrusted":                   418,
		},
		Bytes: map[string]int64{
			RequestAuditClientResponseKey: 0,
			"request":                     12,
			"untrusted":                   999,
		},
		Tokens: map[string]int{"input_tokens": 3},
	})

	require.Equal(t, map[string]int{RequestAuditClientResponseKey: 599}, metadata.Status)
	require.Len(t, metadata.Status, 1, "only the client response phase may carry a status key")
	require.Equal(t, map[string]int64{RequestAuditClientResponseKey: 0}, metadata.Bytes,
		"an observed zero-byte client response is a fact, not missing data")
	require.Len(t, metadata.Bytes, 1, "only the client response phase may carry a bytes key")
	require.Equal(t, "/v1/messages", metadata.Routes["inbound"], "the status/bytes allowlist must not drop other phases")
	require.Equal(t, map[string]int{"input_tokens": 3}, metadata.Tokens)
}

func TestSanitizeRequestAuditMetadataClientResponseStatusBounds(t *testing.T) {
	for _, tc := range []struct {
		status int
		keep   bool
	}{
		{status: 100, keep: true},
		{status: 200, keep: true},
		{status: 599, keep: true},
		{status: 99, keep: false},
		{status: 600, keep: false},
		{status: 0, keep: false},
		{status: -1, keep: false},
	} {
		t.Run(fmt.Sprintf("status_%d", tc.status), func(t *testing.T) {
			metadata := SanitizeRequestAuditMetadata(RequestAuditMetadata{
				Status: map[string]int{RequestAuditClientResponseKey: tc.status},
				Bytes:  map[string]int64{RequestAuditClientResponseKey: 7},
			})
			if tc.keep {
				require.Equal(t, map[string]int{RequestAuditClientResponseKey: tc.status}, metadata.Status)
			} else {
				require.Empty(t, metadata.Status, "an out-of-range status is not an observed client response fact")
			}
			require.Equal(t, map[string]int64{RequestAuditClientResponseKey: 7}, metadata.Bytes,
				"status and bytes are allowlisted independently")
		})
	}
}

func TestSanitizeRequestAuditMetadataClientResponseBytesRejectsNegative(t *testing.T) {
	metadata := SanitizeRequestAuditMetadata(RequestAuditMetadata{
		Status: map[string]int{RequestAuditClientResponseKey: 201},
		Bytes:  map[string]int64{RequestAuditClientResponseKey: -1},
	})
	require.Equal(t, map[string]int{RequestAuditClientResponseKey: 201}, metadata.Status)
	require.Empty(t, metadata.Bytes, "a negative byte count is not an observed client response fact")
}

func TestBuildRequestAuditRecordPersistsOnlyClientResponseFacts(t *testing.T) {
	const (
		bodyCanary      = "client-response-body-canary"
		statusCanaryKey = "upstream_canary"
		bytesCanaryKey  = "prompt_bytes_canary"
	)
	rec := BuildRequestAuditRecord(RequestAuditInput{
		UsageLogID: 61,
		Metadata: RequestAuditMetadata{
			Status: map[string]int{RequestAuditClientResponseKey: 200, statusCanaryKey: 503},
			Bytes:  map[string]int64{RequestAuditClientResponseKey: 0, bytesCanaryKey: 4242},
		},
		Body: []byte(`{"content":"` + bodyCanary + `"}`),
	})

	require.Equal(t, map[string]int{RequestAuditClientResponseKey: 200}, rec.Metadata.Status)
	require.Equal(t, map[string]int64{RequestAuditClientResponseKey: 0}, rec.Metadata.Bytes)

	persisted := SanitizeRequestAuditRecord(rec)
	require.Equal(t, rec.Metadata.Status, persisted.Metadata.Status, "the disclosure boundary keeps the same facts")
	require.Equal(t, rec.Metadata.Bytes, persisted.Metadata.Bytes)

	encoded, err := json.Marshal(rec)
	require.NoError(t, err)
	dump := string(encoded)
	require.NotContains(t, dump, bodyCanary)
	require.NotContains(t, dump, statusCanaryKey)
	require.NotContains(t, dump, bytesCanaryKey)
}

func TestBuildRequestAuditRecordDropsTokenShapedSecretsFromProtocolMetadata(t *testing.T) {
	const secret = "sk-test-secret-canary"
	rec := BuildRequestAuditRecord(RequestAuditInput{
		UsageLogID: 42,
		Headers: http.Header{
			"X-Stainless-Lang":    []string{secret},
			"Authorization":       []string{"Bearer " + secret},
			"Proxy-Authorization": []string{"Basic " + secret},
		},
		Metadata: RequestAuditMetadata{
			Routes: map[string]string{"inbound": "/v1/messages?token=" + secret, "upstream": "/v1/responses"},
			IDs:    map[string]string{"session": secret, "response": secret},
			Status: map[string]int{"untrusted": 200},
			Bytes:  map[string]int64{"untrusted": 1},
		},
		Attempts:  []RequestAuditAttempt{{AccountID: 7, Model: secret, Protocol: RequestAuditProtocolAnthropic, Stage: RequestAuditStageWire}},
		SSEEvents: []RequestAuditSSEEvent{{Type: secret, Data: []byte("secret delta " + secret)}},
	})
	encoded, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), secret)
	require.Equal(t, map[string]any{"present": true}, rec.Headers["Authorization"])
	require.Equal(t, "unknown", rec.Events[0].Type)
	require.Equal(t, "", rec.Attempts[0].Model)
	require.Empty(t, rec.Metadata.IDs)
	require.Empty(t, rec.Metadata.Status)
	require.Empty(t, rec.Metadata.Bytes)
}

func TestSanitizeRequestAuditMetadataAllowsRequestFingerprintsOnly(t *testing.T) {
	const (
		localDigest    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		upstreamDigest = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
		localCanary    = "raw-local-request-id-canary"
		upstreamCanary = "raw-upstream-request-id-canary"
		unknownCanary  = "unknown-id-canary"
	)

	metadata := SanitizeRequestAuditMetadata(RequestAuditMetadata{IDs: map[string]string{
		"local_request_fingerprint":    localDigest,
		"upstream_request_fingerprint": upstreamDigest,
		"local_request_id":             localCanary,
		"upstream_request_id":          upstreamCanary,
		"unknown_identifier":           unknownCanary,
	}})

	require.Equal(t, map[string]string{
		"local_request_fingerprint":    localDigest,
		"upstream_request_fingerprint": upstreamDigest,
	}, metadata.IDs)
	encoded, err := json.Marshal(metadata)
	require.NoError(t, err)
	dump := string(encoded)
	require.NotContains(t, dump, localCanary)
	require.NotContains(t, dump, upstreamCanary)
	require.NotContains(t, dump, unknownCanary)
}

func TestBuildRequestAuditRecordSanitizesNotCapturedMetadata(t *testing.T) {
	const secret = "sk-test-secret-canary"
	stream := true
	rec := BuildRequestAuditRecord(RequestAuditInput{
		UsageLogID:        44,
		NotCapturedReason: RequestAuditNotCapturedReasonPhase1Uncovered,
		Metadata: RequestAuditMetadata{
			Routes: map[string]string{"inbound": "/v1/responses", "upstream": "/v1/responses/" + secret},
			IDs:    map[string]string{"session": secret},
			ProtocolFields: &RequestAuditProtocolFields{
				Stream:        &stream,
				ThinkingType:  "enabled",
				PresentFields: []string{"model", "tool_name:" + secret},
			},
		},
	})
	encoded, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), secret)
	require.Equal(t, "/v1/responses", rec.Metadata.Routes["inbound"])
	require.Empty(t, rec.Metadata.IDs)
	require.Nil(t, rec.Metadata.ProtocolFields)
}

func TestBuildRequestAuditRecordSanitizesProtocolFields(t *testing.T) {
	const canary = "raw-tool-name-canary"
	stream := false
	rec := BuildRequestAuditRecord(RequestAuditInput{
		UsageLogID: 45,
		Metadata: RequestAuditMetadata{
			ProtocolFields: &RequestAuditProtocolFields{
				Stream:           &stream,
				ThinkingType:     "disabled",
				PresentFields:    []string{"model", "messages", "input", "tools", "stream", "thinking", "tool_name:" + canary},
				NormalizedFields: []string{"thinking.extra_fields_removed", "thinking." + canary},
			},
		},
	})

	require.NotNil(t, rec.Metadata.ProtocolFields)
	require.NotNil(t, rec.Metadata.ProtocolFields.Stream)
	require.False(t, *rec.Metadata.ProtocolFields.Stream, "explicit false must remain distinguishable from unknown")
	require.Equal(t, "disabled", rec.Metadata.ProtocolFields.ThinkingType)
	require.Equal(t, []string{"model", "messages", "input", "tools", "stream", "thinking"}, rec.Metadata.ProtocolFields.PresentFields)
	require.Equal(t, []string{"thinking.extra_fields_removed"}, rec.Metadata.ProtocolFields.NormalizedFields)

	encoded, err := json.Marshal(rec)
	require.NoError(t, err)
	dump := string(encoded)
	require.NotContains(t, dump, canary)
	require.NotContains(t, dump, "tool_name")
	require.NotContains(t, dump, "thinking.raw-tool-name-canary")
}

func TestBuildRequestAuditRecordOmitsUnknownProtocolEnumsAndStreamState(t *testing.T) {
	rec := BuildRequestAuditRecord(RequestAuditInput{
		UsageLogID: 46,
		Metadata: RequestAuditMetadata{
			ProtocolFields: &RequestAuditProtocolFields{
				ThinkingType:  "adaptive-canary",
				PresentFields: []string{"model", "unknown-field-canary"},
			},
		},
	})

	require.NotNil(t, rec.Metadata.ProtocolFields)
	require.Nil(t, rec.Metadata.ProtocolFields.Stream)
	require.Empty(t, rec.Metadata.ProtocolFields.ThinkingType)
	require.Equal(t, []string{"model"}, rec.Metadata.ProtocolFields.PresentFields)
	require.Empty(t, rec.Metadata.ProtocolFields.NormalizedFields)
}

func TestSanitizeRequestAuditHeadersRejectsArbitraryValuesInAllowedNames(t *testing.T) {
	out := SanitizeRequestAuditHeaders(http.Header{
		"X-Stainless-Lang":            []string{"Bearer sk-secret prompt text"},
		"X-Stainless-Retry-Count":     []string{"0;Authorization=secret"},
		"X-Stainless-Runtime-Version": []string{strings.Repeat("x", 65)},
	})
	require.NotContains(t, out, "X-Stainless-Lang")
	require.NotContains(t, out, "X-Stainless-Retry-Count")
	require.NotContains(t, out, "X-Stainless-Runtime-Version")
}

func TestSanitizeRequestAuditHeadersOmitsMissingSecrets(t *testing.T) {
	out := SanitizeRequestAuditHeaders(http.Header{"X-Stainless-Lang": []string{"go"}})
	_, hasAuth := out["Authorization"]
	require.False(t, hasAuth)
	require.Equal(t, "go", out["X-Stainless-Lang"])
}

func TestRequestAuditFingerprintIsSaltedAndScopedToAuthenticatedUser(t *testing.T) {
	fingerprinter, err := NewRequestAuditFingerprinter(strings.Repeat("k", 32))
	require.NoError(t, err)

	first, err := fingerprinter.BeginForUser(7)
	require.NoError(t, err)
	second, err := fingerprinter.BeginForUser(7)
	require.NoError(t, err)
	otherUser, err := fingerprinter.BeginForUser(8)
	require.NoError(t, err)
	first.DigestRequest([]byte("same prompt"))
	second.DigestRequest([]byte("same prompt"))
	otherUser.DigestRequest([]byte("same prompt"))

	require.Len(t, first.RequestDigest, 64)
	require.NotEqual(t, first.RequestDigest, second.RequestDigest, "each logical request has a fresh record salt")
	require.NotEqual(t, first.RequestDigest, otherUser.RequestDigest, "digests cannot be correlated across authenticated users")
	require.Len(t, first.Salt(), 32)
	require.NotEqual(t, first.Salt(), second.Salt())
	firstSalt := first.Salt()
	firstSalt[0] ^= 0xff
	require.NotEqual(t, firstSalt, first.Salt(), "Salt returns a defensive copy")

	event := first.DigestEvent(0, "message_start", []byte("same event"))
	require.Len(t, event, 64)
	rec := &RequestAuditRecord{RequestFingerprint: first.RequestDigest, FingerprintKeyVersion: first.KeyVersion, FingerprintSalt: first.Salt()}
	require.Equal(t, first.RequestDigest, rec.RequestFingerprint)
	require.Equal(t, RequestAuditFingerprintKeyVersion, rec.FingerprintKeyVersion)
	dump, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NotContains(t, string(dump), "same prompt")
	require.NotContains(t, string(dump), "same event")
	require.NotContains(t, string(dump), "FingerprintSalt", "the per-record salt field is internal and not serialized")
	require.NotContains(t, string(dump), string(first.Salt()), "salt bytes are never serialized")
}

func TestRequestAuditFingerprinterRejectsInvalidUserID(t *testing.T) {
	fingerprinter, err := NewRequestAuditFingerprinter(strings.Repeat("k", 32))
	require.NoError(t, err)
	_, err = fingerprinter.BeginForUser(0)
	require.Error(t, err)
	_, err = fingerprinter.BeginForUser(-1)
	require.Error(t, err)
}

func TestBuildRequestAuditRecordOmitsModelBody(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"secret prompt"}],"thinking":{"type":"disabled"}}`)
	rec := BuildRequestAuditRecord(RequestAuditInput{
		UsageLogID: 42,
		Headers:    http.Header{"X-Stainless-Lang": []string{"js"}, "Authorization": []string{"Bearer sk-leak"}},
		Body:       body,
	})
	require.Equal(t, int64(42), rec.UsageLogID)
	require.Equal(t, "js", rec.Headers["X-Stainless-Lang"])
	encoded, err := json.Marshal(rec)
	require.NoError(t, err)
	dump := string(encoded)
	require.NotContains(t, dump, "secret prompt")
	require.NotContains(t, dump, "sk-leak")
	require.NotContains(t, dump, `"messages"`)
	require.NotContains(t, dump, "claude-sonnet-4")
}

func TestBuildRequestAuditRecordOmitsSSEDeltaText(t *testing.T) {
	rec := BuildRequestAuditRecord(RequestAuditInput{
		UsageLogID: 42,
		SSEEvents: []RequestAuditSSEEvent{
			{Type: "message_start", Data: []byte(`{"type":"message_start","message":{}}`)},
			{Type: "content_block_delta", Data: []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"secret delta"}}`)},
			{Type: "message_stop", Data: []byte(`{"type":"message_stop"}`)},
		},
	})
	require.Len(t, rec.Events, 3)
	require.Equal(t, "message_start", rec.Events[0].Type)
	require.Equal(t, 0, rec.Events[0].Index)
	require.Equal(t, "content_block_delta", rec.Events[1].Type)
	require.Equal(t, 1, rec.Events[1].Index)
	require.Equal(t, "message_stop", rec.Events[2].Type)
	require.Equal(t, 2, rec.Events[2].Index)
	require.Equal(t, len(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"secret delta"}}`), rec.Events[1].Bytes)
	encoded, err := json.Marshal(rec)
	require.NoError(t, err)
	dump := string(encoded)
	require.NotContains(t, dump, "secret delta")
	require.NotContains(t, dump, "text_delta")
	require.NotContains(t, dump, `"data"`)
}

func TestBuildRequestAuditRecordTruncatesSSEEventsFirstCome(t *testing.T) {
	events := make([]RequestAuditSSEEvent, 0, 2002)
	payload := []byte("x")
	for i := 0; i < 2001; i++ {
		events = append(events, RequestAuditSSEEvent{Type: "content_block_delta", Data: payload})
	}
	oversized := make([]byte, 300*1024)
	events = append(events, RequestAuditSSEEvent{Type: "content_block_delta", Data: oversized})

	rec := BuildRequestAuditRecord(RequestAuditInput{UsageLogID: 7, SSEEvents: events})
	require.NotEmpty(t, rec.Events)
	require.LessOrEqual(t, len(rec.Events), 2001)
	encoded, err := json.Marshal(rec)
	require.NoError(t, err)
	dump := string(encoded)
	require.NotContains(t, dump, string(oversized[:64]))
	require.Contains(t, dump, `"truncated"`)
	var truncatedCount int
	for _, ev := range rec.Events {
		if ev.Truncated {
			truncatedCount++
			require.Equal(t, "truncated", ev.Type)
		}
		require.NotContains(t, ev.Type, string(oversized[:8]))
	}
	require.GreaterOrEqual(t, truncatedCount, 1)
	require.Equal(t, RequestAuditCaptureTruncated, rec.CaptureCompleteness)
	tr := rec.Events[len(rec.Events)-1]
	require.Equal(t, "truncated", tr.Type)
	require.True(t, tr.Truncated)
	require.Equal(t, "max_events", tr.Reason)
	require.Equal(t, 2002, tr.Original)
	require.Equal(t, 2000, tr.Kept)
	require.Equal(t, 2, tr.Dropped)
	require.Equal(t, 2001+300*1024, tr.OriginalBytes)
	require.Equal(t, 2000, tr.KeptBytes)
	require.Equal(t, 1+300*1024, tr.DroppedBytes)
}

func TestBuildRequestAuditRecordDoesNotCapOnPayloadBytes(t *testing.T) {
	events := make([]RequestAuditSSEEvent, 100)
	for i := range events {
		events[i] = RequestAuditSSEEvent{Type: "content_block_delta", Bytes: 3 * 1024}
	}
	rec := BuildRequestAuditRecord(RequestAuditInput{UsageLogID: 9, SSEEvents: events})
	require.Equal(t, RequestAuditCaptureComplete, rec.CaptureCompleteness)
	require.Len(t, rec.Events, 100)
	require.NotEqual(t, "truncated", rec.Events[len(rec.Events)-1].Type)
}

func TestBuildRequestAuditRecordTruncatesWhenSkeletonJSONExceedsBudget(t *testing.T) {
	const n = 1999
	events := make([]RequestAuditSSEEvent, n)
	fp := &RequestAuditFingerprintInput{Events: make(map[int]string, n)}
	for i := range events {
		events[i] = RequestAuditSSEEvent{Type: "response.function_call_arguments.delta", Bytes: 3}
		fp.Events[i] = strings.Repeat("a", 64)
	}
	rec := BuildRequestAuditRecord(RequestAuditInput{UsageLogID: 10, SSEEvents: events, Fingerprint: fp})
	require.Equal(t, RequestAuditCaptureTruncated, rec.CaptureCompleteness)
	require.Greater(t, len(rec.Events), 1)
	require.Less(t, len(rec.Events), n)
	tr := rec.Events[len(rec.Events)-1]
	require.Equal(t, "truncated", tr.Type)
	require.True(t, tr.Truncated)
	require.Equal(t, "max_bytes", tr.Reason)
	kept := len(rec.Events) - 1
	require.Equal(t, n, tr.Original)
	require.Equal(t, kept, tr.Kept)
	require.Equal(t, n-kept, tr.Dropped)
	require.Equal(t, n*3, tr.OriginalBytes)
	require.Equal(t, kept*3, tr.KeptBytes)
	require.Equal(t, (n-kept)*3, tr.DroppedBytes)
	require.Less(t, kept, requestAuditMaxSSEEvents)
	keptJSON, err := json.Marshal(rec.Events[:kept])
	require.NoError(t, err)
	require.LessOrEqual(t, len(keptJSON), requestAuditMaxSSEBytes)
	fullJSON, err := json.Marshal(rec.Events)
	require.NoError(t, err)
	require.LessOrEqual(t, len(fullJSON), requestAuditMaxSSEBytes)
	encoded, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "secret")
}

func TestBuildRequestAuditRecordKeepsUpstreamAttemptsWithoutBody(t *testing.T) {
	rec := BuildRequestAuditRecord(RequestAuditInput{
		UsageLogID: 11,
		Headers:    http.Header{"X-Stainless-Lang": []string{"js"}, "Authorization": []string{"Bearer sk-leak"}},
		Body:       []byte(`{"messages":[{"role":"user","content":"secret prompt"}]}`),
		Attempts: []RequestAuditAttempt{
			{AccountID: 3001, Model: "gpt-5.1", Protocol: RequestAuditProtocolOpenAIChat, Stage: RequestAuditStageWire},
			{AccountID: 3002, Model: "gpt-5.1", Protocol: RequestAuditProtocolOpenAIChat, Stage: RequestAuditStageWire},
		},
	})
	require.Equal(t, int64(11), rec.UsageLogID)
	require.Len(t, rec.Attempts, 2)
	require.Equal(t, int64(3001), rec.Attempts[0].AccountID)
	require.Empty(t, rec.Attempts[0].Model)
	require.Equal(t, RequestAuditProtocolOpenAIChat, rec.Attempts[0].Protocol)
	require.Equal(t, RequestAuditStageWire, rec.Attempts[0].Stage)
	require.Equal(t, int64(3002), rec.Attempts[1].AccountID)
	encoded, err := json.Marshal(rec)
	require.NoError(t, err)
	dump := string(encoded)
	require.NotContains(t, dump, "secret prompt")
	require.NotContains(t, dump, "sk-leak")
	require.NotContains(t, dump, `"messages"`)
}

func TestBuildRequestAuditRecordMarksIncompleteOnStreamTermination(t *testing.T) {
	rec := BuildRequestAuditRecord(RequestAuditInput{
		UsageLogID:       8,
		StreamIncomplete: true,
		SSEEvents: []RequestAuditSSEEvent{
			{Type: "message_start", Bytes: 12},
			{Type: "content_block_delta", Bytes: 40},
		},
	})
	require.Equal(t, RequestAuditCaptureIncomplete, rec.CaptureCompleteness)
}

func TestBuildRequestAuditRecordMarksIncompleteOnClientDisconnect(t *testing.T) {
	rec := BuildRequestAuditRecord(RequestAuditInput{
		UsageLogID:       8,
		ClientDisconnect: true,
		SSEEvents: []RequestAuditSSEEvent{
			{Type: "message_start", Bytes: 12},
			{Type: "content_block_delta", Bytes: 40},
		},
	})
	require.Equal(t, RequestAuditCaptureIncomplete, rec.CaptureCompleteness)
	encoded, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "secret")
}

func TestAttachRequestAuditAfterUsageLogPersistsNotCapturedWithoutMetadata(t *testing.T) {
	repo := &stubRequestAuditRepo{}
	err := AttachRequestAuditAfterUsageLog(context.Background(), repo, &UsageLog{ID: 12}, RequestAuditInput{
		NotCapturedReason: RequestAuditNotCapturedReasonPhase1Uncovered,
		ClientDisconnect:  true,
		SSEEvents: []RequestAuditSSEEvent{{
			Type: "response.output_text.delta", Bytes: 42,
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, repo.created)
	require.Equal(t, int64(12), repo.created.UsageLogID)
	require.Equal(t, RequestAuditCaptureNotCaptured, repo.created.CaptureCompleteness)
	require.Equal(t, RequestAuditNotCapturedReasonPhase1Uncovered, repo.created.CaptureReason)
	require.Empty(t, repo.created.Headers)
	require.Empty(t, repo.created.Events)
	require.Empty(t, repo.created.Attempts)
}

func TestAttachRequestAuditAfterUsageLogUsesLinkedUsageTokenCountsOnly(t *testing.T) {
	repo := &stubRequestAuditRepo{}
	usage := &UsageLog{ID: 13, InputTokens: 123, OutputTokens: 45}
	// The record is only attached when the logical request captured something;
	// caller-supplied token counts are untrusted facts that never attach on their own.
	err := AttachRequestAuditAfterUsageLog(context.Background(), repo, usage, RequestAuditInput{
		Body:    []byte(`{"messages":[{"content":"do not persist this prompt"}]}`),
		Headers: http.Header{"X-Stainless-Lang": []string{"js"}},
		Metadata: RequestAuditMetadata{Tokens: map[string]int{
			"input_tokens":  999,
			"output_tokens": 999,
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, repo.created)
	require.Equal(t, map[string]int{"input_tokens": 123, "output_tokens": 45}, repo.created.Metadata.Tokens)
	encoded, err := json.Marshal(repo.created)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "do not persist this prompt")
}

func TestAttachRequestAuditAfterUsageLogWritesWhenIDPresent(t *testing.T) {
	repo := &stubRequestAuditRepo{}
	err := AttachRequestAuditAfterUsageLog(context.Background(), repo, &UsageLog{ID: 11}, RequestAuditInput{
		Headers: http.Header{
			"X-Stainless-Lang": []string{"js"},
			"Authorization":    []string{"Bearer sk-leak"},
		},
		Body: []byte(`{"messages":[{"content":"secret prompt"}]}`),
	})
	require.NoError(t, err)
	require.NotNil(t, repo.created)
	require.Equal(t, int64(11), repo.created.UsageLogID)
	require.Equal(t, "js", repo.created.Headers["X-Stainless-Lang"])
	require.Equal(t, map[string]any{"present": true}, repo.created.Headers["Authorization"])
	encoded, err := json.Marshal(repo.created)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "secret prompt")
	require.NotContains(t, string(encoded), "sk-leak")
}

func TestAttachRequestAuditAfterUsageLogSkipsWhenUsageIDZero(t *testing.T) {
	repo := &stubRequestAuditRepo{}
	err := AttachRequestAuditAfterUsageLog(context.Background(), repo, &UsageLog{ID: 0}, RequestAuditInput{
		Headers: http.Header{"X-Stainless-Lang": []string{"js"}},
	})
	require.NoError(t, err)
	require.Nil(t, repo.created)
}

func TestAttachRequestAuditAfterUsageLogRecordsWriteFailureWhenStoreRecovers(t *testing.T) {
	repo := &recoveringRequestAuditRepo{}
	err := AttachRequestAuditAfterUsageLog(context.Background(), repo, &UsageLog{ID: 91}, RequestAuditInput{
		Headers:   http.Header{"Authorization": []string{"Bearer sk-sensitive"}},
		SSEEvents: []RequestAuditSSEEvent{{Type: "message_delta", Data: []byte(`{"delta":"secret completion"}`)}},
	})
	require.NoError(t, err)
	require.Equal(t, 2, repo.calls)
	require.NotNil(t, repo.marker)
	require.Equal(t, int64(91), repo.marker.UsageLogID)
	require.Equal(t, RequestAuditCaptureWriteFailed, repo.marker.CaptureCompleteness)
	require.Equal(t, "audit_write_failed", repo.marker.CaptureReason)
	require.Empty(t, repo.marker.Headers)
	require.Empty(t, repo.marker.Events)
	require.Empty(t, repo.marker.Attempts)
	encoded, err := json.Marshal(repo.marker)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "secret completion")
	require.NotContains(t, string(encoded), "sk-sensitive")
}

type recoveringRequestAuditRepo struct {
	calls  int
	marker *RequestAuditRecord
}

func (s *recoveringRequestAuditRepo) CreateRequestAudit(_ context.Context, rec *RequestAuditRecord) error {
	s.calls++
	if s.calls == 1 {
		return fmt.Errorf("store temporarily unavailable")
	}
	s.marker = rec
	return nil
}

func (s *recoveringRequestAuditRepo) GetByUsageLogID(_ context.Context, usageLogID int64) (*RequestAuditRecord, error) {
	if s.marker != nil && s.marker.UsageLogID == usageLogID {
		return s.marker, nil
	}
	return nil, nil
}

func TestAttachRequestAuditAfterUsageLogFailOpen(t *testing.T) {
	before := RequestAuditOrdinaryWriteFailureCount()
	repo := &stubRequestAuditRepo{err: fmt.Errorf("audit store down")}
	err := AttachRequestAuditAfterUsageLog(context.Background(), repo, &UsageLog{ID: 9}, RequestAuditInput{
		Headers: http.Header{"X-Stainless-Lang": []string{"js"}},
	})
	require.NoError(t, err)
	require.NotNil(t, repo.created)
	require.Equal(t, int64(9), repo.created.UsageLogID)
	require.Equal(t, before+1, RequestAuditOrdinaryWriteFailureCount())
}

func TestRequestAuditModelDigestUsesOneRecordDomainAcrossStages(t *testing.T) {
	fingerprinter, err := NewRequestAuditFingerprinter(strings.Repeat("k", 32))
	require.NoError(t, err)
	first, err := fingerprinter.BeginForUser(7)
	require.NoError(t, err)
	sameRecord, err := fingerprinter.BeginForUser(7)
	require.NoError(t, err)
	otherUser, err := fingerprinter.BeginForUser(8)
	require.NoError(t, err)

	const sameAlias = "client-canary-model"
	firstDigest := first.DigestModel(sameAlias)
	require.Len(t, firstDigest, 64)
	require.Equal(t, firstDigest, first.DigestModel(sameAlias), "equal model values at different stages share the record-scoped digest")
	require.NotEqual(t, firstDigest, first.DigestModel("normalized-canary-model"), "changed model aliases produce different digests")
	require.NotEqual(t, firstDigest, sameRecord.DigestModel(sameAlias), "model digests cannot be correlated across records")
	require.NotEqual(t, firstDigest, otherUser.DigestModel(sameAlias), "model digests cannot be correlated across users")
}

func TestRequestAuditIdentifierDigestIsScopedAndDomainSeparated(t *testing.T) {
	fingerprinter, err := NewRequestAuditFingerprinter(strings.Repeat("k", 32))
	require.NoError(t, err)
	first, err := fingerprinter.BeginForUser(7)
	require.NoError(t, err)
	second, err := fingerprinter.BeginForUser(7)
	require.NoError(t, err)

	firstDigest := first.DigestIdentifier("session", "identifier-canary")
	require.Len(t, firstDigest, 64)
	require.Equal(t, firstDigest, first.DigestIdentifier("session", "identifier-canary"))
	require.NotEqual(t, firstDigest, first.DigestIdentifier("request", "identifier-canary"), "identifier kinds are domain separated")
	require.NotEqual(t, firstDigest, second.DigestIdentifier("session", "identifier-canary"), "identifier digests are record scoped")

	localRequestDigest := first.DigestIdentifier("local_request", "identifier-canary")
	upstreamRequestDigest := first.DigestIdentifier("upstream_request", "identifier-canary")
	require.Len(t, localRequestDigest, 64)
	require.Len(t, upstreamRequestDigest, 64)
	require.NotEqual(t, localRequestDigest, upstreamRequestDigest, "local and upstream identifiers are domain separated")
}

func TestRequestAuditIdentifierDigestRejectsUnsafeRequestIDs(t *testing.T) {
	fingerprinter, err := NewRequestAuditFingerprinter(strings.Repeat("k", 32))
	require.NoError(t, err)
	fp, err := fingerprinter.BeginForUser(7)
	require.NoError(t, err)

	for _, kind := range []string{"local_request", "upstream_request"} {
		t.Run(kind, func(t *testing.T) {
			require.NotEmpty(t, fp.DigestIdentifier(kind, strings.Repeat("x", 256)), "256-byte identifiers are accepted")
			require.Empty(t, fp.DigestIdentifier(kind, strings.Repeat("x", 257)), "identifiers over 256 bytes are rejected")
			require.Empty(t, fp.DigestIdentifier(kind, "unsafe\nidentifier"), "identifiers with control characters are rejected")
		})
	}
}

func TestSanitizeRequestAuditAttemptKeepsOnlyValidModelFingerprint(t *testing.T) {
	const valid = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	attempt := SanitizeRequestAuditAttempt(RequestAuditAttempt{
		Model:            "model-canary-must-not-persist",
		ModelFingerprint: valid,
		Protocol:         RequestAuditProtocolOpenAIChat,
		Stage:            RequestAuditStagePostNormalize,
	})
	require.Empty(t, attempt.Model)
	require.Equal(t, valid, attempt.ModelFingerprint)

	for _, invalid := range []string{
		"",
		"0123456789abcdef",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdeF",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde!",
	} {
		t.Run(fmt.Sprintf("reject_%q", invalid), func(t *testing.T) {
			clean := SanitizeRequestAuditAttempt(RequestAuditAttempt{
				ModelFingerprint: invalid,
				Protocol:         RequestAuditProtocolOpenAIChat,
				Stage:            RequestAuditStageClientEntry,
			})
			require.Empty(t, clean.ModelFingerprint)
		})
	}

	encoded, err := json.Marshal(attempt)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "model-canary-must-not-persist")
	require.Contains(t, string(encoded), valid)
}
