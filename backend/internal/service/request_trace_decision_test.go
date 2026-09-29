//go:build unit

package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func validRequestTraceDecisionFacts() RequestTraceDecisionFacts {
	decidedAt := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	return RequestTraceDecisionFacts{
		Decision:     RequestTraceDecisionRoute,
		Outcome:      RequestTraceDecisionSelected,
		Source:       RequestTraceDecisionSourceGroup,
		Sequence:     1,
		ModelFrom:    "claude-sonnet-4-5",
		ModelTo:      "claude-sonnet-4-5-20250929",
		ProtocolFrom: "anthropic.messages",
		ProtocolTo:   "anthropic.messages",
		AccountID:    73,
		DecidedAt:    &decidedAt,
	}
}

// The closed enum sets are the contract the database allowlist in migration 262
// expands: a value outside them can never be persisted or read back.
func TestRequestTraceDecisionFactsAcceptsOnlyClosedEnums(t *testing.T) {
	for _, decision := range []RequestTraceDecisionKind{
		RequestTraceDecisionAuth, RequestTraceDecisionRoute, RequestTraceDecisionModelMapping,
		RequestTraceDecisionAccountSwitch, RequestTraceDecisionIdentity,
	} {
		facts := validRequestTraceDecisionFacts()
		facts.Decision = decision
		require.Truef(t, ValidRequestTraceDecisionFacts(&facts), "decision %q must be accepted", decision)
	}
	for _, outcome := range []RequestTraceDecisionOutcome{
		RequestTraceDecisionAccepted, RequestTraceDecisionRejected, RequestTraceDecisionSelected,
		RequestTraceDecisionUnchanged, RequestTraceDecisionRewritten, RequestTraceDecisionNotSent,
		RequestTraceDecisionUnsupported,
	} {
		facts := validRequestTraceDecisionFacts()
		facts.Outcome = outcome
		require.Truef(t, ValidRequestTraceDecisionFacts(&facts), "outcome %q must be accepted", outcome)
	}
	for _, source := range []RequestTraceDecisionSource{
		RequestTraceDecisionSourceInbound, RequestTraceDecisionSourceAPIKey, RequestTraceDecisionSourceGroup,
		RequestTraceDecisionSourceAccount, RequestTraceDecisionSourceIdentity,
		RequestTraceDecisionSourceProtocolConvert,
	} {
		facts := validRequestTraceDecisionFacts()
		facts.Source = source
		require.Truef(t, ValidRequestTraceDecisionFacts(&facts), "source %q must be accepted", source)
	}
}

func TestRequestTraceDecisionFactsRejectsValuesOutsideTheClosedSets(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*RequestTraceDecisionFacts)
	}{
		{name: "empty decision", mutate: func(f *RequestTraceDecisionFacts) { f.Decision = "" }},
		{name: "unknown decision", mutate: func(f *RequestTraceDecisionFacts) { f.Decision = "authorization" }},
		{name: "decision with uppercase", mutate: func(f *RequestTraceDecisionFacts) { f.Decision = "Auth" }},
		{name: "unknown outcome", mutate: func(f *RequestTraceDecisionFacts) { f.Outcome = "maybe" }},
		{name: "unknown source", mutate: func(f *RequestTraceDecisionFacts) { f.Source = "cache" }},
		{name: "zero sequence", mutate: func(f *RequestTraceDecisionFacts) { f.Sequence = 0 }},
		{name: "negative sequence", mutate: func(f *RequestTraceDecisionFacts) { f.Sequence = -1 }},
		{name: "sequence beyond bound", mutate: func(f *RequestTraceDecisionFacts) { f.Sequence = requestTraceDecisionMaxSequence + 1 }},
		{name: "negative account id", mutate: func(f *RequestTraceDecisionFacts) { f.AccountID = -7 }},
		{name: "unsafe model token", mutate: func(f *RequestTraceDecisionFacts) { f.ModelFrom = "claude sonnet 4.5" }},
		{name: "model token with newline", mutate: func(f *RequestTraceDecisionFacts) { f.ModelTo = "claude\nsonnet" }},
		{name: "oversized model token", mutate: func(f *RequestTraceDecisionFacts) { f.ModelFrom = strings.Repeat("m", 129) }},
		{name: "unsafe protocol token", mutate: func(f *RequestTraceDecisionFacts) { f.ProtocolFrom = "anthropic messages" }},
		{name: "protocol token with trailing space", mutate: func(f *RequestTraceDecisionFacts) { f.ProtocolTo = "anthropic.messages " }},
		{name: "token with leading separator", mutate: func(f *RequestTraceDecisionFacts) { f.ProtocolFrom = "-anthropic" }},
		{name: "zero decided_at", mutate: func(f *RequestTraceDecisionFacts) { zero := time.Time{}; f.DecidedAt = &zero }},
		{name: "non-utc decided_at", mutate: func(f *RequestTraceDecisionFacts) {
			zone := time.FixedZone("UTC+8", 8*3600)
			local := time.Date(2026, 9, 29, 18, 0, 0, 0, zone)
			f.DecidedAt = &local
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			facts := validRequestTraceDecisionFacts()
			testCase.mutate(&facts)
			require.False(t, ValidRequestTraceDecisionFacts(&facts))
		})
	}
}

func TestRequestTraceDecisionFactsRejectsNilAndKeepsHeadroomUnderTheByteBound(t *testing.T) {
	require.False(t, ValidRequestTraceDecisionFacts(nil), "an absent decision is never a valid projection")

	facts := validRequestTraceDecisionFacts()
	facts.Decision = RequestTraceDecisionAccountSwitch
	facts.Outcome = RequestTraceDecisionUnsupported
	facts.Source = RequestTraceDecisionSourceProtocolConvert
	facts.Sequence = requestTraceDecisionMaxSequence
	facts.ModelFrom = strings.Repeat("m", 128)
	facts.ModelTo = strings.Repeat("o", 128)
	facts.ProtocolFrom = strings.Repeat("p", 128)
	facts.ProtocolTo = strings.Repeat("q", 128)
	facts.AccountID = 1 << 62
	require.True(t, ValidRequestTraceDecisionFacts(&facts), "a maximum-width legal decision still fits")

	encoded, err := json.Marshal(facts)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), RequestTraceDecisionFactsLimit,
		"every legal decision must stay inside the queue/JSONB byte bound without truncation")

	// The bound is a backstop, not a promise that a caller can grow a decision:
	// one byte past the token bound is refused before the size is even measured.
	facts.ProtocolTo = strings.Repeat("q", 129)
	require.False(t, ValidRequestTraceDecisionFacts(&facts))
}

func TestRequestTraceDecisionFactsCloneIsDeepAndFailClosed(t *testing.T) {
	require.Nil(t, CloneRequestTraceDecisionFacts(nil))
	invalid := validRequestTraceDecisionFacts()
	invalid.Source = "cache"
	require.Nil(t, CloneRequestTraceDecisionFacts(&invalid), "an invalid decision is never copied forward")

	original := validRequestTraceDecisionFacts()
	cloned := CloneRequestTraceDecisionFacts(&original)
	require.NotNil(t, cloned)
	require.NotSame(t, &original, cloned)
	*cloned.DecidedAt = cloned.DecidedAt.Add(time.Hour)
	cloned.ModelTo = "tampered"
	require.Equal(t, "claude-sonnet-4-5-20250929", original.ModelTo, "the clone must own its own tokens")
	require.Equal(t, time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC), *original.DecidedAt,
		"the clone must own its own instant, not alias the caller's")
	require.True(t, ValidRequestTraceDecisionFacts(cloned))
}

// RequestTraceDecisionTimestamp is the only normalisation the projection accepts:
// a zero instant stays absent and every other instant is stored as UTC.
func TestRequestTraceDecisionTimestampNormalisesToUTC(t *testing.T) {
	require.Nil(t, RequestTraceDecisionTimestamp(time.Time{}))
	zone := time.FixedZone("UTC+8", 8*3600)
	normalised := RequestTraceDecisionTimestamp(time.Date(2026, 9, 29, 18, 0, 0, 0, zone))
	require.NotNil(t, normalised)
	require.Equal(t, time.UTC, normalised.Location())
	require.Equal(t, time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC), *normalised)
}

// The decision stage is body-less: it carries exactly one validated decision and
// never transport facts, and no other stage may carry a decision.
func TestRequestTraceDecisionStageCarriesOnlyADecision(t *testing.T) {
	decision := validRequestTraceDecisionFacts()
	stage := NewRequestTraceGatewayDecisionStage("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 7, 2, "route_selected", &decision)
	require.Equal(t, RequestTraceDecisionStage, stage.Stage)
	require.Equal(t, 7, stage.Ordinal)
	require.Equal(t, 2, stage.AttemptIndex)
	require.Equal(t, RequestTraceNotObserved, stage.State)
	require.Equal(t, "route_selected", stage.Reason)
	require.Empty(t, stage.Payload)
	require.Nil(t, stage.Metadata)
	require.NotNil(t, stage.Decision)
	require.True(t, ValidRequestTraceDecisionStage(stage))

	invalid := []struct {
		name   string
		mutate func(*RequestTraceStage)
	}{
		{name: "missing decision", mutate: func(s *RequestTraceStage) { s.Decision = nil }},
		{name: "invalid decision", mutate: func(s *RequestTraceStage) {
			broken := validRequestTraceDecisionFacts()
			broken.Sequence = 0
			s.Decision = &broken
		}},
		{name: "observed payload", mutate: func(s *RequestTraceStage) { s.Payload = []byte("body") }},
		{name: "stored state", mutate: func(s *RequestTraceStage) { s.State = RequestTraceStored }},
		{name: "transport facts", mutate: func(s *RequestTraceStage) { s.Metadata = &RequestTraceStageFacts{Method: "POST"} }},
		{name: "empty reason code", mutate: func(s *RequestTraceStage) { s.Reason = "" }},
		{name: "free-form reason", mutate: func(s *RequestTraceStage) { s.Reason = "Route Selected" }},
		{name: "unbounded reason code", mutate: func(s *RequestTraceStage) { s.Reason = "route_" + strings.Repeat("x", 96) }},
	}
	for _, testCase := range invalid {
		t.Run(testCase.name, func(t *testing.T) {
			broken := stage
			broken.Decision = CloneRequestTraceDecisionFacts(stage.Decision)
			testCase.mutate(&broken)
			require.False(t, ValidRequestTraceDecisionStage(broken))
		})
	}

	// Every other stage name is allowed to carry no decision at all and nothing else.
	other := RequestTraceStage{TraceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ordinal: 1, Stage: "wire_attempt", State: RequestTraceNotObserved, Reason: "wire_observed"}
	require.True(t, ValidRequestTraceDecisionStage(other))
	claimed := other
	claimed.Decision = &decision
	require.False(t, ValidRequestTraceDecisionStage(claimed), "a transport stage cannot smuggle a decision")
}
