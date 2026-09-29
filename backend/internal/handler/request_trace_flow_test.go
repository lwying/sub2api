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
	r.POST("/v1/responses", RequestTraceCaptureMiddleware(func(context.Context) bool { return true }, repo, queue), func(c *gin.Context) {
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
		Verdict: service.RequestTraceIdentityVerdictRewritten,
		Source:  service.RequestTraceDecisionSourceIdentity,
		AttemptIndex: 1, Reason: "identity_rewritten",
		InboundSeen: true, WireSeen: true, Changed: true,
	})
	flow.identityVerdict(service.RequestTraceIdentityVerdictEvent{
		Verdict: service.RequestTraceIdentityVerdictNotSent,
		Source:  service.RequestTraceDecisionSourceProtocolConvert,
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
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) bool { return true }, repo, queue), func(c *gin.Context) {
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
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) bool { return true }, repo, queue), func(c *gin.Context) {
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
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) bool { return true }, repo, queue), func(c *gin.Context) {
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

func TestRequestTraceClientDisconnectMidStreamIsNotClaimedComplete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &finalizingRequestTraceRepoStub{finalized: make(chan service.RequestTraceCaptureState, 1)}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	r := gin.New()
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) bool { return true }, repo, queue), func(c *gin.Context) {
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
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) bool { return true }, repo, queue), func(c *gin.Context) {
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
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) bool { return true }, repo, queue), func(c *gin.Context) {
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
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) bool { return true }, repo, queue), func(c *gin.Context) {
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

func TestRequestTraceAuthenticatedFlowHasInboundAndWireAndFinalStages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &requestTraceWriterRepoStub{}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	r := gin.New()
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) bool { return true }, repo, queue), func(c *gin.Context) {
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
