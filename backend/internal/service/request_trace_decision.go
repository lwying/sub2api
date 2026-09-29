package service

import (
	"encoding/json"
	"time"
)

// RequestTraceDecisionStage is the only stage name that carries a gateway decision.
// The decision is metadata about what the gateway decided, never observed wire
// bytes, so the stage is body-less: state stays not_observed and no payload is
// stored for it.
const RequestTraceDecisionStage = "gateway_decision"

// RequestTraceDecisionFactsLimit bounds the serialized decision projection before
// it leaves the gateway, mirroring requestTraceFactsLimit. The JSONB column keeps
// its own 4096 byte database bound (migrations 261 and 262).
const RequestTraceDecisionFactsLimit = 1024

// requestTraceDecisionMaxSequence mirrors the stage and attempt bounds: one logical
// request can never accumulate an unbounded number of decision events, so a
// sequence outside this range is a caller bug rather than a large trace.
const requestTraceDecisionMaxSequence = 1000

// RequestTraceDecisionKind is the gateway stage a decision describes.
type RequestTraceDecisionKind string

const (
	RequestTraceDecisionAuth          RequestTraceDecisionKind = "auth"
	RequestTraceDecisionRoute         RequestTraceDecisionKind = "route"
	RequestTraceDecisionModelMapping  RequestTraceDecisionKind = "model_mapping"
	RequestTraceDecisionAccountSwitch RequestTraceDecisionKind = "account_switch"
	RequestTraceDecisionIdentity      RequestTraceDecisionKind = "identity"
)

// RequestTraceDecisionOutcome is the observed result of one decision.
type RequestTraceDecisionOutcome string

const (
	RequestTraceDecisionAccepted    RequestTraceDecisionOutcome = "accepted"
	RequestTraceDecisionRejected    RequestTraceDecisionOutcome = "rejected"
	RequestTraceDecisionSelected    RequestTraceDecisionOutcome = "selected"
	RequestTraceDecisionUnchanged   RequestTraceDecisionOutcome = "unchanged"
	RequestTraceDecisionRewritten   RequestTraceDecisionOutcome = "rewritten"
	RequestTraceDecisionNotSent     RequestTraceDecisionOutcome = "not_sent"
	RequestTraceDecisionUnsupported RequestTraceDecisionOutcome = "unsupported"
)

// RequestTraceDecisionSource names where the decision's input came from.
type RequestTraceDecisionSource string

const (
	RequestTraceDecisionSourceInbound         RequestTraceDecisionSource = "inbound"
	RequestTraceDecisionSourceAPIKey          RequestTraceDecisionSource = "api_key"
	RequestTraceDecisionSourceGroup           RequestTraceDecisionSource = "group"
	RequestTraceDecisionSourceAccount         RequestTraceDecisionSource = "account"
	RequestTraceDecisionSourceIdentity        RequestTraceDecisionSource = "identity"
	RequestTraceDecisionSourceProtocolConvert RequestTraceDecisionSource = "protocol_convert"
)

// RequestTraceDecisionFacts is the only persisted projection of a gateway decision
// event (auth, route, model mapping, account switch or identity rewrite). It has no
// free-form field: a caller cannot attach a header, a body fragment, a raw URL or an
// arbitrary map to a decision, and the enums above are closed sets. An absent value
// is omitted (model/protocol/account/instant) or rejected (enum, sequence) rather
// than presented as an observed empty value.
type RequestTraceDecisionFacts struct {
	Decision     RequestTraceDecisionKind    `json:"decision"`
	Outcome      RequestTraceDecisionOutcome `json:"outcome"`
	Source       RequestTraceDecisionSource  `json:"source"`
	Sequence     int                         `json:"sequence"`
	ModelFrom    string                      `json:"model_from,omitempty"`
	ModelTo      string                      `json:"model_to,omitempty"`
	ProtocolFrom string                      `json:"protocol_from,omitempty"`
	ProtocolTo   string                      `json:"protocol_to,omitempty"`
	AccountID    int64                       `json:"account_id,omitempty"`
	DecidedAt    *time.Time                  `json:"decided_at,omitempty"`
}

func requestTraceDecisionKindValid(kind RequestTraceDecisionKind) bool {
	switch kind {
	case RequestTraceDecisionAuth, RequestTraceDecisionRoute, RequestTraceDecisionModelMapping,
		RequestTraceDecisionAccountSwitch, RequestTraceDecisionIdentity:
		return true
	default:
		return false
	}
}

func requestTraceDecisionOutcomeValid(outcome RequestTraceDecisionOutcome) bool {
	switch outcome {
	case RequestTraceDecisionAccepted, RequestTraceDecisionRejected, RequestTraceDecisionSelected,
		RequestTraceDecisionUnchanged, RequestTraceDecisionRewritten, RequestTraceDecisionNotSent,
		RequestTraceDecisionUnsupported:
		return true
	default:
		return false
	}
}

func requestTraceDecisionSourceValid(source RequestTraceDecisionSource) bool {
	switch source {
	case RequestTraceDecisionSourceInbound, RequestTraceDecisionSourceAPIKey, RequestTraceDecisionSourceGroup,
		RequestTraceDecisionSourceAccount, RequestTraceDecisionSourceIdentity,
		RequestTraceDecisionSourceProtocolConvert:
		return true
	default:
		return false
	}
}

// RequestTraceDecisionTimestamp normalises an observed instant to the UTC pointer
// the decision projection stores; a zero instant stays absent instead of being
// written as an observed 0001-01-01 value.
func RequestTraceDecisionTimestamp(observedAt time.Time) *time.Time {
	if observedAt.IsZero() {
		return nil
	}
	utc := observedAt.UTC()
	return &utc
}

// ValidRequestTraceDecisionFacts is the second trust boundary, applied by the
// queue, the repository writer and every reader. Only the closed enum sets, a
// positive bounded sequence, bounded safe tokens, a positive-or-absent account id
// and a UTC instant may reach the JSONB column or a disclosure DTO; anything else
// fails closed instead of being stored or read back as a partially trusted value.
func ValidRequestTraceDecisionFacts(facts *RequestTraceDecisionFacts) bool {
	if facts == nil {
		return false
	}
	if !requestTraceDecisionKindValid(facts.Decision) || !requestTraceDecisionOutcomeValid(facts.Outcome) ||
		!requestTraceDecisionSourceValid(facts.Source) || facts.Sequence <= 0 ||
		facts.Sequence > requestTraceDecisionMaxSequence {
		return false
	}
	if facts.AccountID < 0 {
		return false
	}
	// Model and protocol tokens share the transport facts' safe-token rule: a
	// bounded single token, never free text.
	for _, token := range []string{facts.ModelFrom, facts.ModelTo, facts.ProtocolFrom, facts.ProtocolTo} {
		if token != requestTraceFactTokenOrEmpty(token) {
			return false
		}
	}
	if facts.DecidedAt != nil && (facts.DecidedAt.IsZero() || facts.DecidedAt.Location() != time.UTC) {
		return false
	}
	encoded, err := json.Marshal(facts)
	return err == nil && len(encoded) <= RequestTraceDecisionFactsLimit
}

// CloneRequestTraceDecisionFacts validates the typed projection and returns an
// independent copy for the asynchronous queue, the repository and the readers, so
// a caller that keeps mutating its own value cannot change what is persisted.
func CloneRequestTraceDecisionFacts(facts *RequestTraceDecisionFacts) *RequestTraceDecisionFacts {
	if !ValidRequestTraceDecisionFacts(facts) {
		return nil
	}
	encoded, err := json.Marshal(facts)
	if err != nil {
		return nil
	}
	var copied RequestTraceDecisionFacts
	if json.Unmarshal(encoded, &copied) != nil {
		return nil
	}
	return &copied
}

// ValidRequestTraceDecisionStage is the rule the capture queue, the repository
// writer and the readers share for the two projections a stage may carry: the
// gateway_decision stage must be body-less (not_observed, no payload, no transport
// facts), carry exactly one validated decision and name a bounded reason code, and
// no other stage may carry a decision at all.
func ValidRequestTraceDecisionStage(stage RequestTraceStage) bool {
	if stage.Stage != RequestTraceDecisionStage {
		return stage.Decision == nil
	}
	return stage.Decision != nil && stage.Metadata == nil && stage.State == RequestTraceNotObserved &&
		len(stage.Payload) == 0 && ValidRequestTraceStageReason(stage.Reason) &&
		ValidRequestTraceDecisionFacts(stage.Decision)
}

// NewRequestTraceGatewayDecisionStage builds the body-less decision stage. The
// reason is a bounded code owned by the caller and the decision projection is the
// only value the stage carries; the ordinal is assigned by the trace's own stage
// ordering, exactly like every other stage.
func NewRequestTraceGatewayDecisionStage(traceID string, ordinal, attemptIndex int, reason string,
	decision *RequestTraceDecisionFacts) RequestTraceStage {
	return RequestTraceStage{
		TraceID: traceID, Ordinal: ordinal, Stage: RequestTraceDecisionStage, AttemptIndex: attemptIndex,
		State: RequestTraceNotObserved, Reason: reason, Decision: decision,
	}
}
