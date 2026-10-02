//go:build unit

package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTraceCollectorPreservesDistinctBodyStagesAndRedactsKnownCredentials(t *testing.T) {
	trace := NewRequestTraceCollector()
	inbound := RequestTraceBodyKey{Stage: "client_entry", View: "decoded"}
	outbound := RequestTraceBodyKey{Stage: "wire_request", AttemptIndex: 1, View: "wire"}
	response := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	client := RequestTraceBodyKey{Stage: "client_response", View: "downstream"}
	original := []byte(`{"metadata":{"user_id":"{\"device_id\":\"first\",\"account_uuid\":\"tenant\",\"session_id\":\"s1\"}"},"messages":[{"content":"prompt sentinel","nested":{"fallback_credit_token":"secret-canary"}}]}`)
	wire := []byte(`{"metadata":{"user_id":"{\"device_id\":\"outbound\",\"account_uuid\":\"tenant\",\"session_id\":\"s2\"}"},"messages":[{"content":"prompt sentinel"}]}`)
	for _, stage := range []struct {
		key  RequestTraceBodyKey
		body []byte
	}{{inbound, original}, {outbound, wire}, {response, []byte(`{"content":"upstream sentinel"}`)}, {client, []byte(`{"content":"client sentinel"}`)}} {
		trace.StartStage(stage.key, "application/json", false)
		trace.AppendStage(stage.key, stage.body)
		trace.FinishStage(stage.key, true)
	}
	snapshots := trace.Snapshots()
	require.Len(t, snapshots, 4)
	require.Equal(t, inbound, snapshots[0].Key)
	require.Equal(t, outbound, snapshots[1].Key)
	require.Equal(t, response, snapshots[2].Key)
	require.Equal(t, client, snapshots[3].Key)
	require.NotContains(t, string(snapshots[0].Payload), "secret-canary")
	require.Contains(t, string(snapshots[0].Payload), "prompt sentinel")
	require.Contains(t, string(snapshots[0].Payload), "first")
	require.NotContains(t, string(snapshots[0].Payload), "outbound")
	require.Contains(t, string(snapshots[1].Payload), "outbound")
	require.Contains(t, string(snapshots[2].Payload), "upstream sentinel")
	require.NotContains(t, string(snapshots[2].Payload), "client sentinel")
	require.Contains(t, string(snapshots[3].Payload), "client sentinel")
	snapshots[0].Payload[0] = '!'
	require.NotEqual(t, snapshots[0].Payload[0], trace.Snapshots()[0].Payload[0], "revealing must not alias collector memory")
}

func TestTraceRedactionHeadersMergeCaseVariantsWithoutLosingKnownCredential(t *testing.T) {
	header := http.Header{
		"authorization": {"Bearer sk-lower-secret"},
		"Authorization": {"Bearer sk-upper-secret"},
		"X-Trace":       {"diagnostic"},
	}
	result := RedactRequestTraceHeaders(header)
	require.Equal(t, []string{"[REDACTED]"}, result.Values.Values("Authorization"))
	require.Equal(t, "diagnostic", result.Values.Get("X-Trace"))
	require.NotContains(t, result.Values, "authorization")
}

func TestTraceFactValidationNeverTreatsCredentialValuesAsSafeHeaders(t *testing.T) {
	unsafe := &RequestTraceStageFacts{Method: "POST", RequestHeaders: http.Header{"X-Auth": {"Bearer synthetic-secret"}}}
	require.False(t, ValidRequestTraceStageFacts("client_metadata", unsafe))
	unsafe.RequestHeaders = http.Header{"X-Auth": {"[REDACTED]"}}
	require.True(t, ValidRequestTraceStageFacts("client_metadata", unsafe))
}

func TestTraceRedactionMasksCommonCredentialHeaderAliases(t *testing.T) {
	headers := http.Header{}
	for _, name := range []string{"Authentication", "X-Auth", "X-Authentication", "X-Signature", "X-Credential", "X-Bearer", "X-Proxy-Auth"} {
		headers.Set(name, "synthetic-header-secret")
	}
	redacted := RedactRequestTraceHeaders(headers)
	for name := range headers {
		require.Equal(t, "[REDACTED]", redacted.Values.Get(name), "header %s", name)
	}
}

func TestTraceRedactionOmitsUnverifiedCredentialPathSegments(t *testing.T) {
	u, err := url.Parse("https://synthetic.example/v1/secret/synthetic-path-secret/messages?source=demo")
	require.NoError(t, err)
	value, omitted := RedactRequestTraceURL(u)
	require.True(t, omitted)
	require.Empty(t, value)
}

func TestTraceRedactionRejectsAmbiguousOrOpaqueURLs(t *testing.T) {
	for _, raw := range []string{
		"https://example.test/path?api_key=first;feature=on",
		"https://example.test/path?api_key=first%26feature%3Don",
		"https://example.test/path?broken=%zz",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		redacted, omitted := RedactRequestTraceURL(u)
		if !omitted {
			require.NotContains(t, redacted, "first")
		}
	}
}

func TestTraceRedactionOmitsKnownSecretsWithoutDiscardingUnknownHeaders(t *testing.T) {
	header := http.Header{}
	header.Set("Authorization", "Bearer sk-head-sentinel")
	header.Set("Proxy-Authorization", "Basic sentinela")
	header.Set("Cookie", "sid=COOKIE_SENTINEL")
	header.Set("X-Api-Key", "key-sentinel")
	header.Set("X-Trace-Context", "diagnostic-marker")
	header.Set("Set-Cookie", "sid=SET_COOKIE_SENTINEL")
	safe := RedactRequestTraceHeaders(header)
	require.Equal(t, "[REDACTED]", safe.Values.Get("Authorization"))
	require.Equal(t, "[REDACTED]", safe.Values.Get("Proxy-Authorization"))
	require.Equal(t, "[REDACTED]", safe.Values.Get("Cookie"))
	require.Equal(t, "[REDACTED]", safe.Values.Get("X-Api-Key"))
	require.Equal(t, "[REDACTED]", safe.Values.Get("Set-Cookie"))
	require.Equal(t, "diagnostic-marker", safe.Values.Get("X-Trace-Context"))
	require.NotContains(t, safe.Values, "Sensitive-Header-Present")

	u, err := url.Parse("https://user:password@example.test/path?api_key=sk-query-sentinel&X-Amz-Signature=signature-sentinel&proxy=https%3A%2F%2Fu%3Ap%40proxy.test&feature=on")
	require.NoError(t, err)
	redacted, omitted := RedactRequestTraceURL(u)
	require.False(t, omitted)
	require.NotContains(t, redacted, "sk-query-sentinel")
	require.NotContains(t, redacted, "signature-sentinel")
	require.NotContains(t, redacted, "password")
	require.NotContains(t, redacted, "proxy.test")
	require.Contains(t, redacted, "feature=on")
}

func TestTraceCollectorBoundedAndMarksUnverifiedFragments(t *testing.T) {
	trace := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "client_entry", View: "decoded"}
	trace.StartStage(key, "application/json", false)
	trace.AppendStage(key, []byte(`{"messages":[{"content":"unterminated"}`))
	trace.FinishStage(key, false)
	first := trace.Snapshots()[0]
	require.Equal(t, "redaction_unverified", first.State)
	require.True(t, first.RedactionUnverified)
	require.Equal(t, "incomplete_read", first.Reason)

	long := bytes.Repeat([]byte("a"), RequestTraceBodyLimit+8192)
	other := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	trace.StartStage(other, "text/plain", false)
	trace.AppendStage(other, long)
	trace.FinishStage(other, true)
	result := trace.Snapshots()[1]
	require.LessOrEqual(t, len(result.Payload), RequestTraceBodyLimit)
	require.EqualValues(t, len(long), result.ObservedBytes)
	require.True(t, result.RedactionUnverified)
	require.Equal(t, "truncated_unverified", result.Reason)
	// The caller may reuse and overwrite the source slice after capture.
	copyBefore := append([]byte(nil), result.Payload...)
	long[0] = 'b'
	require.Equal(t, copyBefore, trace.Snapshots()[1].Payload)
}

func TestTraceCollectorSingleVerifiedUncompressedInboundViewDoesNotDoubleBudget(t *testing.T) {
	collector := NewRequestTraceCollector()
	body := []byte(`{"metadata":{"user_id":"synthetic"},"messages":[{"content":"hello"}]}`)
	collector.CaptureInboundViews("", body, int64(len(body)), body, int64(len(body)), true)
	views := collector.Snapshots()
	require.Len(t, views, 1)
	require.Equal(t, "client_entry", views[0].Key.Stage)
	require.Equal(t, "decoded", views[0].Key.View)
	require.Contains(t, string(views[0].Payload), "synthetic")
}

func TestTraceCollectorAllowsDecodeFailedCompressedRawAsExplicitUnverifiedException(t *testing.T) {
	trace := NewRequestTraceCollector()
	raw := []byte("corrupt-gzip-bytes")
	trace.CaptureInboundViews("gzip", raw, int64(len(raw)), nil, 0, false)
	stages := trace.Snapshots()
	require.Len(t, stages, 2)
	require.Equal(t, "redaction_unverified", stages[0].State)
	require.Contains(t, string(stages[0].Payload), "corrupt-gzip")
	require.Equal(t, "not_observed", stages[1].State)
}

func TestTraceCollectorOmitsRecoverableCompressedBytesAndCountsDecodedView(t *testing.T) {
	trace := NewRequestTraceCollector()
	decoded := []byte(`{"messages":[{"content":"hello"}],"credentials":{"api_key":"sk-body-sentinel"}}`)
	raw := []byte("ZIP_SENTINEL_REPRESENTS_COMPRESSED_SECRET")
	trace.CaptureInboundViews("gzip", raw, int64(len(raw)), decoded, int64(len(decoded)), true)
	stages := trace.Snapshots()
	require.Len(t, stages, 2)
	require.Equal(t, "credential_recoverable_omitted", stages[0].Reason)
	require.Empty(t, stages[0].Payload)
	require.EqualValues(t, len(raw), stages[0].ObservedBytes)
	require.NotContains(t, string(stages[1].Payload), "sk-body-sentinel")
	require.Contains(t, string(stages[1].Payload), "hello")

	trace = NewRequestTraceCollector()
	trace.CaptureInboundViews("gzip", raw, int64(len(raw)), []byte(`{"broken":`), 10, false)
	stages = trace.Snapshots()
	require.Len(t, stages, 2)
	require.Equal(t, "redaction_unverified", stages[0].State)
	require.Contains(t, string(stages[0].Payload), "ZIP_SENTINEL")
}

func TestTraceCollectorAllowsOnlyPersistableViewNames(t *testing.T) {
	trace := NewRequestTraceCollector()
	trace.StartStage(RequestTraceBodyKey{Stage: "client_entry", View: "unknown-view"}, "application/json", false)
	trace.AppendStage(RequestTraceBodyKey{Stage: "client_entry", View: "unknown-view"}, []byte(`{"content":"bad"}`))
	trace.FinishStage(RequestTraceBodyKey{Stage: "client_entry", View: "unknown-view"}, true)
	require.Empty(t, trace.Snapshots(), "unknown view cannot produce a record the database will reject")
}

func TestTraceCollectorSSESeparatesCRLFBetweenDistinctFrames(t *testing.T) {
	trace := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	trace.StartStage(key, "text/event-stream", true)
	trace.AppendStage(key, []byte("data: {\"delta\":\"first\"}\r\n\r"))
	trace.AppendStage(key, []byte("\ndata: {\"delta\":\"second\"}\r\n\r\n"))
	trace.FinishStage(key, true)
	result := trace.Snapshots()[0]
	require.Equal(t, 0, result.DroppedEvents)
	require.Equal(t, 2, bytes.Count(result.Payload, []byte("data: ")))
	require.Equal(t, 2, bytes.Count(result.Payload, []byte("\n\n")), "each frame must end independently")
	require.True(t, bytes.Contains(result.Payload, []byte("first")))
	require.True(t, bytes.Contains(result.Payload, []byte("second")))
}

func TestTraceCollectorSSESeparatesCRLFTerminatorAcrossAppends(t *testing.T) {
	trace := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	trace.StartStage(key, "text/event-stream", true)
	trace.AppendStage(key, []byte("data: {\"delta\":\"first\"}\r"))
	trace.AppendStage(key, []byte("\n\r"))
	trace.AppendStage(key, []byte("\ndata: {\"delta\":\"second\"}\r\n\r\n"))
	trace.FinishStage(key, true)
	result := trace.Snapshots()[0]
	require.Equal(t, 0, result.DroppedEvents)
	require.Equal(t, 2, bytes.Count(result.Payload, []byte("data: ")))
	require.Contains(t, string(result.Payload), "first")
	require.Contains(t, string(result.Payload), "second")
}

func TestTraceCollectorRejectsBinarySSEFrameEvenIfItSpoofsDataLine(t *testing.T) {
	collector := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	collector.StartStage(key, "text/event-stream", true)
	collector.AppendStage(key, []byte("data: {\"hello\":\"synthetic\"}\n\xff\n\n"))
	collector.FinishStage(key, true)
	stage := collector.Snapshots()[0]
	require.NotEqual(t, "stored", stage.State)
	require.True(t, stage.RedactionUnverified)
}

func TestTraceCollectorCompressedUpstreamSSEIsMarkedHighRiskNotVerified(t *testing.T) {
	collector := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	collector.StartStage(key, "text/event-stream", true)
	collector.AppendStage(key, []byte("\x1f\x8b\x08synthetic-compressed\n\n"))
	collector.FinishStage(key, true)
	stage := collector.Snapshots()[0]
	require.NotEqual(t, "stored", stage.State)
	require.True(t, stage.RedactionUnverified)
}

func TestTraceCollectorSSEPreservesControlEventsAndMarksUnverifiedText(t *testing.T) {
	collector := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	collector.StartStage(key, "text/event-stream", true)
	collector.AppendStage(key, []byte(": synthetic keepalive\n\n"))
	collector.AppendStage(key, []byte("event: ping\n\n"))
	collector.FinishStage(key, true)
	stage := collector.Snapshots()[0]
	require.Equal(t, "redaction_unverified", stage.State)
	require.True(t, stage.RedactionUnverified, "arbitrary keepalive text cannot be credential-verified")
	require.Contains(t, string(stage.Payload), "synthetic keepalive")
	require.Contains(t, string(stage.Payload), "event: ping")
}

func TestTraceCollectorSSERetainsProtocolUsageFieldTokens(t *testing.T) {
	trace := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	trace.StartStage(key, "text/event-stream", true)
	trace.AppendStage(key, []byte("event: message\ndata: {\"usage\":{\"input_tokens\":12,\"output_tokens\":3},\"api_key\":\"sk-sse-secret\"}\n\n"))
	trace.FinishStage(key, true)
	result := trace.Snapshots()[0]
	require.Contains(t, string(result.Payload), `"input_tokens":12`)
	require.Contains(t, string(result.Payload), `"output_tokens":3`)
	require.NotContains(t, string(result.Payload), "sk-sse-secret")
	require.Equal(t, "stored", result.State)
}

func TestTraceCollectorSSERecognizesTerminalDoneAsProtocolMarker(t *testing.T) {
	trace := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	trace.StartStage(key, "text/event-stream", true)
	trace.AppendStage(key, []byte("data: [DONE]\n\n"))
	trace.FinishStage(key, true)
	result := trace.Snapshots()[0]
	require.Equal(t, "stored", result.State)
	require.Equal(t, "retained", result.Reason)
	require.False(t, result.RedactionUnverified)
	require.Equal(t, "data: [DONE]\n\n", string(result.Payload))
}

func TestTraceCollectorSSEHandlesCRLFTerminator(t *testing.T) {
	trace := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	trace.StartStage(key, "text/event-stream", true)
	trace.AppendStage(key, []byte("event: message\r\ndata: {\"delta\":\"first\"}\r\n\r\n"))
	trace.FinishStage(key, true)
	result := trace.Snapshots()[0]
	require.Contains(t, string(result.Payload), "first")
	require.Equal(t, 0, result.DroppedEvents)
}

func TestTraceCollectorObservedEmptyBodyIsNotHighRisk(t *testing.T) {
	collector := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	collector.StartStage(key, "application/json", false)
	collector.FinishStage(key, true)
	snapshot := collector.Snapshots()[0]
	require.Equal(t, "not_observed", snapshot.State)
	require.Equal(t, "body_not_observed", snapshot.Reason)
	require.False(t, snapshot.RedactionUnverified, "zero bytes cannot contain an unverified credential")
}

func TestTraceCollectorBudgetOmissionDoesNotInventCredentialRisk(t *testing.T) {
	collector := NewRequestTraceCollector()
	for index := 1; index <= 7; index++ {
		key := RequestTraceBodyKey{Stage: "wire_request", AttemptIndex: index, View: "wire"}
		collector.StartStage(key, "application/json", false)
		collector.AppendStage(key, []byte(`{"text":"`+strings.Repeat("x", RequestTraceBodyLimit-16)+`"}`))
		collector.FinishStage(key, true)
	}
	key := RequestTraceBodyKey{Stage: "wire_request", AttemptIndex: 8, View: "wire"}
	collector.StartStage(key, "application/json", false)
	collector.AppendStage(key, []byte(`{"text":"synthetic"}`))
	collector.FinishStage(key, true)
	last := collector.Snapshots()[7]
	require.Positive(t, last.ObservedBytes)
	require.Equal(t, "truncated", last.State)
	require.Equal(t, "truncated", last.Reason)
	require.False(t, last.RedactionUnverified)
	require.Empty(t, last.Payload)
}

func TestTraceCollectorManyAttemptsStillRetainsFinalClientResponse(t *testing.T) {
	collector := NewRequestTraceCollector()
	for i := 1; i <= RequestTraceStageCountLimit; i++ {
		key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: i, View: "received"}
		collector.StartStage(key, "application/json", false)
		collector.AppendStage(key, []byte(`{"message":"synthetic"}`))
		collector.FinishStage(key, true)
	}
	client := RequestTraceBodyKey{Stage: "client_response", View: "downstream"}
	collector.StartStage(client, "application/json", false)
	collector.AppendStage(client, []byte(`{"result":"final"}`))
	collector.FinishStage(client, true)
	result := collector.Snapshots()
	require.Len(t, result, RequestTraceStageCountLimit+1)
	require.Equal(t, "client_response", result[len(result)-1].Key.Stage)
	require.Contains(t, string(result[len(result)-1].Payload), "final")
}

func TestTraceCollectorUnterminatedSSEFrameDoesNotInvalidateWholeTrace(t *testing.T) {
	for _, chunks := range [][][]byte{
		{[]byte("data: {\"delta\":1}\n")},
		{[]byte("data: {\"delta\":1}\n\n"), []byte("data: {\"delta\":2}\n")},
	} {
		collector := NewRequestTraceCollector()
		key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
		collector.StartStage(key, "text/event-stream", true)
		for _, chunk := range chunks {
			collector.AppendStage(key, chunk)
		}
		collector.FinishStage(key, true)
		snapshot := collector.Snapshots()[0]
		require.Equal(t, "truncated", snapshot.State)
		require.Equal(t, "incomplete_event", snapshot.Reason)
		require.Positive(t, snapshot.DroppedEvents)
		require.False(t, snapshot.RedactionUnverified)
	}
}

func TestTraceCollectorSSEKeepsOnlyCompleteEventsAndRedactsKnownFields(t *testing.T) {
	trace := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	trace.StartStage(key, "text/event-stream", true)
	trace.AppendStage(key, []byte("event: message\ndata: {\"delta\":\"first\",\"api_key\":\"sk-event-sentinel\"}\n\n"))
	trace.AppendStage(key, []byte("event: message\ndata: {\"delta\":\"second\"}\n\npartial"))
	trace.FinishStage(key, false)
	result := trace.Snapshots()[0]
	require.Contains(t, string(result.Payload), "first")
	require.Contains(t, string(result.Payload), "second")
	require.NotContains(t, string(result.Payload), "sk-event-sentinel")
	require.NotContains(t, string(result.Payload), "partial")
	require.Equal(t, 1, result.DroppedEvents)
	require.Equal(t, "incomplete_read", result.Reason)
	for _, item := range bytes.Split(result.Payload, []byte("\n\n")) {
		if len(item) == 0 {
			continue
		}
		require.Contains(t, string(item), "data: ")
	}
}

func TestTraceCollectorDoesNotAliasOrPersistArbitraryStageMetadata(t *testing.T) {
	trace := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "wire_request", AttemptIndex: 1, View: "wire"}
	trace.StartStage(key, "application/json", false)
	input := []byte(`{"metadata":{"user_id":"{\"device_id\":\"dev\"}"}}`)
	trace.AppendStage(key, input)
	trace.FinishStage(key, true)
	input[0] = '!'
	result := trace.Snapshots()[0]
	require.Contains(t, string(result.Payload), "device_id")
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "authorization")
}

func TestTraceCollectorOmitsCompressedRawWhenCredentialIsFoundDespiteOversizedDecodedView(t *testing.T) {
	trace := NewRequestTraceCollector()
	// A valid body larger than the collector prefix still contains a known
	// credential; the compressed original cannot be disclosed as a back door.
	decoded, _ := json.Marshal(map[string]any{"api_key": "sk-secret", "message": strings.Repeat("line\n", 210_000)})
	_, credentialDetected, _ := RedactRequestTraceJSON(decoded)
	require.True(t, credentialDetected, "full decoded JSON must detect the known credential")
	trace.CaptureInboundViews("gzip", []byte("compressed-secret"), 17, decoded, int64(len(decoded)), true)
	snapshots := trace.Snapshots()
	require.Len(t, snapshots, 2)
	require.Empty(t, snapshots[0].Payload, "known secret in decoded view must make the compressed source unreadable")
	require.NotContains(t, string(snapshots[1].Payload), "sk-secret")
}

func TestTraceCollectorSSEBudgetPreservesLaterStageReserve(t *testing.T) {
	trace := NewRequestTraceCollector()
	for attempt := 1; attempt <= 9; attempt++ {
		key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: attempt, View: "received"}
		trace.StartStage(key, "text/event-stream", true)
		// One large valid SSE event per attempt. Keep complete events only;
		// the reserve for the later client response must not be spent here.
		chunk := []byte("data: {\"delta\":\"" + strings.Repeat("a", RequestTraceBodyLimit-120) + "\"}\n\n")
		trace.AppendStage(key, chunk)
		trace.FinishStage(key, true)
	}
	clientKey := RequestTraceBodyKey{Stage: "client_response", View: "downstream"}
	trace.StartStage(clientKey, "application/json", false)
	client := []byte(`{"content":"` + strings.Repeat("z", RequestTraceBodyLimit-30) + `"}`)
	trace.AppendStage(clientKey, client)
	trace.FinishStage(clientKey, true)
	var bytesRetained int
	for _, snapshot := range trace.Snapshots() {
		bytesRetained += snapshot.RetainedBytes
	}
	require.LessOrEqual(t, bytesRetained, RequestTraceTotalBodyLimit)
	require.NotEmpty(t, trace.Snapshots()[len(trace.Snapshots())-1].Payload, "final client response retains its reserved budget")
}

func TestTraceCollectorDropsOversizedSSEEventButKeepsLaterCompleteEvent(t *testing.T) {
	trace := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	trace.StartStage(key, "text/event-stream", true)
	trace.AppendStage(key, []byte("data: "+strings.Repeat("A", RequestTraceBodyLimit+64)+"\n\n"))
	trace.AppendStage(key, []byte("data: {\"delta\":\"later\"}\n"))
	trace.AppendStage(key, []byte("\n"))
	trace.FinishStage(key, true)
	result := trace.Snapshots()[0]
	require.NotContains(t, string(result.Payload), strings.Repeat("A", 64))
	require.Contains(t, string(result.Payload), "later")
	require.Equal(t, 1, result.DroppedEvents)
}

func TestTraceCollectorDoesNotMarkVerifiedSSETruncationAsUnverified(t *testing.T) {
	trace := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	trace.StartStage(key, "text/event-stream", true)
	trace.AppendStage(key, []byte("data: {\"delta\":\""+strings.Repeat("a", RequestTraceBodyLimit)+"\"}\n\n"))
	trace.FinishStage(key, true)
	result := trace.Snapshots()[0]
	require.Equal(t, "truncated", result.State)
	require.Equal(t, "truncated", result.Reason)
	require.False(t, result.RedactionUnverified)
	require.Empty(t, result.Payload)
	require.Equal(t, 1, result.DroppedEvents)
}

func TestTraceCollectorDistinguishesIncompleteButRedactedJSONFromUnverifiedBytes(t *testing.T) {
	trace := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	trace.StartStage(key, "application/json", false)
	trace.AppendStage(key, []byte(`{"message":"visible","access_token":"known-secret"}`))
	trace.FinishStage(key, false)
	result := trace.Snapshots()[0]
	require.Equal(t, "incomplete_read", result.Reason)
	require.Equal(t, "truncated", result.State)
	require.False(t, result.RedactionUnverified, "valid redacted JSON is not unverified, even if the stream ended early")
	require.NotContains(t, string(result.Payload), "known-secret")
}

func TestTraceJSONRedactionRetainsBothClaudeUserIDStringFormats(t *testing.T) {
	for _, userID := range []string{
		"user_1111111111111111111111111111111111111111111111111111111111111111_account_11111111-1111-1111-1111-111111111111_session_22222222-2222-2222-2222-222222222222",
		`{"device_id":"synthetic-device","account_uuid":"11111111-1111-1111-1111-111111111111","session_id":"22222222-2222-2222-2222-222222222222"}`,
	} {
		original, err := json.Marshal(map[string]any{"metadata": map[string]any{"user_id": userID}, "api_key": "synthetic-secret"})
		require.NoError(t, err)
		redacted, credential, verified := RedactRequestTraceJSON(original)
		require.True(t, verified)
		require.True(t, credential)
		require.NotContains(t, string(redacted), "synthetic-secret")
		var value struct {
			Metadata struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		require.NoError(t, json.Unmarshal(redacted, &value))
		require.Equal(t, userID, value.Metadata.UserID)
	}
}

func TestTraceJSONRedactionPreservesProtocolUsageCountersOnly(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":3,` +
		`"cache_read_input_tokens":4,"total_tokens":5,"cached_tokens":6,"reasoning_tokens":7,` +
		`"accepted_prediction_tokens":8,"rejected_prediction_tokens":9},` +
		`"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":11},` +
		`"access_token":"synthetic-access-credential","refresh_token":"synthetic-refresh-credential"}`)
	redacted, found, verified := RedactRequestTraceJSON(body)
	require.True(t, verified)
	require.True(t, found)
	text := string(redacted)
	for _, counter := range []string{
		"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens",
		"total_tokens", "cached_tokens", "reasoning_tokens", "accepted_prediction_tokens",
		"rejected_prediction_tokens", "ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens",
	} {
		require.NotContains(t, text, `"`+counter+`":"[REDACTED]"`, "%s is a usage counter, not a credential", counter)
	}
	require.NotContains(t, text, "synthetic-access-credential")
	require.NotContains(t, text, "synthetic-refresh-credential")
}

// The counter allow-list must stay closed. A structural rule such as "name ends
// with tokens" would also exempt these plural credential names.
func TestTraceJSONRedactionStillRedactsPluralCredentialNames(t *testing.T) {
	body := []byte(`{"access_tokens":"synthetic-a","refresh_tokens":"synthetic-b",` +
		`"session_tokens":"synthetic-c","client_tokens":"synthetic-d",` +
		`"vendor_token_count":"synthetic-e","some_tokens":"synthetic-f"}`)
	redacted, found, verified := RedactRequestTraceJSON(body)
	require.True(t, verified)
	require.True(t, found)
	for _, secret := range []string{"synthetic-a", "synthetic-b", "synthetic-c", "synthetic-d", "synthetic-e", "synthetic-f"} {
		require.NotContains(t, string(redacted), secret)
	}
}

func TestTraceJSONRedactionPreservesClaudeThinkingBudgetTokens(t *testing.T) {
	payload := []byte(`{"max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	redacted, found, verified := RedactRequestTraceJSON(payload)
	require.True(t, verified)
	require.False(t, found)
	require.Contains(t, string(redacted), `"max_tokens":4096`)
	require.Contains(t, string(redacted), `"budget_tokens":2048`)
}

func TestTraceCollectorRedactsKnownJSONEvenWhenReadNotCompleted(t *testing.T) {
	trace := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: 1, View: "received"}
	trace.StartStage(key, "application/json", false)
	trace.AppendStage(key, []byte(`{"message":"received","api_key":"sk-incomplete-known"}`))
	trace.FinishStage(key, false)
	result := trace.Snapshots()[0]
	require.Equal(t, "incomplete_read", result.Reason)
	require.Contains(t, string(result.Payload), "received")
	require.NotContains(t, string(result.Payload), "sk-incomplete-known")
}

func TestTraceCollectorOmitsKnownCredentialWhenSanitizedJSONExceedsStageBudget(t *testing.T) {
	trace := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "wire_request", AttemptIndex: 1, View: "wire"}
	// Replacing tiny values with [REDACTED] expands the sanitized body beyond
	// the stage cap. The raw body is within that cap but cannot be retained.
	const n = 58_000
	body := []byte("[")
	for i := 0; i < n; i++ {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, []byte(`{"api_key":"s"}`)...)
	}
	body = append(body, ']')
	require.Less(t, len(body), RequestTraceBodyLimit)
	trace.StartStage(key, "application/json", false)
	trace.AppendStage(key, body)
	trace.FinishStage(key, true)
	result := trace.Snapshots()[0]
	require.Empty(t, result.Payload, "known credential must not fall back to raw bytes on sanitizer overflow")
	require.Equal(t, "credential_redaction_unverified", result.Reason)
}

func TestTraceCollectorSnapshotsNeverExposeArbitraryMetadata(t *testing.T) {
	trace := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "wire_request", AttemptIndex: 1, View: "wire"}
	trace.StartStage(key, "application/json", false)
	trace.AppendStage(key, []byte(`{"content":"legitimate"}`))
	trace.FinishStage(key, true)
	encoded, err := json.Marshal(trace.Snapshots())
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "metadata")
	require.NotContains(t, string(encoded), "authorization")
}

func TestTraceCollectorSSERetainedAndPendingBytesStayWithinTotalBudget(t *testing.T) {
	trace := NewRequestTraceCollector()
	const count = RequestTraceStageCountLimit
	for attempt := 1; attempt <= count; attempt++ {
		key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: attempt, View: "received"}
		trace.StartStage(key, "text/event-stream", true)
		trace.AppendStage(key, bytes.Repeat([]byte("Z"), RequestTraceBodyLimit-1))
	}
	// Include unfinished frames as well as committed payloads; otherwise each
	// pending stage is another 1 MiB, and 128 attempts consume 128 MiB.
	require.LessOrEqual(t, trace.MemoryBytes(), RequestTraceTotalBodyLimit)
}

func TestTraceCollectorDoesNotPermitUnboundedUniqueAttemptMetadata(t *testing.T) {
	trace := NewRequestTraceCollector()
	for attempt := 1; attempt <= 3000; attempt++ {
		key := RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: attempt, View: "received"}
		trace.StartStage(key, "application/json", false)
		trace.AppendStage(key, []byte(`{"message":"bounded"}`))
		trace.FinishStage(key, true)
	}
	snapshots := trace.Snapshots()
	require.LessOrEqual(t, len(snapshots), RequestTraceStageCountLimit)
}

// 可选实例预算只能调低单阶段上限：正文超过该预算时截断并标记，绝不绕过 1 MiB 硬上限。
func TestTraceCollectorOptionalBudgetLowersStageLimit(t *testing.T) {
	const budget = 64 << 10
	trace := NewRequestTraceCollector(budget)
	key := RequestTraceBodyKey{Stage: "client_response", View: "downstream"}
	trace.StartStage(key, "application/json", false)
	trace.AppendStage(key, []byte(`{"content":"`+strings.Repeat("a", budget+8192)+`"}`))
	trace.FinishStage(key, true)
	snapshots := trace.Snapshots()
	require.Len(t, snapshots, 1)
	require.LessOrEqual(t, len(snapshots[0].Payload), budget,
		"实例预算必须把单阶段留存压到预算以内")
	require.LessOrEqual(t, trace.MemoryBytes(), budget)
	require.Equal(t, "truncated", snapshots[0].State)
}

// 传入不小于硬上限的预算不得把上限调高；0／负数表示未指定，保持硬上限。
func TestTraceCollectorBudgetNeverRaisesHardStageLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget int64
	}{
		{"larger than hard limit", RequestTraceTotalBodyLimit},
		{"zero means unset", 0},
		{"negative means unset", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := NewRequestTraceCollector(tc.budget)
			require.Equal(t, RequestTraceBodyLimit, trace.bodyLimit)
			key := RequestTraceBodyKey{Stage: "client_entry", View: "decoded"}
			long := []byte(`{"content":"` + strings.Repeat("a", RequestTraceBodyLimit-16) + `"}`)
			trace.StartStage(key, "application/json", false)
			trace.AppendStage(key, long)
			trace.FinishStage(key, true)
			require.LessOrEqual(t, trace.MemoryBytes(), RequestTraceBodyLimit)
			require.LessOrEqual(t, trace.MemoryBytes(), RequestTraceTotalBodyLimit)
		})
	}
}

// 旧的无参构造行为不变：仍按 1 MiB 单阶段硬上限留存。
func TestTraceCollectorDefaultConstructorKeepsHardStageLimit(t *testing.T) {
	trace := NewRequestTraceCollector()
	require.Equal(t, RequestTraceBodyLimit, trace.bodyLimit)
}

func TestTraceCollectorCapsRetainedBytesAfterJSONReencoding(t *testing.T) {
	trace := NewRequestTraceCollector()
	key := RequestTraceBodyKey{Stage: "wire_request", AttemptIndex: 1, View: "wire"}
	// The numeric format is preserved even when the body is close to the cap.
	body := append([]byte(`{"content":"`), bytes.Repeat([]byte("a"), RequestTraceBodyLimit-40)...)
	body = append(body, []byte(`"}`)...)
	trace.StartStage(key, "application/json", false)
	trace.AppendStage(key, body)
	trace.FinishStage(key, true)
	snapshot := trace.Snapshots()[0]
	require.LessOrEqual(t, snapshot.RetainedBytes, RequestTraceBodyLimit)
	require.LessOrEqual(t, snapshot.RetainedBytes, RequestTraceTotalBodyLimit)
	require.Equal(t, len(snapshot.Payload), snapshot.RetainedBytes)
}

func TestTraceJSONRedactionKeepsProtocolTokenCounters(t *testing.T) {
	body := []byte(`{"max_tokens":4096,"input_tokens":17,"output_tokens":4,"fallback_credit_token":"sk-known","messages":[{"content":"hello"}]}`)
	redacted, known, verified := RedactRequestTraceJSON(body)
	require.True(t, verified)
	require.True(t, known)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(redacted, &parsed))
	require.Equal(t, float64(4096), parsed["max_tokens"])
	require.Equal(t, float64(17), parsed["input_tokens"])
	require.Equal(t, float64(4), parsed["output_tokens"])
	require.Equal(t, "[REDACTED]", parsed["fallback_credit_token"])
}

func TestTraceJSONRedactionDoesNotFlattenMetadataUserIDString(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"{\"device_id\":\"dev-1\",\"account_uuid\":\"acct-1\",\"session_id\":\"sess-1\"}"},"custom":{"client_secret":"hidden"}}`)
	redacted, hadCredential, verified := RedactRequestTraceJSON(body)
	require.True(t, verified)
	require.True(t, hadCredential)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(redacted, &parsed))
	metadata := parsed["metadata"].(map[string]any)
	require.Equal(t, `{"device_id":"dev-1","account_uuid":"acct-1","session_id":"sess-1"}`, metadata["user_id"])
	require.NotContains(t, string(redacted), "hidden")
	require.False(t, strings.Contains(string(redacted), "client_secret\":\"hidden"))
}

// 节点预算必须同时约束对象键与数组元素：数组不计数就等于没有上限，
// 一个只由数组组成的正文会绕过"有界 JSON 副本"的承诺。
func TestTraceJSONRedactionBudgetCoversArrayElements(t *testing.T) {
	body := []byte("[")
	for i := 0; i < 200_000; i++ {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, '1')
	}
	body = append(body, ']')
	require.Less(t, len(body), RequestTraceBodyLimit, "the fixture must fit the stage limit")

	redacted, _, verified := RedactRequestTraceJSON(body)
	require.False(t, verified, "an over-budget array must fail closed instead of being copied whole")
	require.Nil(t, redacted)
}
