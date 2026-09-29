package service

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/tidwall/gjson"
)

// RequestTraceIdentityFieldPath is the only body field an identity verdict
// observes: the Claude Code client identity string in the Anthropic metadata
// object. The verdict event reports whether this field was seen and whether its
// value changed; the value itself is never copied into the event, the queue, the
// database or a log line.
const RequestTraceIdentityFieldPath = "metadata.user_id"

// RequestTraceIdentityVerdict is the closed three-value answer to "what reached
// the frozen wire body for the client identity field". It is a verdict about the
// bytes a transport is about to send, not about the inbound request, so a
// conversion that drops the field can never be presented as "sent".
type RequestTraceIdentityVerdict string

const (
	// RequestTraceIdentityVerdictSent: the frozen wire body still carries the
	// identity the seam received, unchanged.
	RequestTraceIdentityVerdictSent RequestTraceIdentityVerdict = "sent"
	// RequestTraceIdentityVerdictRewritten: the gateway decided a different
	// identity for the frozen wire body (rewritten client value, injected value
	// where the client sent none, or a rebuilt value).
	RequestTraceIdentityVerdictRewritten RequestTraceIdentityVerdict = "rewritten"
	// RequestTraceIdentityVerdictNotSent: the seam observed an identity and the
	// frozen wire body has none, so the upstream never received it.
	RequestTraceIdentityVerdictNotSent RequestTraceIdentityVerdict = "not_sent"
)

// RequestTraceIdentityVerdictEvent is the whole payload an identity seam may
// report. It is deliberately closed: no raw user_id, no body bytes, no header,
// no free-form string. InboundSeen/WireSeen/Changed are bounded booleans about
// presence and equality only, and Source is one of the closed decision sources.
//
// AttemptIndex is the 1-based ordinal of the HTTP attempt the frozen body belongs
// to, or 0 when the seam cannot observe an attempt counter.
type RequestTraceIdentityVerdictEvent struct {
	Verdict      RequestTraceIdentityVerdict
	Source       RequestTraceDecisionSource
	ProtocolFrom string
	ProtocolTo   string
	AttemptIndex int
	Reason       string
	InboundSeen  bool
	WireSeen     bool
	Changed      bool
}

// RequestTraceIdentityVerdictObserver consumes one already-validated verdict. It
// runs inline on the gateway path and must consume the event immediately: the
// event has value semantics and the caller retains nothing.
type RequestTraceIdentityVerdictObserver func(RequestTraceIdentityVerdictEvent)

type requestTraceIdentityVerdictObserverContextKey struct{}

// WithRequestTraceIdentityVerdictObserver opts one request context into identity
// verdict reporting. Without an observer every seam is a no-op: no body is
// inspected and no verdict is constructed, so a Trace-disabled deployment pays
// nothing and existing forced-audit behaviour is untouched.
func WithRequestTraceIdentityVerdictObserver(ctx context.Context, observer RequestTraceIdentityVerdictObserver) context.Context {
	if ctx == nil || observer == nil {
		return ctx
	}
	return context.WithValue(ctx, requestTraceIdentityVerdictObserverContextKey{}, observer)
}

// RequestTraceIdentityVerdictObserverFromContext reports the opt-in observer, if
// any. A missing observer is the normal case, not an error.
func RequestTraceIdentityVerdictObserverFromContext(ctx context.Context) (RequestTraceIdentityVerdictObserver, bool) {
	if ctx == nil {
		return nil, false
	}
	observer, ok := ctx.Value(requestTraceIdentityVerdictObserverContextKey{}).(RequestTraceIdentityVerdictObserver)
	return observer, ok && observer != nil
}

// requestTraceIdentityVerdictComplete is the fail-closed contract every reported
// event must satisfy: closed enums, a bounded reason code, bounded safe protocol
// tokens, a bounded attempt ordinal and verdict/boolean agreement. A caller bug
// drops the event instead of persisting a half-trusted identity claim.
func requestTraceIdentityVerdictComplete(event RequestTraceIdentityVerdictEvent) bool {
	switch event.Verdict {
	case RequestTraceIdentityVerdictSent, RequestTraceIdentityVerdictRewritten, RequestTraceIdentityVerdictNotSent:
	default:
		return false
	}
	if !requestTraceDecisionSourceValid(event.Source) || !ValidRequestTraceStageReason(event.Reason) {
		return false
	}
	if event.AttemptIndex < 0 || event.AttemptIndex > requestTraceDecisionMaxSequence {
		return false
	}
	for _, token := range []string{event.ProtocolFrom, event.ProtocolTo} {
		if token != "" && token != requestTraceFactTokenOrEmpty(token) {
			return false
		}
	}
	switch event.Verdict {
	case RequestTraceIdentityVerdictSent:
		// "sent" claims the upstream receives the same identity: the field must
		// exist on the wire and must not have changed.
		return event.WireSeen && !event.Changed
	case RequestTraceIdentityVerdictRewritten:
		// "rewritten" claims the gateway decided a value that was not there before.
		return event.WireSeen && event.Changed
	default:
		// "not_sent" is only meaningful against an observed inbound identity, and
		// it may not claim the field reached the wire.
		return event.InboundSeen && !event.WireSeen && !event.Changed
	}
}

// ReportRequestTraceIdentityVerdict delivers one validated verdict to the opt-in
// observer. It never returns an error and never panics into the gateway: an
// invalid event, a missing observer or a broken observer only means the verdict
// was not reported. Forced audit gates are unaffected either way.
func ReportRequestTraceIdentityVerdict(ctx context.Context, event RequestTraceIdentityVerdictEvent) (reported bool) {
	observer, ok := RequestTraceIdentityVerdictObserverFromContext(ctx)
	if !ok || !requestTraceIdentityVerdictComplete(event) {
		return false
	}
	defer func() {
		if recover() != nil {
			reported = false
		}
	}()
	observer(event)
	return true
}

// RequestTraceIdentityAttemptIndex returns the 1-based ordinal of the HTTP attempt
// the frozen body belongs to. The attempt counter is incremented by the transport
// when the request is actually sent, so at a pre-send freeze the next ordinal is
// the current count plus one. 0 means "not observable", never "attempt one".
func RequestTraceIdentityAttemptIndex(ctx context.Context) int {
	counter, ok := httpattempt.FromContext(ctx)
	if !ok || counter == nil {
		return 0
	}
	return int(counter.Load()) + 1
}

// requestTraceIdentityFieldValue returns the identity value a body carries. A
// missing, empty or non-string field is not an identity: an unreadable value is
// never presented as an observed one. The value is used for one local comparison
// and is dropped immediately.
func requestTraceIdentityFieldValue(body []byte) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	result := gjson.GetBytes(body, RequestTraceIdentityFieldPath)
	if !result.Exists() || result.Type != gjson.String || result.String() == "" {
		return "", false
	}
	return result.String(), true
}

// requestTraceIdentityBodyVerdict derives the verdict from two frozen bodies: the
// body the seam received and the body the transport is about to send. It returns
// ok=false when neither body carries an identity, because then the request
// contains no identity fact to report and a fabricated event would be worse than
// a gap.
func requestTraceIdentityBodyVerdict(inboundBody, wireBody []byte) (verdict RequestTraceIdentityVerdict, inboundSeen, wireSeen, changed bool, ok bool) {
	inbound, hasInbound := requestTraceIdentityFieldValue(inboundBody)
	wire, hasWire := requestTraceIdentityFieldValue(wireBody)
	switch {
	case hasInbound && hasWire:
		inboundSeen, wireSeen, changed = true, true, inbound != wire
	case hasInbound:
		inboundSeen = true
	case hasWire:
		// The gateway wrote an identity the seam did not receive.
		wireSeen, changed = true, true
	default:
		return "", false, false, false, false
	}
	switch {
	case !wireSeen:
		return RequestTraceIdentityVerdictNotSent, inboundSeen, wireSeen, changed, true
	case changed:
		return RequestTraceIdentityVerdictRewritten, inboundSeen, wireSeen, changed, true
	default:
		return RequestTraceIdentityVerdictSent, inboundSeen, wireSeen, changed, true
	}
}

// ReportRequestTraceIdentityBodyVerdict is the seam hook. Call it exactly where a
// wire body is frozen, passing the body this seam received and the body the
// transport will send, plus the closed source and a bounded reason code. It
// inspects only metadata.user_id presence and equality, reports at most one
// bounded event, and returns whether the event was delivered.
func ReportRequestTraceIdentityBodyVerdict(
	ctx context.Context,
	inboundBody, wireBody []byte,
	source RequestTraceDecisionSource,
	protocolFrom, protocolTo, reason string,
) bool {
	if _, ok := RequestTraceIdentityVerdictObserverFromContext(ctx); !ok {
		// No opt-in observer: do not even inspect the bodies.
		return false
	}
	verdict, inboundSeen, wireSeen, changed, ok := requestTraceIdentityBodyVerdict(inboundBody, wireBody)
	if !ok {
		return false
	}
	return ReportRequestTraceIdentityVerdict(ctx, RequestTraceIdentityVerdictEvent{
		Verdict:      verdict,
		Source:       source,
		ProtocolFrom: protocolFrom,
		ProtocolTo:   protocolTo,
		AttemptIndex: RequestTraceIdentityAttemptIndex(ctx),
		Reason:       reason,
		InboundSeen:  inboundSeen,
		WireSeen:     wireSeen,
		Changed:      changed,
	})
}

// RequestTraceIdentityVerdictDecisionFacts projects a reported identity verdict
// into the persisted gateway-decision projection, so a Trace reader sees the same
// three-state answer the wire seam decided. The caller assigns Sequence and
// DecidedAt exactly like every other decision; an unaccepted event yields false.
func RequestTraceIdentityVerdictDecisionFacts(event RequestTraceIdentityVerdictEvent) (RequestTraceDecisionFacts, bool) {
	if !requestTraceIdentityVerdictComplete(event) {
		return RequestTraceDecisionFacts{}, false
	}
	var outcome RequestTraceDecisionOutcome
	switch event.Verdict {
	case RequestTraceIdentityVerdictSent:
		outcome = RequestTraceDecisionUnchanged
	case RequestTraceIdentityVerdictRewritten:
		outcome = RequestTraceDecisionRewritten
	default:
		outcome = RequestTraceDecisionNotSent
	}
	return RequestTraceDecisionFacts{
		Decision:     RequestTraceDecisionIdentity,
		Outcome:      outcome,
		Source:       event.Source,
		ProtocolFrom: event.ProtocolFrom,
		ProtocolTo:   event.ProtocolTo,
	}, true
}
