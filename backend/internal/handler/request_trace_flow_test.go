//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 默认采集构造仅用于单元测试，生产路径始终使用请求入口冻结的配置。
func newRequestTraceFlow() *requestTraceFlow {
	return newRequestTraceFlowWithCapture(true, service.RequestTraceBodyLimit)
}

type countingTraceTransport struct{ base http.RoundTripper }

func (t *countingTraceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	sink := httpattempt.StartTraceAttempt(req)
	clone := req.Clone(req.Context())
	clone.Body = httpattempt.TraceRequestBody(req.Body, sink)
	resp, err := t.base.RoundTrip(clone)
	httpattempt.NotifyTraceRoundTrip(sink, resp, err)
	if resp != nil {
		resp.Body = httpattempt.TraceResponseBody(resp.Body, sink)
	}
	return resp, err
}

func TestRequestTraceAuthenticatedWithoutObservedWireDoesNotClaimComplete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &finalizingRequestTraceRepoStub{finalized: make(chan service.RequestTraceCaptureState, 1)}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	r := gin.New()
	r.POST("/v1/responses", RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return RequestTraceGateForCapture(true) }, repo, queue), func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{})
		BindRequestTraceAfterAuth()(c)
	}, func(c *gin.Context) {
		_, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
		require.NoError(t, err)
		c.Data(http.StatusOK, "application/json", []byte(`{"synthetic":"local-only"}`))
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"synthetic"}`)))
	waitForRequestTraceWrites(t, &repo.requestTraceWriterRepoStub, 3)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Equal(t, service.RequestTracePartial, repo.traces[0].CaptureState)
	var gap bool
	for _, stage := range repo.stages {
		if stage.Stage == "wire_attempt" && stage.Reason == "attempt_not_observed" {
			gap = true
		}
	}
	require.True(t, gap, "an unobserved upstream branch must leave an explicit gap")
	select {
	case <-repo.finalized:
		t.Fatal("unobserved attempt cannot finalize the trace")
	default:
	}
}

func TestRequestTraceIdentityVerdictPersistsOutcomeWithoutIdentityValue(t *testing.T) {
	flow := newRequestTraceFlow()
	const identityCanary = "user_synthetic_identity_CANARY"
	flow.identityVerdict(service.RequestTraceIdentityVerdictEvent{
		Verdict:      service.RequestTraceIdentityVerdictRewritten,
		Source:       service.RequestTraceDecisionSourceIdentity,
		AttemptIndex: 1, Reason: "identity_rewritten",
		InboundSeen: true, WireSeen: true, Changed: true,
	})
	flow.identityVerdict(service.RequestTraceIdentityVerdictEvent{
		Verdict:      service.RequestTraceIdentityVerdictNotSent,
		Source:       service.RequestTraceDecisionSourceProtocolConvert,
		AttemptIndex: 1, Reason: "identity_not_sent",
		InboundSeen: true, WireSeen: false, Changed: false,
	})
	stages := flow.finish(strings.Repeat("a", 32), true)
	var outcomes []service.RequestTraceDecisionOutcome
	encoded, err := json.Marshal(stages)
	require.NoError(t, err)
	for _, stage := range stages {
		if stage.Stage == service.RequestTraceDecisionStage && stage.Decision.Decision == service.RequestTraceDecisionIdentity {
			outcomes = append(outcomes, stage.Decision.Outcome)
		}
	}
	require.Equal(t, []service.RequestTraceDecisionOutcome{
		service.RequestTraceDecisionRewritten, service.RequestTraceDecisionNotSent,
	}, outcomes)
	require.NotContains(t, string(encoded), identityCanary)
}

func TestRequestTraceAccountSwitchDecisionIsNotInferredFromAttemptDiff(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	flow := newRequestTraceFlow()
	c.Set("request_trace_flow", flow)
	recordRequestTraceAccountSwitch(c, 41)
	stages := flow.finish(strings.Repeat("a", 32), true)
	var count int
	for _, stage := range stages {
		if stage.Stage == service.RequestTraceDecisionStage && stage.Decision.Decision == service.RequestTraceDecisionAccountSwitch {
			count++
			require.Equal(t, int64(41), stage.Decision.AccountID)
			require.Equal(t, service.RequestTraceDecisionRejected, stage.Decision.Outcome)
			require.Equal(t, "account_switch", stage.Reason)
		}
	}
	require.Equal(t, 1, count)
}

func TestRequestTraceChannelModelMappingRecordsOnlyObservedValidRewrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	flow := newRequestTraceFlow()
	c.Set("request_trace_flow", flow)
	recordRequestTraceChannelModelMapping(c, "synthetic-from", service.ChannelMappingResult{Mapped: true, MappedModel: "synthetic-to"})
	stages := flow.finish(strings.Repeat("a", 32), false)
	var mapped bool
	for _, stage := range stages {
		if stage.Stage == service.RequestTraceDecisionStage && stage.Decision.Decision == service.RequestTraceDecisionModelMapping {
			mapped = true
			require.Equal(t, "synthetic-from", stage.Decision.ModelFrom)
			require.Equal(t, "synthetic-to", stage.Decision.ModelTo)
		}
	}
	require.True(t, mapped)
}

func TestRequestTraceDecisionOverflowLeavesExplicitGap(t *testing.T) {
	flow := newRequestTraceFlow()
	for i := 0; i < service.RequestTraceStageCountLimit; i++ {
		flow.recordDecision(0, "route_selected", service.RequestTraceDecisionFacts{
			Decision: service.RequestTraceDecisionRoute,
			Outcome:  service.RequestTraceDecisionSelected,
			Source:   service.RequestTraceDecisionSourceInbound,
		})
	}
	stages := flow.finish(strings.Repeat("a", 32), false)
	var gap bool
	for _, stage := range stages {
		if stage.Stage == "capture_gap" && stage.Reason == "decision_budget_exceeded" {
			gap = true
			require.Positive(t, stage.DroppedEvents)
		}
	}
	require.True(t, gap)
}

func TestRequestTraceAuthAndRouteDecisionsSurviveQueueWithoutBodyRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &requestTraceWriterRepoStub{}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	r := gin.New()
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return RequestTraceGateForCapture(true) }, repo, queue), func(c *gin.Context) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "synthetic_reject"})
	})
	body := &unreadableTraceBody{}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("synthetic"))
	req.Body = body
	r.ServeHTTP(httptest.NewRecorder(), req)
	require.Zero(t, body.reads)
	waitForRequestTraceWrites(t, repo, 4)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	var kinds []service.RequestTraceDecisionKind
	for _, stage := range repo.stages {
		if stage.Stage == service.RequestTraceDecisionStage {
			require.NotNil(t, stage.Decision)
			kinds = append(kinds, stage.Decision.Decision)
		}
	}
	require.Equal(t, []service.RequestTraceDecisionKind{service.RequestTraceDecisionRoute, service.RequestTraceDecisionAuth}, kinds)
}

func TestRequestTraceVertexWireFactsAreUnsupportedWithoutClaimingFullCoverage(t *testing.T) {
	flow := newRequestTraceFlow()
	sink := flow.traceObserver().OnAttempt(httpattempt.TraceAttemptStart{
		Ordinal: 1, Method: http.MethodPost, Protocol: "anthropic.messages", ValueProtocol: "vertex",
	})
	sink.RequestBodyChunk([]byte(`{"anthropic_version":"synthetic"}`), io.EOF)
	sink.RoundTripResult(http.StatusOK, http.Header{"Content-Type": {"application/json"}}, nil)
	for _, stage := range flow.finish(strings.Repeat("a", 32), true) {
		if stage.Stage == "wire_attempt" {
			require.Equal(t, service.RequestTraceUnsupported, stage.State)
			require.Equal(t, "vertex", stage.Metadata.ValueProtocol)
			return
		}
	}
	t.Fatal("Vertex attempt missing")
}

func TestRequestTraceBedrockWireFactsAreUnsupportedWithoutClaimingFullCoverage(t *testing.T) {
	flow := newRequestTraceFlow()
	sink := flow.traceObserver().OnAttempt(httpattempt.TraceAttemptStart{
		Ordinal: 1, Method: http.MethodPost, Protocol: "anthropic.messages", ValueProtocol: "bedrock",
		Header: http.Header{"Authorization": {"AWS4-HMAC-SHA256 Credential=synthetic, Signature=synthetic"},
			"X-Amz-Security-Token": {"synthetic-session-secret"}},
	})
	sink.RequestBodyChunk([]byte(`{"anthropic_version":"synthetic"}`), io.EOF)
	sink.RoundTripResult(http.StatusOK, http.Header{"Content-Type": {"application/json"}}, nil)
	sink.ResponseBodyChunk([]byte(`{"content":"synthetic"}`), io.EOF)
	stages := flow.finish(strings.Repeat("a", 32), true)
	for _, stage := range stages {
		if stage.Stage == "wire_attempt" {
			require.Equal(t, service.RequestTraceUnsupported, stage.State)
			require.Equal(t, "wire_protocol_outside_phase1", stage.Reason)
			require.Equal(t, "bedrock", stage.Metadata.ValueProtocol)
			require.Equal(t, "[REDACTED]", stage.Metadata.RequestHeaders.Get("Authorization"))
			require.Equal(t, "[REDACTED]", stage.Metadata.RequestHeaders.Get("X-Amz-Security-Token"))
			return
		}
	}
	t.Fatal("Bedrock attempt missing")
}

func TestRequestTraceFailedWireTransportStillHasAttemptAndFinalResponse(t *testing.T) {
	flow := newRequestTraceFlow()
	sink := flow.traceObserver().OnAttempt(httpattempt.TraceAttemptStart{Ordinal: 1, Method: http.MethodPost, Protocol: "anthropic.messages"})
	sink.RequestBodyChunk([]byte(`{"prompt":"synthetic"}`), io.EOF)
	sink.RoundTripResult(0, nil, io.ErrUnexpectedEOF)
	flow.downstreamChunk([]byte(`{"error":"synthetic-transport"}`), http.StatusBadGateway)
	flow.setClientWrittenComplete(true)
	stages := flow.finish(strings.Repeat("a", 32), true)
	var foundAttempt, foundResponse bool
	for _, stage := range stages {
		if stage.Stage == "wire_attempt" {
			foundAttempt = stage.Reason == "transport_error" && stage.AttemptIndex == 1 && stage.Metadata != nil && stage.Metadata.Status == 0
		}
		if stage.Stage == "client_response" && bytes.Contains(stage.Payload, []byte("synthetic-transport")) {
			foundResponse = true
		}
	}
	require.True(t, foundAttempt)
	require.True(t, foundResponse)
}

func TestRequestTraceUpstreamStreamBreakAfterHeadersIsNotComplete(t *testing.T) {
	flow := newRequestTraceFlow()
	sink := flow.traceObserver().OnAttempt(httpattempt.TraceAttemptStart{
		Ordinal: 1, Method: http.MethodPost, Protocol: "anthropic.messages",
	})
	sink.RequestBodyChunk([]byte(`{"prompt":"synthetic"}`), io.EOF)
	sink.RoundTripResult(http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}}, nil)
	sink.ResponseBodyChunk([]byte("data: {\"delta\":\"first\"}\n\n"), nil)
	sink.ResponseBodyChunk(nil, io.ErrUnexpectedEOF)
	sink.ResponseBodyClosed(io.ErrUnexpectedEOF)
	var upstream *service.RequestTraceStage
	for _, stage := range flow.finish(strings.Repeat("a", 32), true) {
		if stage.Stage == "upstream_response" {
			copyStage := stage
			upstream = &copyStage
		}
	}
	require.NotNil(t, upstream)
	require.NotEqual(t, service.RequestTraceStored, upstream.State,
		"an upstream stream that broke before its terminal event is not complete")
	require.Contains(t, string(upstream.Payload), "first")
}

func TestRequestTraceAttemptEndedAtWaitsForResponseBodyEOF(t *testing.T) {
	flow := newRequestTraceFlow()
	sink := flow.traceObserver().OnAttempt(httpattempt.TraceAttemptStart{
		Ordinal: 1, Method: http.MethodPost, Protocol: "anthropic.messages",
	})
	sink.RoundTripResult(http.StatusOK, http.Header{"Content-Type": {"application/json"}}, nil)
	var headersAt time.Time
	flow.mu.Lock()
	require.Len(t, flow.attempts, 1)
	if flow.attempts[0].facts.EndedAt != nil {
		headersAt = *flow.attempts[0].facts.EndedAt
	}
	flow.mu.Unlock()
	require.True(t, headersAt.IsZero(), "response headers are TTFB, not the end of a streaming attempt")

	sink.ResponseBodyChunk([]byte(`{"response":"synthetic"}`), nil)
	sink.ResponseBodyChunk(nil, io.EOF)
	stages := flow.finish(strings.Repeat("a", 32), true)
	for _, stage := range stages {
		if stage.Stage == "wire_attempt" {
			require.NotNil(t, stage.Metadata)
			require.NotNil(t, stage.Metadata.EndedAt, "EOF must mark the actual response-read end")
			require.False(t, stage.Metadata.EndedAt.Before(*stage.Metadata.StartedAt))
			return
		}
	}
	t.Fatal("wire_attempt stage not found")
}

func TestRequestTraceEncodedUpstreamResponseReportsUnverifiedTransmittedBytes(t *testing.T) {
	flow := newRequestTraceFlow()
	sink := flow.traceObserver().OnAttempt(httpattempt.TraceAttemptStart{Ordinal: 1, Method: http.MethodPost, Protocol: "anthropic.messages"})
	sink.RoundTripResult(http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}, "Content-Encoding": {"gzip"}}, nil)
	sink.ResponseBodyChunk([]byte("\x1f\x8b\x08synthetic-compressed"), io.EOF)
	sink.ResponseBodyClosed(nil)
	for _, stage := range flow.finish(strings.Repeat("a", 32), true) {
		if stage.Stage == "upstream_response" {
			require.Equal(t, service.RequestTraceUnverified, stage.State)
			require.Equal(t, "encoded_body_unverified", stage.Reason)
			require.True(t, stage.RedactionUnverified)
			return
		}
	}
	t.Fatal("upstream response not observed")
}

func TestRequestTracePrematureUpstreamResponseDoesNotClaimWholeWireRequest(t *testing.T) {
	flow := newRequestTraceFlow()
	observer := flow.traceObserver()
	sink := observer.OnAttempt(httpattempt.TraceAttemptStart{
		Ordinal: 1, Method: http.MethodPost, Protocol: "anthropic.messages",
	})
	sink.RequestBodyChunk([]byte(`{"first":"complete"}`), nil)
	sink.RoundTripResult(http.StatusRequestEntityTooLarge, http.Header{}, nil)
	sink.RequestBodyClosed(nil)
	flow.collector.FinishStage(service.RequestTraceBodyKey{Stage: "wire_request", AttemptIndex: 1, View: "wire"}, false)
	var wire *service.RequestTraceBodySnapshot
	for _, snapshot := range flow.collector.Snapshots() {
		if snapshot.Key.Stage == "wire_request" {
			copy := snapshot
			wire = &copy
		}
	}
	require.NotNil(t, wire)
	require.NotEqual(t, "stored", wire.State, "a response status cannot prove the request body was fully transmitted")
}

func TestRequestTraceDecodeFailureIdentifiesDecodedViewGap(t *testing.T) {
	flow := newRequestTraceFlow()
	flow.inboundBody(httputil.InboundBodyObservation{
		ContentEncoding: "gzip", RawPrefix: []byte("broken-gzip"), RawBytes: 11,
		Outcome: httputil.InboundBodyDecodeFailed,
	})
	snapshots := flow.collector.Snapshots()
	require.Len(t, snapshots, 2)
	require.Equal(t, "decoded", snapshots[1].Key.View)
	require.Equal(t, "decode_failed", snapshots[1].Reason)
	require.Equal(t, "not_observed", snapshots[1].State)
}

func TestRequestTraceWireAttemptsExposeIndependentActualOutcomes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &requestTraceWriterRepoStub{}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"synthetic429"}`))
			return
		}
		_, _ = w.Write([]byte(`{"answer":"synthetic-ok"}`))
	}))
	defer upstream.Close()
	client := upstream.Client()
	client.Transport = &countingTraceTransport{base: client.Transport}
	r := gin.New()
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return RequestTraceGateForCapture(true) }, repo, queue), func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{})
		BindRequestTraceAfterAuth()(c)
	}, func(c *gin.Context) {
		for _, owner := range []int64{11, 22} {
			ctx := httpattempt.WithMetadata(c.Request.Context(), httpattempt.Metadata{
				AccountID: owner, Protocol: "anthropic.messages", Model: "synthetic-model",
			})
			out, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL,
				strings.NewReader(`{"model":"synthetic-model"}`))
			require.NoError(t, err)
			resp, err := client.Do(out)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, resp.Body)
			require.NoError(t, err)
			_ = resp.Body.Close()
		}
		c.Data(http.StatusOK, "application/json", []byte(`{"answer":"synthetic-ok"}`))
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	waitForRequestTraceWrites(t, repo, 5)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	var got []service.RequestTraceStage
	for _, stage := range repo.stages {
		if stage.Stage == "wire_attempt" {
			got = append(got, stage)
		}
	}
	require.Len(t, got, 2)
	require.Equal(t, []int{1, 2}, []int{got[0].AttemptIndex, got[1].AttemptIndex})
	require.Equal(t, []int{429, 200}, []int{got[0].Metadata.Status, got[1].Metadata.Status})
	require.Equal(t, []int64{11, 22}, []int64{got[0].Metadata.AccountID, got[1].Metadata.AccountID})
}

func TestRequestTraceHijackedResponseAlwaysShowsUnobservableGap(t *testing.T) {
	flow := newRequestTraceFlow()
	flow.markHijacked()
	for i := 1; i <= service.RequestTraceStageCountLimit; i++ {
		start := httpattempt.TraceAttemptStart{Ordinal: i, Method: "POST", Protocol: "anthropic.messages"}
		sink := flow.traceObserver().OnAttempt(start)
		if sink != nil {
			sink.RoundTripResult(http.StatusOK, http.Header{}, nil)
		}
	}
	stages := flow.finish(strings.Repeat("a", 32), true)
	require.Len(t, stages, service.RequestTraceStageCountLimit)
	var hijackGap, limitGap bool
	for _, stage := range stages {
		if stage.Reason == "hijacked_unobservable" {
			hijackGap = true
		}
		if stage.Reason == "stage_budget_exceeded" {
			limitGap = true
		}
	}
	require.True(t, hijackGap, "a hijacked connection cannot be reported without a gap")
	require.True(t, limitGap, "the stage budget must also be reported")
}

func TestRequestTraceManyAttemptsKeepFinalClientResponseAndExplicitGap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &requestTraceWriterRepoStub{}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		_, _ = w.Write([]byte(`{"answer":"synthetic"}`))
	}))
	defer upstream.Close()
	client := upstream.Client()
	client.Transport = &countingTraceTransport{base: client.Transport}
	r := gin.New()
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return RequestTraceGateForCapture(true) }, repo, queue), func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{})
		BindRequestTraceAfterAuth()(c)
	}, func(c *gin.Context) {
		for i := 0; i < 70; i++ {
			out, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL,
				strings.NewReader(`{"text":"synthetic"}`))
			require.NoError(t, err)
			resp, err := client.Do(out)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, resp.Body)
			require.NoError(t, err)
			_ = resp.Body.Close()
		}
		c.Data(http.StatusOK, "application/json", []byte(`{"final":"FINAL_RESPONSE_CANARY"}`))
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	require.Equal(t, http.StatusOK, w.Code)
	waitForRequestTraceWrites(t, repo, service.RequestTraceStageCountLimit)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Len(t, repo.stages, service.RequestTraceStageCountLimit)
	var response, gap bool
	for _, stage := range repo.stages {
		if stage.Stage == "client_response" && bytes.Contains(stage.Payload, []byte("FINAL_RESPONSE_CANARY")) {
			response = true
		}
		if stage.Stage == "capture_gap" && stage.Reason == "stage_budget_exceeded" {
			gap = true
		}
	}
	require.True(t, response, "later-stage reserve must preserve the final response")
	require.True(t, gap, "excess attempts must not disappear without an explicit gap")
	for _, stage := range repo.stages {
		if stage.Stage == "capture_gap" {
			require.Zero(t, stage.ObservedBytes, "the gap counts dropped stages, not network bytes")
			require.Positive(t, stage.DroppedEvents, "the gap must count omitted stages")
		}
	}
}

func TestRequestTraceStageBudgetGapCountsOnlyActuallyOmittedStages(t *testing.T) {
	flow := newRequestTraceFlow()
	for i := 1; i <= 70; i++ {
		// Each real attempt contributes a wire_attempt and a wire_request body
		// stage, so the 128-stage limit is exceeded without synthetic stage IDs.
		sink := flow.traceObserver().OnAttempt(httpattempt.TraceAttemptStart{
			Ordinal: i, Method: http.MethodPost, Protocol: "anthropic.messages",
		})
		require.NotNil(t, sink)
	}
	for i := 0; i < service.RequestTraceStageCountLimit/2+1; i++ {
		flow.recordDecision(0, "route_selected", service.RequestTraceDecisionFacts{
			Decision: service.RequestTraceDecisionRoute, Outcome: service.RequestTraceDecisionSelected,
			Source: service.RequestTraceDecisionSourceInbound,
		})
	}
	flow.downstreamChunk([]byte(`{"final":"FINAL_RESPONSE_CANARY"}`), 0)
	flow.markHijacked()
	const originalCount = 1 + 64 + 70 + 1 + 1 + 1 // metadata, decisions, attempts, downstream, decision gap, hijack gap
	stages := flow.finish(strings.Repeat("a", 32), true)
	require.Len(t, stages, service.RequestTraceStageCountLimit)
	var final, hijack, decisions, budget int
	for _, stage := range stages {
		switch stage.Reason {
		case "hijacked_unobservable":
			hijack++
		case "decision_budget_exceeded":
			decisions++
		case "stage_budget_exceeded":
			budget++
			// The gap itself is new, while the final response, hijack gap and
			// decision gap are moved (not dropped) from the original stage set.
			require.Equal(t, originalCount-(service.RequestTraceStageCountLimit-1), stage.DroppedEvents,
				"only original stages missing from the retained set count as dropped")
		}
		if stage.Stage == "client_response" && strings.Contains(string(stage.Payload), "FINAL_RESPONSE_CANARY") {
			final++
		}
	}
	require.Equal(t, 1, final, "preserve one actual downstream response, never duplicate it")
	require.Equal(t, 1, hijack)
	require.Equal(t, 1, decisions)
	require.Equal(t, 1, budget)
}

func TestRequestTraceClientDisconnectMidStreamIsNotClaimedComplete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &finalizingRequestTraceRepoStub{finalized: make(chan service.RequestTraceCaptureState, 1)}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	r := gin.New()
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return RequestTraceGateForCapture(true) }, repo, queue), func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{})
		BindRequestTraceAfterAuth()(c)
	}, func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.WriteString("data: {\"delta\":\"first\"}\n\n")
		c.Writer.Flush()
		// The client goes away before the upstream stream finishes.
		if cancel, ok := c.Request.Context().Value(cancelKey{}).(context.CancelFunc); ok {
			cancel()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(
		context.WithValue(ctx, cancelKey{}, context.CancelFunc(cancel)))
	r.ServeHTTP(httptest.NewRecorder(), req)
	waitForRequestTraceWrites(t, &repo.requestTraceWriterRepoStub, 3)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	var clientStage *service.RequestTraceStage
	for i := range repo.stages {
		if repo.stages[i].Stage == "client_response" {
			clientStage = &repo.stages[i]
		}
	}
	require.NotNil(t, clientStage)
	require.NotEqual(t, service.RequestTraceStored, clientStage.State,
		"a client that disconnected mid-stream cannot be reported as fully written")
	require.Equal(t, "incomplete_read", clientStage.Reason)
	select {
	case <-repo.finalized:
		t.Fatal("an interrupted client stream must not finalize the trace as complete")
	default:
	}
}

type cancelKey struct{}

func TestRequestTraceDownstreamSSEPreservesWholeEventsAndRedactsKnownKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &requestTraceWriterRepoStub{}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	r := gin.New()
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return RequestTraceGateForCapture(true) }, repo, queue), func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{})
		BindRequestTraceAfterAuth()(c)
	}, func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.WriteString("event: message\ndata: {\"api_key\":\"synthetic-secret\",\"text\":\"safe\"}\n\n")
		c.Writer.Flush()
		_, _ = c.Writer.WriteString("data: [DONE]\n\n")
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	waitForRequestTraceWrites(t, repo, 1)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	var found bool
	for _, stage := range repo.stages {
		if stage.Stage == "client_response" && len(stage.Payload) > 0 {
			found = true
			require.NotContains(t, string(stage.Payload), "synthetic-secret")
			require.Contains(t, string(stage.Payload), "[DONE]")
			require.Equal(t, service.RequestTraceStored, stage.State)
		}
	}
	require.True(t, found)
}

type finalizingRequestTraceRepoStub struct {
	requestTraceWriterRepoStub
	finalized chan service.RequestTraceCaptureState
}

func (r *finalizingRequestTraceRepoStub) FinalizeRequestTraceCapture(_ context.Context, _ string, state service.RequestTraceCaptureState) error {
	r.finalized <- state
	return nil
}

func TestRequestTraceCompleteFactsAreFinalizedOnlyAfterQueueWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &finalizingRequestTraceRepoStub{finalized: make(chan service.RequestTraceCaptureState, 1)}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	r := gin.New()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		_, _ = w.Write([]byte(`{"result":"synthetic"}`))
	}))
	defer upstream.Close()
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return RequestTraceGateForCapture(true) }, repo, queue), func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{})
		BindRequestTraceAfterAuth()(c)
	}, func(c *gin.Context) {
		body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
		require.NoError(t, err)
		out, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, bytes.NewReader(body))
		require.NoError(t, err)
		client := upstream.Client()
		client.Transport = &countingTraceTransport{base: client.Transport}
		resp, err := client.Do(out)
		require.NoError(t, err)
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		_ = resp.Body.Close()
		c.Data(http.StatusOK, "application/json", data)
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"hello":"synthetic"}`))
	r.ServeHTTP(httptest.NewRecorder(), request)
	waitForRequestTraceWrites(t, &repo.requestTraceWriterRepoStub, 5)
	repo.mu.Lock()
	require.Len(t, repo.traces, 1)
	require.Equal(t, service.RequestTracePartial, repo.traces[0].CaptureState, "queue creates a partial row pending finalization")
	repo.mu.Unlock()
	select {
	case state := <-repo.finalized:
		require.Equal(t, service.RequestTraceStored, state)
	case <-time.After(time.Second):
		t.Fatal("complete trace was never finalized")
	}
}

func TestRequestTracePersistsRedactedClientAndRealAttemptFacts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &requestTraceWriterRepoStub{}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Canary", "upstream-visible")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	r := gin.New()
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return RequestTraceGateForCapture(true) }, repo, queue), func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{})
		BindRequestTraceAfterAuth()(c)
	}, func(c *gin.Context) {
		body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
		require.NoError(t, err)
		ctx := httpattempt.WithMetadata(c.Request.Context(), httpattempt.Metadata{
			AccountID: 73, Model: "synthetic-model", Protocol: "anthropic.messages",
		})
		out, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/wire?api_key=WIRE_QUERY_SECRET", bytes.NewReader(body))
		require.NoError(t, err)
		out.Header.Set("Authorization", "Bearer WIRE_AUTH_SECRET")
		out.Header.Set("X-Wire-Canary", "wire-visible")
		client := upstream.Client()
		client.Transport = &countingTraceTransport{base: client.Transport}
		resp, err := client.Do(out)
		require.NoError(t, err)
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		_ = resp.Body.Close()
		c.Data(http.StatusOK, "application/json", data)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages?api_key=INBOUND_QUERY_SECRET&source=synthetic", strings.NewReader(`{"metadata":{"user_id":"user_synthetic_account_00000000-0000-0000-0000-000000000001_session_00000000-0000-0000-0000-000000000002"}}`))
	req.Header.Set("Authorization", "Bearer INBOUND_AUTH_SECRET")
	req.Header.Set("X-Inbound-Canary", "inbound-visible")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	waitForRequestTraceWrites(t, repo, 6)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	var clientMetadata, wireAttempt service.RequestTraceStage
	for _, stage := range repo.stages {
		switch stage.Stage {
		case "client_metadata":
			clientMetadata = stage
		case "wire_attempt":
			wireAttempt = stage
		}
		require.NotContains(t, string(stage.Payload), "INBOUND_AUTH_SECRET")
		require.NotContains(t, string(stage.Payload), "WIRE_AUTH_SECRET")
		require.NotContains(t, string(stage.Payload), "WIRE_QUERY_SECRET")
		require.NotContains(t, string(stage.Payload), "INBOUND_QUERY_SECRET")
	}
	require.NotNil(t, clientMetadata.Metadata)
	require.Equal(t, "POST", clientMetadata.Metadata.Method)
	require.Contains(t, clientMetadata.Metadata.URL, "api_key=%5BREDACTED%5D")
	require.Equal(t, "inbound-visible", clientMetadata.Metadata.RequestHeaders.Get("X-Inbound-Canary"))
	require.Equal(t, "[REDACTED]", clientMetadata.Metadata.RequestHeaders.Get("Authorization"))
	require.NotNil(t, wireAttempt.Metadata)
	require.Equal(t, "POST", wireAttempt.Metadata.Method)
	require.Equal(t, "anthropic.messages", wireAttempt.Metadata.Protocol)
	require.EqualValues(t, 73, wireAttempt.Metadata.AccountID)
	require.Equal(t, 200, wireAttempt.Metadata.Status)
	require.Equal(t, "synthetic-model", wireAttempt.Metadata.Model)
	require.Equal(t, "wire-visible", wireAttempt.Metadata.RequestHeaders.Get("X-Wire-Canary"))
	require.Equal(t, "upstream-visible", wireAttempt.Metadata.ResponseHeaders.Get("X-Upstream-Canary"))
	require.Equal(t, "[REDACTED]", wireAttempt.Metadata.RequestHeaders.Get("Authorization"))
	require.NotNil(t, wireAttempt.Metadata.StartedAt)
	require.NotNil(t, wireAttempt.Metadata.EndedAt)
}

// 实际选中的上游账号平台必须作为 wire_attempt 的请求时事实落库：
// 采集范围按它判定，管理端也按它检索，账号后来更换平台不改变这条历史。
func TestRequestTraceRecordsSelectedAccountPlatformOnWireAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &requestTraceWriterRepoStub{}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	r := gin.New()
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return RequestTraceGateForCapture(true) }, repo, queue), func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{})
		BindRequestTraceAfterAuth()(c)
	}, func(c *gin.Context) {
		// 这是各入口在选到账号之后的同一个调用点。
		markRequestTraceSelectedPlatform(c, service.PlatformAntigravity)
		body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
		require.NoError(t, err)
		ctx := httpattempt.WithMetadata(c.Request.Context(), httpattempt.Metadata{AccountID: 73, Protocol: "anthropic.messages"})
		out, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, bytes.NewReader(body))
		require.NoError(t, err)
		client := upstream.Client()
		client.Transport = &countingTraceTransport{base: client.Transport}
		resp, err := client.Do(out)
		require.NoError(t, err)
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		_ = resp.Body.Close()
		c.Data(http.StatusOK, "application/json", data)
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"hello":"synthetic"}`))
	r.ServeHTTP(httptest.NewRecorder(), request)
	waitForRequestTraceWrites(t, repo, 4)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	var found bool
	for _, stage := range repo.stages {
		if stage.Stage != "wire_attempt" || stage.Metadata == nil {
			continue
		}
		found = true
		require.Equal(t, service.PlatformAntigravity, stage.Metadata.Platform,
			"the wire attempt must carry the platform that was actually selected")
	}
	require.True(t, found, "a real attempt must have been observed")
}

// 正文关闭：四路正文都不创建 slot／payload，但尝试元信息与 EOF／Close 结束时间仍在。
func TestRequestTraceFlowBodyCaptureDisabledKeepsMetadataOnly(t *testing.T) {
	flow := newRequestTraceFlowWithCapture(false, service.RequestTraceBodyLimit)
	// 入站正文观察回调即使被调用，也不得留下任何字节。
	flow.inboundBody(httputil.InboundBodyObservation{
		Outcome:       httputil.InboundBodyComplete,
		DecodedPrefix: []byte(`{"prompt":"TRACE_PROMPT_CANARY"}`), DecodedBytes: 31,
	})
	sink := flow.traceObserver().OnAttempt(httpattempt.TraceAttemptStart{
		Ordinal: 1, Method: http.MethodPost, Protocol: "anthropic.messages", ValueProtocol: "messages",
		Header: http.Header{"Content-Type": {"application/json"}},
	})
	sink.RequestBodyChunk([]byte(`{"prompt":"TRACE_PROMPT_CANARY"}`), io.EOF)
	sink.RoundTripResult(http.StatusOK, http.Header{"Content-Type": {"application/json"}}, nil)
	sink.ResponseBodyChunk([]byte(`{"answer":"TRACE_RESPONSE_CANARY"}`), io.EOF)
	flow.downstreamChunk([]byte(`{"answer":"TRACE_RESPONSE_CANARY"}`), http.StatusOK)

	stages := flow.finish(strings.Repeat("a", 32), true)
	for _, stage := range stages {
		require.Empty(t, stage.Payload, "正文关闭不得留下任何 payload: %s/%s", stage.Stage, stage.View)
		require.Zero(t, stage.RetainedBytes)
		if stage.Stage == "wire_request" || stage.Stage == "upstream_response" ||
			(stage.Stage == "client_response" && stage.View == "downstream") {
			t.Fatalf("正文关闭不应出现正文阶段 %s/%s", stage.Stage, stage.View)
		}
	}
	encoded, err := json.Marshal(stages)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "TRACE_PROMPT_CANARY")
	require.NotContains(t, string(encoded), "TRACE_RESPONSE_CANARY")

	var disabled, attempt bool
	for _, stage := range stages {
		if stage.Stage == "client_entry" && stage.Reason == "capture_body_disabled" {
			disabled = true
		}
		if stage.Stage == "wire_attempt" {
			attempt = true
			require.NotNil(t, stage.Metadata, "尝试元信息必须保留")
			require.Equal(t, http.StatusOK, stage.Metadata.Status, "状态码必须保留")
			require.NotNil(t, stage.Metadata.EndedAt, "响应 EOF 的结束时间不得因关闭正文而丢失")
		}
	}
	require.True(t, disabled, "应以非失败原因码 capture_body_disabled 表达仅元信息")
	require.True(t, attempt)
}

// HTTP 200 总开关：只跳过客户端最终状态恰好 200 的整条 Trace，201／204／4xx／5xx 保留。
func TestRequestTraceHTTP200SwitchDropsOnlyFinal200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gate := func() service.RequestTraceGate {
		return service.RequestTraceGate{CaptureAllowed: true, Scope: service.RequestTraceSettings{
			CaptureBody: true, CaptureHTTP200: false, SampleRateHTTP200: 100, SampleRateOther: 100,
			BodyMaxBytes: service.RequestTraceBodyLimit,
			AllGroups:    true, ModelScope: service.RequestTraceScopeAll, PlatformScope: service.RequestTraceScopeAll,
		}}
	}
	for _, tc := range []struct {
		status int
		want   int
	}{{http.StatusOK, 0}, {http.StatusCreated, 1}, {http.StatusNoContent, 1}, {http.StatusNotFound, 1}, {http.StatusInternalServerError, 1}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			repo := &gateSnapshotTraceRepo{}
			queue := service.NewRequestTraceCaptureQueue(repo)
			defer queue.Stop()
			r := gin.New()
			r.POST("/v1/messages",
				RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return gate() }, repo, queue),
				func(c *gin.Context) { c.Status(tc.status) },
			)
			r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("body")))
			if tc.want > 0 {
				waitForGateSnapshotTraces(t, repo, tc.want)
			} else {
				// 用 Stop 排空队列后再断言，而不是靠 sleep 的"时间上没发生"：
				// 队列若真的入队了，Stop 会等它落库，断言因此是确定性的。
				queue.Stop()
			}
			require.Len(t, repo.storedTraces(), tc.want)
		})
	}
}

// 采样：0% 全跳过、100% 全保留，且 HTTP 200 与非 200 各自独立。
func TestRequestTraceSamplingByFinalStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gate := func(http200, other int) service.RequestTraceGate {
		return service.RequestTraceGate{CaptureAllowed: true, Scope: service.RequestTraceSettings{
			CaptureBody: true, CaptureHTTP200: true, SampleRateHTTP200: http200, SampleRateOther: other,
			BodyMaxBytes: service.RequestTraceBodyLimit,
			AllGroups:    true, ModelScope: service.RequestTraceScopeAll, PlatformScope: service.RequestTraceScopeAll,
		}}
	}
	for _, tc := range []struct {
		name                 string
		status               int
		http200, other, want int
	}{
		{"200 dropped by 0% http200 rate", http.StatusOK, 0, 100, 0},
		{"non-200 kept by 100% other rate", http.StatusBadRequest, 0, 100, 1},
		{"non-200 dropped by 0% other rate", http.StatusBadRequest, 100, 0, 0},
		{"200 kept by 100% http200 rate", http.StatusOK, 100, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &gateSnapshotTraceRepo{}
			queue := service.NewRequestTraceCaptureQueue(repo)
			defer queue.Stop()
			r := gin.New()
			r.POST("/v1/messages",
				RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return gate(tc.http200, tc.other) }, repo, queue),
				func(c *gin.Context) { c.Status(tc.status) },
			)
			r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("body")))
			if tc.want > 0 {
				waitForGateSnapshotTraces(t, repo, tc.want)
			} else {
				// 用 Stop 排空队列后再断言，而不是靠 sleep 的"时间上没发生"：
				// 队列若真的入队了，Stop 会等它落库，断言因此是确定性的。
				queue.Stop()
			}
			require.Len(t, repo.storedTraces(), tc.want)
		})
	}
}

// 端到端：正文关闭 + 已认证 + 没有真实缺口（本地 mock 应答）时，metadata-only 采集
// 必须走到既有的 stored 状态并被队列 finalize；同时确实没有任何正文阶段。
func TestRequestTraceBodyDisabledMetadataOnlyReachesStored(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &finalizingRequestTraceRepoStub{finalized: make(chan service.RequestTraceCaptureState, 1)}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()

	gate := func(context.Context) service.RequestTraceGate {
		return service.RequestTraceGate{CaptureAllowed: true, Scope: service.RequestTraceSettings{
			CaptureBody: false, CaptureHTTP200: true, SampleRateHTTP200: 100, SampleRateOther: 100,
			BodyMaxBytes: service.RequestTraceBodyLimit,
			AllGroups:    true, ModelScope: service.RequestTraceScopeAll, PlatformScope: service.RequestTraceScopeAll,
		}}
	}
	r := gin.New()
	r.POST("/v1/messages",
		RequestTraceCaptureMiddleware(gate, repo, queue),
		func(c *gin.Context) {
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 61, UserID: 71})
			BindRequestTraceAfterAuth()(c)
		},
		func(c *gin.Context) {
			recordLocalMockTraceDecision(c)
			c.JSON(http.StatusOK, gin.H{"ok": true})
		},
	)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("body")))

	select {
	case state := <-repo.finalized:
		require.Equal(t, service.RequestTraceStored, state,
			"完整 metadata-only 采集必须使用既有 stored 状态")
	case <-time.After(2 * time.Second):
		t.Fatal("body-disabled metadata-only trace must finalize as stored")
	}

	repo.mu.Lock()
	defer repo.mu.Unlock()
	var disabled bool
	for _, stage := range repo.stages {
		require.Empty(t, stage.Payload, "正文关闭不得留下任何 payload")
		require.NotEqual(t, "wire_request", stage.Stage)
		require.NotEqual(t, "upstream_response", stage.Stage)
		if stage.Stage == "client_entry" && stage.Reason == "capture_body_disabled" {
			disabled = true
		}
	}
	require.True(t, disabled)
}

func TestRequestTraceAuthenticatedFlowHasInboundAndWireAndFinalStages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &requestTraceWriterRepoStub{}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	r := gin.New()
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return RequestTraceGateForCapture(true) }, repo, queue), func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{})
		BindRequestTraceAfterAuth()(c)
	}, func(c *gin.Context) {
		body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
		require.NoError(t, err)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			_, _ = io.Copy(io.Discard, req.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"answer":"TRACE_RESPONSE_CANARY"}`))
		}))
		defer upstream.Close()
		out, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, bytes.NewReader(body))
		require.NoError(t, err)
		client := upstream.Client()
		client.Transport = &countingTraceTransport{base: client.Transport}
		resp, err := client.Do(out)
		require.NoError(t, err)
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		_ = resp.Body.Close()
		c.Data(http.StatusOK, "application/json", data)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"metadata":{"user_id":"{\"device_id\":\"dev\",\"session_id\":\"sid\"}"},"messages":[{"content":"TRACE_PROMPT_CANARY"}]}`))
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		repo.mu.Lock()
		count := len(repo.stages)
		repo.mu.Unlock()
		if count > 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.GreaterOrEqual(t, len(repo.stages), 3)
	var inbound, wireRequest, upstreamResponse, finalResponse bool
	for _, stage := range repo.stages {
		switch stage.Stage {
		case "client_entry":
			inbound = bytes.Contains(stage.Payload, []byte("TRACE_PROMPT_CANARY"))
		case "wire_request":
			wireRequest = bytes.Contains(stage.Payload, []byte("TRACE_PROMPT_CANARY"))
		case "upstream_response":
			upstreamResponse = bytes.Contains(stage.Payload, []byte("TRACE_RESPONSE_CANARY"))
		case "client_response":
			finalResponse = bytes.Contains(stage.Payload, []byte("TRACE_RESPONSE_CANARY"))
		}
	}
	require.True(t, inbound, "client body must be captured after authentication")
	require.True(t, wireRequest, "wire body must reflect bytes actually sent upstream")
	require.True(t, upstreamResponse, "real upstream response must be attributed to the attempt")
	require.True(t, finalResponse, "downstream body must reflect bytes written to the client")
}
