//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	identityLegacyDeviceID  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	identityLegacyAccount   = "11111111-2222-3333-4444-555555555555"
	identityLegacySession   = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	identityJSONDeviceID    = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	identityJSONAccountUUID = "99999999-8888-7777-6666-555555555555"
	identityJSONSessionID   = "12121212-3434-5656-7878-909090909090"
)

// identityLegacyUserID is the pre-2.1.78 concatenated form: exactly 159 bytes
// when the account uuid is present.
func identityLegacyUserID() string {
	return "user_" + identityLegacyDeviceID + "_account_" + identityLegacyAccount + "_session_" + identityLegacySession
}

// identityJSONUserID is the >=2.1.78 JSON string form Claude Code sends today.
func identityJSONUserID() string {
	return `{"device_id":"` + identityJSONDeviceID + `","account_uuid":"` + identityJSONAccountUUID +
		`","session_id":"` + identityJSONSessionID + `"}`
}

func identityAnthropicBody(userID string) []byte {
	metadata := ""
	if userID != "" {
		encoded, err := json.Marshal(userID)
		if err != nil {
			panic(err)
		}
		metadata = `,"metadata":{"user_id":` + string(encoded) + `}`
	}
	return []byte(`{"model":"claude-sonnet-4-5","max_tokens":32,` +
		`"messages":[{"role":"user","content":"hello"}]` + metadata + `}`)
}

type identityVerdictRecorder struct {
	events []RequestTraceIdentityVerdictEvent
}

func (r *identityVerdictRecorder) observer() RequestTraceIdentityVerdictObserver {
	return func(event RequestTraceIdentityVerdictEvent) {
		r.events = append(r.events, event)
	}
}

func identityVerdictContext(recorder *identityVerdictRecorder) context.Context {
	return WithRequestTraceIdentityVerdictObserver(context.Background(), recorder.observer())
}

func identityWireBody(userID string) []byte {
	return identityAnthropicBody(userID)
}

// responsesWireBody models the frozen Anthropic -> Responses conversion result:
// the Responses request type has no metadata field, so the identity is gone.
func responsesWireBody(userID string) []byte {
	body := map[string]any{
		"model":        "gpt-5.6-sol",
		"stream":       true,
		"instructions": "you are a coding agent",
		"input":        []any{map[string]any{"role": "user", "content": "hello"}},
	}
	if userID != "" {
		body["metadata"] = map[string]any{"user_id": userID}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return encoded
}

// --- verdict derivation -----------------------------------------------------

func TestRequestTraceIdentityBodyVerdictSentForLegacyAndJSONIdentity(t *testing.T) {
	for name, userID := range map[string]string{
		"legacy 159 concatenated": identityLegacyUserID(),
		"json string user_id":     identityJSONUserID(),
	} {
		t.Run(name, func(t *testing.T) {
			verdict, inboundSeen, wireSeen, changed, ok := requestTraceIdentityBodyVerdict(
				identityAnthropicBody(userID), identityWireBody(userID))
			require.True(t, ok)
			require.Equal(t, RequestTraceIdentityVerdictSent, verdict)
			require.True(t, inboundSeen)
			require.True(t, wireSeen)
			require.False(t, changed)
		})
	}
}

func TestRequestTraceIdentityBodyVerdictRewrittenWhenWireValueDiffers(t *testing.T) {
	for name, userID := range map[string]string{
		"legacy 159 concatenated": identityLegacyUserID(),
		"json string user_id":     identityJSONUserID(),
	} {
		t.Run(name, func(t *testing.T) {
			rewritten := userID + " "
			verdict, inboundSeen, wireSeen, changed, ok := requestTraceIdentityBodyVerdict(
				identityAnthropicBody(userID), identityWireBody(rewritten))
			require.True(t, ok)
			require.Equal(t, RequestTraceIdentityVerdictRewritten, verdict)
			require.True(t, inboundSeen)
			require.True(t, wireSeen)
			require.True(t, changed)
		})
	}
}

// A value the gateway wrote where the client sent none is a rewrite, not a send.
func TestRequestTraceIdentityBodyVerdictRewrittenWhenGatewayInjectsIdentity(t *testing.T) {
	verdict, inboundSeen, wireSeen, changed, ok := requestTraceIdentityBodyVerdict(
		identityAnthropicBody(""), identityWireBody(identityJSONUserID()))
	require.True(t, ok)
	require.Equal(t, RequestTraceIdentityVerdictRewritten, verdict)
	require.False(t, inboundSeen)
	require.True(t, wireSeen)
	require.True(t, changed)
}

// The Anthropic -> Responses conversion drops metadata: the frozen Responses body
// has no user_id, so the attempt must be reported as "not sent" rather than
// inferred to have carried the inbound value.
func TestRequestTraceIdentityBodyVerdictNotSentWhenProtocolConversionDropsIdentity(t *testing.T) {
	for name, userID := range map[string]string{
		"legacy 159 concatenated": identityLegacyUserID(),
		"json string user_id":     identityJSONUserID(),
	} {
		t.Run(name, func(t *testing.T) {
			verdict, inboundSeen, wireSeen, changed, ok := requestTraceIdentityBodyVerdict(
				identityAnthropicBody(userID), responsesWireBody(""))
			require.True(t, ok)
			require.Equal(t, RequestTraceIdentityVerdictNotSent, verdict)
			require.True(t, inboundSeen)
			require.False(t, wireSeen)
			require.False(t, changed)
		})
	}
}

func TestRequestTraceIdentityBodyVerdictOmitsEventWithoutAnyIdentity(t *testing.T) {
	verdict, inboundSeen, wireSeen, changed, ok := requestTraceIdentityBodyVerdict(
		identityAnthropicBody(""), responsesWireBody(""))
	require.False(t, ok)
	require.Equal(t, RequestTraceIdentityVerdict(""), verdict)
	require.False(t, inboundSeen)
	require.False(t, wireSeen)
	require.False(t, changed)
}

func TestRequestTraceIdentityBodyVerdictIgnoresEmptyAndNonStringIdentity(t *testing.T) {
	for name, body := range map[string][]byte{
		"empty user_id":      []byte(`{"metadata":{"user_id":""}}`),
		"null user_id":       []byte(`{"metadata":{"user_id":null}}`),
		"object user_id":     []byte(`{"metadata":{"user_id":{"device_id":"x"}}}`),
		"malformed json":     []byte(`{"metadata":`),
		"no metadata at all": []byte(`{"model":"claude-sonnet-4-5"}`),
	} {
		t.Run(name, func(t *testing.T) {
			verdict, _, _, _, ok := requestTraceIdentityBodyVerdict(body, body)
			require.False(t, ok)
			require.Equal(t, RequestTraceIdentityVerdict(""), verdict)
		})
	}
}

// --- reporting contract -----------------------------------------------------

func TestRequestTraceIdentityVerdictEventNeverCarriesRawIdentity(t *testing.T) {
	recorder := &identityVerdictRecorder{}
	ctx := identityVerdictContext(recorder)
	require.True(t, ReportRequestTraceIdentityBodyVerdict(ctx,
		identityAnthropicBody(identityJSONUserID()), identityWireBody(identityJSONUserID()+" "),
		RequestTraceDecisionSourceIdentity, RequestAuditProtocolAnthropic, RequestAuditProtocolAnthropic,
		"anthropic_wire_freeze"))

	require.Len(t, recorder.events, 1)
	encoded, err := json.Marshal(recorder.events[0])
	require.NoError(t, err)
	for _, secret := range []string{identityJSONDeviceID, identityJSONSessionID, identityJSONAccountUUID, identityLegacyDeviceID} {
		require.NotContains(t, string(encoded), secret)
	}
}

func TestReportRequestTraceIdentityVerdictIsOptIn(t *testing.T) {
	// No observer on the context: nothing is inspected, nothing is reported.
	require.False(t, ReportRequestTraceIdentityBodyVerdict(context.Background(),
		identityAnthropicBody(identityJSONUserID()), identityWireBody(""),
		RequestTraceDecisionSourceIdentity, RequestAuditProtocolAnthropic, RequestAuditProtocolAnthropic,
		"anthropic_wire_freeze"))
	observer, ok := RequestTraceIdentityVerdictObserverFromContext(context.Background())
	require.False(t, ok)
	require.Nil(t, observer)
	// A nil observer is not a way to install one.
	_, ok = RequestTraceIdentityVerdictObserverFromContext(
		WithRequestTraceIdentityVerdictObserver(context.Background(), nil))
	require.False(t, ok)
}

func TestReportRequestTraceIdentityVerdictRejectsInconsistentEvents(t *testing.T) {
	base := RequestTraceIdentityVerdictEvent{
		Verdict: RequestTraceIdentityVerdictSent, Source: RequestTraceDecisionSourceIdentity,
		ProtocolFrom: RequestAuditProtocolAnthropic, ProtocolTo: RequestAuditProtocolAnthropic,
		AttemptIndex: 1, Reason: "anthropic_wire_freeze", InboundSeen: true, WireSeen: true,
	}
	cases := map[string]func(*RequestTraceIdentityVerdictEvent){
		"unknown verdict":    func(e *RequestTraceIdentityVerdictEvent) { e.Verdict = "maybe" },
		"unknown source":     func(e *RequestTraceIdentityVerdictEvent) { e.Source = "guessed" },
		"free-form reason":   func(e *RequestTraceIdentityVerdictEvent) { e.Reason = "rewrote the user id" },
		"empty reason":       func(e *RequestTraceIdentityVerdictEvent) { e.Reason = "" },
		"unbounded attempt":  func(e *RequestTraceIdentityVerdictEvent) { e.AttemptIndex = requestTraceDecisionMaxSequence + 1 },
		"negative attempt":   func(e *RequestTraceIdentityVerdictEvent) { e.AttemptIndex = -1 },
		"free-form protocol": func(e *RequestTraceIdentityVerdictEvent) { e.ProtocolTo = "openai responses" },
		"sent but changed":   func(e *RequestTraceIdentityVerdictEvent) { e.Changed = true },
		"sent without wire":  func(e *RequestTraceIdentityVerdictEvent) { e.WireSeen = false },
		"rewritten unchanged": func(e *RequestTraceIdentityVerdictEvent) {
			e.Verdict = RequestTraceIdentityVerdictRewritten
			e.Changed = false
		},
		"not_sent with wire": func(e *RequestTraceIdentityVerdictEvent) {
			e.Verdict = RequestTraceIdentityVerdictNotSent
			e.WireSeen = true
			e.Changed = false
		},
		"not_sent without inbound": func(e *RequestTraceIdentityVerdictEvent) {
			e.Verdict = RequestTraceIdentityVerdictNotSent
			e.InboundSeen = false
			e.WireSeen = false
			e.Changed = false
		},
	}
	require.True(t, requestTraceIdentityVerdictComplete(base), "the baseline event must be valid")
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			event := base
			mutate(&event)
			called := false
			ctx := WithRequestTraceIdentityVerdictObserver(context.Background(),
				func(RequestTraceIdentityVerdictEvent) { called = true })
			require.False(t, ReportRequestTraceIdentityVerdict(ctx, event))
			require.False(t, called)
			_, ok := RequestTraceIdentityVerdictDecisionFacts(event)
			require.False(t, ok)
		})
	}
}

// A broken observer must never turn into a gateway failure, and must never be
// reported as a delivered verdict.
func TestReportRequestTraceIdentityVerdictSurvivesPanickingObserver(t *testing.T) {
	ctx := WithRequestTraceIdentityVerdictObserver(context.Background(),
		func(RequestTraceIdentityVerdictEvent) { panic("observer bug") })
	require.NotPanics(t, func() {
		require.False(t, ReportRequestTraceIdentityVerdict(ctx, RequestTraceIdentityVerdictEvent{
			Verdict: RequestTraceIdentityVerdictSent, Source: RequestTraceDecisionSourceIdentity,
			Reason: "anthropic_wire_freeze", WireSeen: true,
		}))
	})
}

func TestRequestTraceIdentityVerdictDecisionFactsProjection(t *testing.T) {
	for verdict, wantOutcome := range map[RequestTraceIdentityVerdict]RequestTraceDecisionOutcome{
		RequestTraceIdentityVerdictSent:      RequestTraceDecisionUnchanged,
		RequestTraceIdentityVerdictRewritten: RequestTraceDecisionRewritten,
		RequestTraceIdentityVerdictNotSent:   RequestTraceDecisionNotSent,
	} {
		event := RequestTraceIdentityVerdictEvent{
			Verdict: verdict, Source: RequestTraceDecisionSourceProtocolConvert,
			ProtocolFrom: RequestAuditProtocolAnthropic, ProtocolTo: RequestAuditProtocolOpenAIResp,
			Reason: "anthropic_to_responses_wire_freeze",
		}
		switch verdict {
		case RequestTraceIdentityVerdictSent:
			event.InboundSeen, event.WireSeen = true, true
		case RequestTraceIdentityVerdictRewritten:
			event.InboundSeen, event.WireSeen, event.Changed = true, true, true
		default:
			event.InboundSeen = true
		}
		facts, ok := RequestTraceIdentityVerdictDecisionFacts(event)
		require.Truef(t, ok, "verdict %q", verdict)
		require.Equal(t, RequestTraceDecisionIdentity, facts.Decision)
		require.Equal(t, wantOutcome, facts.Outcome)
		require.Equal(t, RequestTraceDecisionSourceProtocolConvert, facts.Source)
		require.Equal(t, RequestAuditProtocolAnthropic, facts.ProtocolFrom)
		require.Equal(t, RequestAuditProtocolOpenAIResp, facts.ProtocolTo)
	}
}

func TestRequestTraceIdentityAttemptIndexFollowsHTTPAttemptCounter(t *testing.T) {
	require.Equal(t, 0, RequestTraceIdentityAttemptIndex(context.Background()),
		"an absent counter is not observable, not attempt one")
	counter := httpattempt.NewCounter()
	ctx := httpattempt.WithCounter(context.Background(), counter)
	require.Equal(t, 1, RequestTraceIdentityAttemptIndex(ctx))
	httpattempt.Increment(ctx)
	require.Equal(t, 2, RequestTraceIdentityAttemptIndex(ctx))
}

// --- real seams -------------------------------------------------------------

func newIdentityVerdictGinContext(t *testing.T, body []byte) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

func TestBuildUpstreamRequestReportsIdentityVerdictAtAnthropicWireFreeze(t *testing.T) {
	for name, userID := range map[string]string{
		"legacy 159 concatenated": identityLegacyUserID(),
		"json string user_id":     identityJSONUserID(),
	} {
		t.Run(name, func(t *testing.T) {
			body := identityAnthropicBody(userID)
			c := newIdentityVerdictGinContext(t, body)
			svc := &GatewayService{cfg: &config.Config{}}
			account := &Account{
				ID: 601, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "upstream-key"},
			}
			recorder := &identityVerdictRecorder{}
			// One already-sent attempt so the ordinal must be 2, not 1.
			counter := httpattempt.NewCounter()
			ctx := identityVerdictContext(recorder)
			ctx = httpattempt.WithCounter(ctx, counter)
			httpattempt.Increment(ctx)

			req, wireBody, err := svc.buildUpstreamRequest(ctx, c, account, body, "upstream-key", "apikey",
				"claude-sonnet-4-5", false, false)
			require.NoError(t, err)
			require.NotNil(t, req)
			require.Equal(t, userID, gjson.GetBytes(wireBody, "metadata.user_id").String())

			require.Len(t, recorder.events, 1)
			event := recorder.events[0]
			require.Equal(t, RequestTraceIdentityVerdictSent, event.Verdict)
			require.Equal(t, RequestTraceDecisionSourceIdentity, event.Source)
			require.Equal(t, "anthropic_wire_freeze", event.Reason)
			require.Equal(t, RequestAuditProtocolAnthropic, event.ProtocolFrom)
			require.Equal(t, RequestAuditProtocolAnthropic, event.ProtocolTo)
			require.Equal(t, 2, event.AttemptIndex)
			require.True(t, event.InboundSeen)
			require.True(t, event.WireSeen)
			require.False(t, event.Changed)
		})
	}
}

func TestBuildUpstreamRequestOmitsIdentityVerdictWithoutIdentity(t *testing.T) {
	body := identityAnthropicBody("")
	c := newIdentityVerdictGinContext(t, body)
	svc := &GatewayService{cfg: &config.Config{}}
	account := &Account{
		ID: 602, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "upstream-key"},
	}
	recorder := &identityVerdictRecorder{}

	_, _, err := svc.buildUpstreamRequest(identityVerdictContext(recorder), c, account, body,
		"upstream-key", "apikey", "claude-sonnet-4-5", false, false)
	require.NoError(t, err)
	require.Empty(t, recorder.events, "no identity was observed in either body, so nothing may be reported")
}

func TestBuildUpstreamAnthropicAPIKeyPassthroughReportsSentIdentityVerdict(t *testing.T) {
	body := identityAnthropicBody(identityJSONUserID())
	c := newIdentityVerdictGinContext(t, body)
	svc := &GatewayService{cfg: &config.Config{}}
	account := &Account{
		ID: 603, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "upstream-key"},
	}
	recorder := &identityVerdictRecorder{}

	req, wireBody, err := svc.buildUpstreamRequestAnthropicAPIKeyPassthrough(
		identityVerdictContext(recorder), c, account, body, "upstream-key")
	require.NoError(t, err)
	require.NotNil(t, req)
	require.Equal(t, identityJSONUserID(), gjson.GetBytes(wireBody, "metadata.user_id").String())

	require.Len(t, recorder.events, 1)
	event := recorder.events[0]
	require.Equal(t, RequestTraceIdentityVerdictSent, event.Verdict)
	require.Equal(t, RequestTraceDecisionSourceInbound, event.Source)
	require.Equal(t, "anthropic_passthrough_wire_freeze", event.Reason)
	require.Equal(t, RequestAuditProtocolAnthropic, event.ProtocolFrom)
	require.Equal(t, RequestAuditProtocolAnthropic, event.ProtocolTo)
	require.False(t, event.Changed)
}

// The OAuth identity seam is where the gateway actually writes metadata.user_id.
// A value the client never sent is a rewrite; a value the client did send and the
// gateway kept is a send, not a rewrite.
func TestApplyClaudeCodeOAuthMimicryToBodyReportsIdentityVerdictAtItsFreeze(t *testing.T) {
	newOAuthAccount := func(id int64) *Account {
		return &Account{
			ID: id, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
			Credentials: map[string]any{"access_token": "oauth-token"},
			Extra:       map[string]any{"account_uuid": identityJSONAccountUUID},
		}
	}
	newMimicryService := func() *GatewayService {
		svc := &GatewayService{cfg: &config.Config{}}
		svc.identityService = NewIdentityService(&stubIdentityCache{fingerprint: &Fingerprint{
			ClientID:  identityJSONDeviceID,
			UserAgent: "claude-cli/2.1.79 (external, cli)",
		}})
		return svc
	}
	anonymousBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":32,` +
		`"messages":[{"role":"user","content":"hello"}]}`)

	t.Run("gateway injected an identity the client never sent", func(t *testing.T) {
		c := newIdentityVerdictGinContext(t, anonymousBody)
		recorder := &identityVerdictRecorder{}

		out := newMimicryService().applyClaudeCodeOAuthMimicryToBody(
			identityVerdictContext(recorder), c, newOAuthAccount(604), anonymousBody, nil, "claude-sonnet-4-5")
		require.NotEmpty(t, gjson.GetBytes(out, "metadata.user_id").String())

		require.Len(t, recorder.events, 1)
		event := recorder.events[0]
		require.Equal(t, RequestTraceIdentityVerdictRewritten, event.Verdict)
		require.Equal(t, RequestTraceDecisionSourceIdentity, event.Source)
		require.Equal(t, "claude_oauth_identity_freeze", event.Reason)
		require.Equal(t, RequestAuditProtocolAnthropic, event.ProtocolFrom)
		require.False(t, event.InboundSeen)
		require.True(t, event.WireSeen)
		require.True(t, event.Changed)
	})

	t.Run("client identity is kept, so it is sent and not rewritten", func(t *testing.T) {
		body := identityAnthropicBody(identityJSONUserID())
		c := newIdentityVerdictGinContext(t, body)
		recorder := &identityVerdictRecorder{}

		out := newMimicryService().applyClaudeCodeOAuthMimicryToBody(
			identityVerdictContext(recorder), c, newOAuthAccount(607), body, nil, "claude-sonnet-4-5")
		require.Equal(t, identityJSONUserID(), gjson.GetBytes(out, "metadata.user_id").String())

		require.Len(t, recorder.events, 1)
		event := recorder.events[0]
		require.Equal(t, RequestTraceIdentityVerdictSent, event.Verdict)
		require.Equal(t, RequestTraceDecisionSourceIdentity, event.Source)
		require.True(t, event.InboundSeen)
		require.True(t, event.WireSeen)
		require.False(t, event.Changed)
	})
}

func TestApplyClaudeCodeOAuthMimicryToBodyOmitsVerdictWithoutIdentity(t *testing.T) {
	account := &Account{ID: 605, Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	svc := &GatewayService{cfg: &config.Config{}}
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)
	c := newIdentityVerdictGinContext(t, body)
	recorder := &identityVerdictRecorder{}

	svc.applyClaudeCodeOAuthMimicryToBody(identityVerdictContext(recorder), c, account, body, nil, "claude-sonnet-4-5")
	require.Empty(t, recorder.events)
}

// End-to-end at the Anthropic -> Responses conversion: the frozen upstream body
// has no metadata field, so the attempt is reported as not_sent.
func TestForwardAsAnthropicReportsNotSentIdentityVerdictForResponsesConversion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := identityAnthropicBody(identityJSONUserID())
	c := newIdentityVerdictGinContext(t, body)

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_identity","object":"response","model":"gpt-5.6-sol","status":"completed","output":[{"type":"message","id":"msg_identity","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_identity"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID: 606, Name: "openai-oauth", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-acc"},
	}
	recorder := &identityVerdictRecorder{}

	_, err := svc.ForwardAsAnthropic(identityVerdictContext(recorder), c, account, body, "", "gpt-5.6-sol")
	require.NoError(t, err)
	require.NotContains(t, string(upstream.lastBody), "metadata",
		"the Responses conversion must not carry the Anthropic metadata field")

	require.Len(t, recorder.events, 1)
	event := recorder.events[0]
	require.Equal(t, RequestTraceIdentityVerdictNotSent, event.Verdict)
	require.Equal(t, RequestTraceDecisionSourceProtocolConvert, event.Source)
	require.Equal(t, "anthropic_to_responses_wire_freeze", event.Reason)
	require.Equal(t, RequestAuditProtocolAnthropic, event.ProtocolFrom)
	require.Equal(t, RequestAuditProtocolOpenAIResp, event.ProtocolTo)
	require.True(t, event.InboundSeen)
	require.False(t, event.WireSeen)
	require.False(t, event.Changed)
}
