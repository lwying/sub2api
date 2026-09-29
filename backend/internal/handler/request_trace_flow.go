package handler

import (
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type requestTraceFlow struct {
	collector             *service.RequestTraceCollector
	mu                    sync.Mutex
	attempts              []requestTraceAttemptFacts
	hijacked              bool
	clientStream          bool
	clientWrittenComplete bool
	clientFacts           service.RequestTraceStageFacts
	rejectReason          string
	decisions             []requestTraceDecisionEvent
	decisionDropped       int
}

type requestTraceAttemptFacts struct {
	index    int
	facts    service.RequestTraceStageFacts
	hadError bool
}

type requestTraceDecisionEvent struct {
	attemptIndex int
	reason       string
	facts        service.RequestTraceDecisionFacts
}

func newRequestTraceFlow() *requestTraceFlow {
	return &requestTraceFlow{collector: service.NewRequestTraceCollector()}
}

func (f *requestTraceFlow) inboundFacts(method string, url *url.URL, headers http.Header) {
	if f == nil {
		return
	}
	facts := service.NewRequestTraceClientFacts(method, url, headers)
	f.mu.Lock()
	f.clientFacts = facts
	f.mu.Unlock()
}

func (f *requestTraceFlow) recordDecision(attemptIndex int, reason string, facts service.RequestTraceDecisionFacts) {
	if f == nil || len(reason) == 0 || len(reason) > 96 || attemptIndex < 0 || attemptIndex > 1000 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.decisions) >= service.RequestTraceStageCountLimit/2 {
		f.decisionDropped++
		return
	}
	facts.Sequence = len(f.decisions) + 1
	if facts.DecidedAt == nil {
		facts.DecidedAt = service.RequestTraceDecisionTimestamp(time.Now())
	}
	if !service.ValidRequestTraceDecisionFacts(&facts) {
		f.decisionDropped++
		return
	}
	f.decisions = append(f.decisions, requestTraceDecisionEvent{attemptIndex: attemptIndex, reason: reason, facts: facts})
}

// RecordRequestTraceDecision records only an actually observed, bounded gateway
// decision. Callers must never synthesize it by diffing inbound and wire bodies.
func RecordRequestTraceDecision(c *gin.Context, attemptIndex int, reason string, facts service.RequestTraceDecisionFacts) {
	if c == nil {
		return
	}
	if value, exists := c.Get("request_trace_flow"); exists {
		if flow, ok := value.(*requestTraceFlow); ok {
			flow.recordDecision(attemptIndex, reason, facts)
		}
	}
}

// identityVerdict persists an identity rewrite verdict decided at a frozen wire
// seam. The verdict carries presence and equality booleans only: no identity
// value, body byte or header reaches the Trace.
func (f *requestTraceFlow) identityVerdict(event service.RequestTraceIdentityVerdictEvent) {
	if f == nil {
		return
	}
	facts, ok := service.RequestTraceIdentityVerdictDecisionFacts(event)
	if !ok {
		return
	}
	reason := event.Reason
	if !service.ValidRequestTraceStageReason(reason) {
		reason = "identity_" + string(event.Verdict)
	}
	f.recordDecision(event.AttemptIndex, reason, facts)
}

func recordRequestTraceAccountSwitch(c *gin.Context, accountID int64) {
	if accountID <= 0 {
		return
	}
	// The switch branch itself is the evidence: this account was abandoned for
	// the logical request and the gateway moved to another one. A different
	// account ID between two attempts alone would not prove that decision.
	RecordRequestTraceDecision(c, 0, "account_switch", service.RequestTraceDecisionFacts{
		Decision:  service.RequestTraceDecisionAccountSwitch,
		Outcome:   service.RequestTraceDecisionRejected,
		Source:    service.RequestTraceDecisionSourceAccount,
		AccountID: accountID,
	})
}

func recordRequestTraceChannelModelMapping(c *gin.Context, requested string, mapping service.ChannelMappingResult) {
	if !mapping.Mapped || requested == mapping.MappedModel {
		return
	}
	RecordRequestTraceDecision(c, 0, "model_rewritten", service.RequestTraceDecisionFacts{
		Decision:  service.RequestTraceDecisionModelMapping,
		Outcome:   service.RequestTraceDecisionRewritten,
		Source:    service.RequestTraceDecisionSourceGroup,
		ModelFrom: requested, ModelTo: mapping.MappedModel,
	})
}

func (f *requestTraceFlow) setRejectReason(reason string) {
	if f == nil || len(reason) > 96 {
		return
	}
	f.mu.Lock()
	f.rejectReason = reason
	f.mu.Unlock()
}

func (f *requestTraceFlow) inboundBody(ob httputil.InboundBodyObservation) {
	if f == nil || f.collector == nil {
		return
	}
	f.collector.CaptureInboundViews(ob.ContentEncoding, ob.RawPrefix, ob.RawBytes, ob.DecodedPrefix,
		ob.DecodedBytes, ob.Outcome == httputil.InboundBodyComplete)
	if ob.Outcome == httputil.InboundBodyDecodeFailed || ob.Outcome == httputil.InboundBodyReadFailed {
		reason := string(ob.Outcome)
		f.collector.MarkStageGap(service.RequestTraceBodyKey{Stage: "client_entry", View: "decoded"}, reason)
	}
}

func (f *requestTraceFlow) traceObserver() *httpattempt.TraceObserver {
	if f == nil || f.collector == nil {
		return nil
	}
	return &httpattempt.TraceObserver{OnAttempt: func(start httpattempt.TraceAttemptStart) httpattempt.TraceAttemptSink {
		if start.Ordinal < 1 || start.Ordinal > 1000 {
			return nil
		}
		started := time.Now().UTC()
		facts := requestTraceAttemptFacts{
			index: start.Ordinal,
			facts: service.NewRequestTraceAttemptFacts(start.Method, start.URL, start.Header, nil,
				start.AccountID, start.Model, start.Protocol, start.ValueProtocol, 0, &started, nil),
		}
		f.mu.Lock()
		f.attempts = append(f.attempts, facts)
		f.mu.Unlock()
		key := service.RequestTraceBodyKey{Stage: "wire_request", AttemptIndex: start.Ordinal, View: "wire"}
		f.collector.StartStage(key, start.Header.Get("Content-Type"), false)
		return &requestTraceAttemptSink{flow: f, index: start.Ordinal, requestKey: key,
			responseKey: service.RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: start.Ordinal, View: "received"}}
	}}
}

type requestTraceAttemptSink struct {
	flow            *requestTraceFlow
	index           int
	requestKey      service.RequestTraceBodyKey
	responseKey     service.RequestTraceBodyKey
	requestEOF      bool
	responseEOF     bool
	responseStarted bool
	responseEncoded bool
}

func (s *requestTraceAttemptSink) RequestBodyChunk(chunk []byte, err error) {
	if s == nil || s.flow == nil {
		return
	}
	s.flow.collector.AppendStage(s.requestKey, chunk)
	if err == io.EOF {
		s.requestEOF = true
		s.flow.collector.FinishStage(s.requestKey, true)
	}
}

func (s *requestTraceAttemptSink) RequestBodyClosed(error) {
	if s != nil && s.flow != nil {
		s.flow.collector.FinishStage(s.requestKey, s.requestEOF)
	}
}

func (s *requestTraceAttemptSink) RoundTripResult(status int, headers http.Header, err error) {
	if s == nil || s.flow == nil {
		return
	}
	s.flow.mu.Lock()
	for i := range s.flow.attempts {
		if s.flow.attempts[i].index == s.index {
			facts := &s.flow.attempts[i].facts
			redacted := service.RedactRequestTraceHeaders(headers)
			facts.ResponseHeaders = redacted.Values
			facts.ResponseHeadersOmitted = redacted.Omitted
			if status >= 100 && status <= 599 {
				facts.Status = status
			}
			ended := time.Now().UTC()
			facts.EndedAt = &ended
			facts.TrimToBudget()
			s.flow.attempts[i].hadError = err != nil
			break
		}
	}
	s.flow.mu.Unlock()
	// An upstream status does not prove the request body reached its EOF: a
	// server can answer 413 before consuming the submitted payload.
	s.flow.collector.FinishStage(s.requestKey, s.requestEOF)
	if status > 0 {
		s.responseStarted = true
		encoded := strings.TrimSpace(headers.Get("Content-Encoding"))
		s.responseEncoded = encoded != "" && !strings.EqualFold(encoded, "identity")
		s.flow.collector.StartStage(s.responseKey, headers.Get("Content-Type"),
			strings.HasPrefix(strings.ToLower(headers.Get("Content-Type")), "text/event-stream") &&
				(encoded == "" || strings.EqualFold(encoded, "identity")))
	}
}

func (s *requestTraceAttemptSink) ResponseBodyChunk(chunk []byte, err error) {
	if s == nil || s.flow == nil {
		return
	}
	if !s.responseStarted {
		return
	}
	s.flow.collector.AppendStage(s.responseKey, chunk)
	if err == io.EOF {
		s.responseEOF = true
		s.flow.collector.FinishStage(s.responseKey, true)
		if s.responseEncoded {
			s.flow.collector.MarkStageEncoding(s.responseKey)
		}
	}
}

func (s *requestTraceAttemptSink) ResponseBodyClosed(error) {
	if s != nil && s.flow != nil && s.responseStarted {
		s.flow.collector.FinishStage(s.responseKey, s.responseEOF)
		if s.responseEncoded {
			s.flow.collector.MarkStageEncoding(s.responseKey)
		}
	}
}

func (f *requestTraceFlow) downstreamChunk(chunk []byte, _ int) {
	if f == nil || f.collector == nil {
		return
	}
	f.mu.Lock()
	stream := f.clientStream
	f.mu.Unlock()
	key := service.RequestTraceBodyKey{Stage: "client_response", View: "downstream"}
	if stream {
		f.collector.StartStage(key, "text/event-stream", true)
	}
	f.collector.AppendStage(key, chunk)
}

func (f *requestTraceFlow) setClientStream(stream bool) {
	if f == nil || !stream {
		return
	}
	f.mu.Lock()
	f.clientStream = true
	f.mu.Unlock()
	f.collector.StartStage(service.RequestTraceBodyKey{Stage: "client_response", View: "downstream"}, "text/event-stream", true)
}

func (f *requestTraceFlow) setClientWrittenComplete(complete bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.clientWrittenComplete = complete
	f.mu.Unlock()
}

func (f *requestTraceFlow) markHijacked() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.hijacked = true
	f.mu.Unlock()
}

func (f *requestTraceFlow) finish(id string, authenticated bool) []service.RequestTraceStage {
	if f == nil || f.collector == nil {
		return nil
	}
	f.mu.Lock()
	facts := append([]requestTraceAttemptFacts(nil), f.attempts...)
	decisions := append([]requestTraceDecisionEvent(nil), f.decisions...)
	decisionDropped := f.decisionDropped
	hijacked := f.hijacked
	inboundFacts := f.clientFacts
	rejectReason := f.rejectReason
	clientComplete := f.clientWrittenComplete && !hijacked
	f.mu.Unlock()
	f.collector.FinishStage(service.RequestTraceBodyKey{Stage: "client_response", View: "downstream"}, clientComplete)
	for _, fact := range facts {
		f.collector.FinishStage(service.RequestTraceBodyKey{Stage: "upstream_response", AttemptIndex: fact.index, View: "received"}, false)
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].index < facts[j].index })
	body := f.collector.Snapshots()
	stages := make([]service.RequestTraceStage, 0, 2+len(facts)+len(body))
	clientFacts := service.RequestTraceStage{
		TraceID: id, Stage: "client_metadata", State: service.RequestTraceNotObserved,
		Reason: "metadata_observed", Metadata: service.CloneRequestTraceStageFacts("client_metadata", &inboundFacts),
	}
	if rejectReason != "" {
		clientFacts.Reason = rejectReason
	}
	stages = append(stages, clientFacts)
	for _, decision := range decisions {
		facts := decision.facts
		stages = append(stages, service.NewRequestTraceGatewayDecisionStage(id, 0, decision.attemptIndex, decision.reason, &facts))
	}
	if !authenticated {
		stages = append(stages, service.RequestTraceStage{TraceID: id, Stage: "client_entry", State: service.RequestTraceNotObserved, Reason: "auth_rejected_body_not_observed"})
	} else if len(body) == 0 {
		stages = append(stages, service.RequestTraceStage{TraceID: id, Stage: "client_entry", State: service.RequestTraceNotObserved, Reason: "body_not_observed"})
	}
	if authenticated && len(facts) == 0 {
		stages = append(stages, service.RequestTraceStage{
			TraceID: id, Stage: "wire_attempt", State: service.RequestTraceNotObserved,
			Reason: "attempt_not_observed",
		})
	}
	for _, fact := range facts {
		state := service.RequestTraceNotObserved
		reason := "wire_observed"
		if fact.hadError {
			reason = "transport_error"
		}
		if fact.facts.ValueProtocol == "bedrock" || fact.facts.ValueProtocol == "vertex" {
			state = service.RequestTraceUnsupported
			reason = "wire_protocol_outside_phase1"
		}
		stages = append(stages, service.RequestTraceStage{
			TraceID: id, Stage: "wire_attempt", AttemptIndex: fact.index,
			State: state, Reason: reason,
			Metadata: service.CloneRequestTraceStageFacts("wire_attempt", &fact.facts),
		})
	}
	for _, snap := range body {
		stages = append(stages, service.RequestTraceStage{
			TraceID: id, Stage: snap.Key.Stage, AttemptIndex: snap.Key.AttemptIndex,
			View: snap.Key.View, State: service.RequestTraceCaptureState(snap.State), Reason: snap.Reason,
			ObservedBytes: snap.ObservedBytes, RetainedBytes: snap.RetainedBytes,
			DroppedEvents: snap.DroppedEvents, RedactionUnverified: snap.RedactionUnverified,
			Payload: snap.Payload,
		})
	}
	if decisionDropped > 0 {
		stages = append(stages, service.RequestTraceStage{
			TraceID: id, Stage: "capture_gap", State: service.RequestTraceTruncated,
			Reason: "decision_budget_exceeded", DroppedEvents: decisionDropped,
		})
	}
	if hijacked {
		stages = append(stages, service.RequestTraceStage{TraceID: id, Stage: "client_response", State: service.RequestTraceNotObserved, Reason: "hijacked_unobservable"})
	}
	if len(stages) > service.RequestTraceStageCountLimit {
		originalCount := len(stages)
		// Reserve the last two records for a visible gap and the actual client
		// response. Otherwise a long retry chain silently discards the result.
		var final, hijackGap, decisionGap *service.RequestTraceStage
		for i := range stages {
			if stages[i].Stage == "client_response" && stages[i].View == "downstream" {
				final = &stages[i]
			}
			if stages[i].Reason == "hijacked_unobservable" {
				hijackGap = &stages[i]
			}
			if stages[i].Reason == "decision_budget_exceeded" {
				decisionGap = &stages[i]
			}
		}
		keep := service.RequestTraceStageCountLimit - 1
		if final != nil {
			keep--
		}
		if hijackGap != nil {
			keep--
		}
		if decisionGap != nil {
			keep--
		}
		stages = stages[:keep]
		stages = append(stages, service.RequestTraceStage{
			TraceID: id, Stage: "capture_gap", State: service.RequestTraceTruncated,
			Reason: "stage_budget_exceeded", DroppedEvents: originalCount - keep,
		})
		if final != nil {
			stages = append(stages, *final)
		}
		if hijackGap != nil {
			stages = append(stages, *hijackGap)
		}
		if decisionGap != nil {
			stages = append(stages, *decisionGap)
		}
	}
	for i := range stages {
		stages[i].Ordinal = i + 1
	}
	return stages
}
