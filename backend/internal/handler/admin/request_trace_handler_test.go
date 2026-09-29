//go:build unit

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type requestTraceReaderStub struct {
	items  []service.RequestTrace
	detail *service.RequestTraceDetail
}

func (s *requestTraceReaderStub) ListRequestTraces(context.Context, service.RequestTraceListFilter) ([]service.RequestTrace, int64, error) {
	return s.items, int64(len(s.items)), nil
}
func (s *requestTraceReaderStub) GetRequestTrace(context.Context, string) (*service.RequestTraceDetail, error) {
	return s.detail, nil
}

func TestRequestTraceAdminDetailsExposeOnlyTypedRedactedFacts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	id := "0123456789abcdef0123456789abcdef"
	trace := service.RequestTrace{TraceID: id, RouteFamily: service.RequestTraceMessages, InboundEndpoint: "/v1/messages"}
	reader := &requestTraceReaderStub{detail: &service.RequestTraceDetail{RequestTrace: trace, Stages: []service.RequestTraceStage{
		{Ordinal: 1, Stage: "wire_attempt", State: service.RequestTraceNotObserved, Reason: "wire_observed", Metadata: &service.RequestTraceStageFacts{
			Method: "POST", URL: "https://synthetic.example/v1/messages?api_key=%5BREDACTED%5D", AccountID: 73,
			Protocol: "anthropic.messages", Status: 200, RequestHeaders: http.Header{"Authorization": {"[REDACTED]"}, "X-Visible": {"allowed"}},
		}},
		{Ordinal: 2, Stage: "client_entry", State: service.RequestTraceStored, Reason: "retained", Payload: []byte(`{"text":"synthetic"}`)},
	}}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("auth_method", service.AuditAuthMethodJWT)
	c.Params = gin.Params{{Key: "trace_id", Value: id}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/request-traces/"+id, nil)
	h := NewRequestTraceHandler(reader)
	h.Get(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	var result struct {
		Data struct {
			Stages []struct {
				Facts *service.RequestTraceStageFacts `json:"facts"`
			} `json:"stages"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &result))
	require.Len(t, result.Data.Stages, 2)
	require.NotNil(t, result.Data.Stages[0].Facts)
	require.Equal(t, "POST", result.Data.Stages[0].Facts.Method)
	require.Equal(t, "[REDACTED]", result.Data.Stages[0].Facts.RequestHeaders.Get("Authorization"))
	require.Contains(t, recorder.Body.String(), "allowed")
	require.NotContains(t, recorder.Body.String(), "metadata")
	require.NotContains(t, recorder.Body.String(), "Bearer")
}

func TestRequestTraceAdminListCannotRevealBodyButSessionDetailCan(t *testing.T) {
	gin.SetMode(gin.TestMode)
	id := "0123456789abcdef0123456789abcdef"
	cleanupAt := time.Now().Add(30 * 24 * time.Hour)
	trace := service.RequestTrace{TraceID: id, RouteFamily: service.RequestTraceMessages, InboundEndpoint: "/v1/messages", CaptureState: service.RequestTraceStored, ClientStatus: 200, CreatedAt: time.Now(), CleanupAfter: &cleanupAt}
	reader := &requestTraceReaderStub{items: []service.RequestTrace{trace}, detail: &service.RequestTraceDetail{RequestTrace: trace, Stages: []service.RequestTraceStage{{Ordinal: 1, Stage: "client_entry", State: service.RequestTraceStored, Reason: "retained", Payload: []byte("CANARY_PROMPT"), RetainedBytes: 13, ObservedBytes: 13}}}}
	h := NewRequestTraceHandler(reader)

	list := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(list)
	c.Set("auth_method", service.AuditAuthMethodJWT)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/request-traces", nil)
	h.List(c)
	require.Equal(t, http.StatusOK, list.Code)
	require.NotContains(t, list.Body.String(), "CANARY_PROMPT")

	key := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(key)
	c.Set("auth_method", service.AuditAuthMethodAdminAPIKey)
	c.Params = gin.Params{{Key: "trace_id", Value: id}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/request-traces/"+id, nil)
	h.Get(c)
	require.Equal(t, http.StatusForbidden, key.Code)
	require.NotContains(t, key.Body.String(), "CANARY_PROMPT")

	detail := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(detail)
	c.Set("auth_method", service.AuditAuthMethodJWT)
	c.Params = gin.Params{{Key: "trace_id", Value: id}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/request-traces/"+id, nil)
	h.Get(c)
	require.Equal(t, http.StatusOK, detail.Code)
	require.Equal(t, "no-store, private", detail.Header().Get("Cache-Control"))
	var payload struct {
		Data struct {
			Stages []struct {
				PayloadText string `json:"payload_text"`
			} `json:"stages"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(bytes.NewReader(detail.Body.Bytes())).Decode(&payload))
	require.Equal(t, "CANARY_PROMPT", payload.Data.Stages[0].PayloadText)
}

// The admin detail is the last reader before the decision reaches a browser, so it
// repeats the repository's trust boundary: only the typed, validated gateway
// decision of a body-less gateway_decision stage is disclosed. A decision that is
// not a closed enum set or that carries free text stays out of the response rather
// than being echoed as a partially trusted value.
func TestRequestTraceAdminDetailDisclosesOnlyValidatedGatewayDecisions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const credentialSentinel = "Bearer sk-ant-oat01-DECISION_SENTINEL"
	id := "0123456789abcdef0123456789abcdef"
	trace := service.RequestTrace{TraceID: id, RouteFamily: service.RequestTraceMessages, InboundEndpoint: "/v1/messages",
		CaptureState: service.RequestTraceStored, ClientStatus: 200, CreatedAt: time.Now()}
	reader := &requestTraceReaderStub{
		items: []service.RequestTrace{trace},
		detail: &service.RequestTraceDetail{RequestTrace: trace, Stages: []service.RequestTraceStage{
			// The one stage that may carry a decision: body-less, not observed.
			{Ordinal: 1, Stage: service.RequestTraceDecisionStage, State: service.RequestTraceNotObserved, Reason: "decision_recorded",
				Decision: &service.RequestTraceDecisionFacts{
					Decision: service.RequestTraceDecisionModelMapping, Outcome: service.RequestTraceDecisionRewritten,
					Source: service.RequestTraceDecisionSourceProtocolConvert, Sequence: 2,
					ModelFrom: "claude-sonnet-4-5", ModelTo: "claude-sonnet-4-5-20250929", AccountID: 73,
				}},
			// The same stage shape, but its decision is outside the closed token set:
			// the free-text credential sentinel fails validation, so it must not be
			// disclosed as an observed value.
			{Ordinal: 2, Stage: service.RequestTraceDecisionStage, State: service.RequestTraceNotObserved, Reason: "decision_recorded",
				Decision: &service.RequestTraceDecisionFacts{
					Decision: service.RequestTraceDecisionIdentity, Outcome: service.RequestTraceDecisionRewritten,
					Source: service.RequestTraceDecisionSourceIdentity, Sequence: 3, ModelTo: credentialSentinel,
				}},
			// A wire attempt cannot claim a decision; the transport stage stays as it is.
			{Ordinal: 3, Stage: "wire_attempt", State: service.RequestTraceStored, Reason: "retained",
				Decision: &service.RequestTraceDecisionFacts{
					Decision: service.RequestTraceDecisionRoute, Outcome: service.RequestTraceDecisionSelected,
					Source: service.RequestTraceDecisionSourceAccount, Sequence: 1, AccountID: 73,
				}},
		}},
	}
	h := NewRequestTraceHandler(reader)

	detail := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(detail)
	c.Set("auth_method", service.AuditAuthMethodJWT)
	c.Params = gin.Params{{Key: "trace_id", Value: id}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/request-traces/"+id, nil)
	h.Get(c)
	require.Equal(t, http.StatusOK, detail.Code)
	var payload struct {
		Data struct {
			Stages []struct {
				Decision *service.RequestTraceDecisionFacts `json:"decision"`
			} `json:"stages"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(detail.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Stages, 3)

	require.NotNil(t, payload.Data.Stages[0].Decision)
	require.Equal(t, service.RequestTraceDecisionModelMapping, payload.Data.Stages[0].Decision.Decision)
	require.Equal(t, service.RequestTraceDecisionRewritten, payload.Data.Stages[0].Decision.Outcome)
	require.Equal(t, "claude-sonnet-4-5-20250929", payload.Data.Stages[0].Decision.ModelTo)

	require.Nil(t, payload.Data.Stages[1].Decision, "an invalid decision must stay undisclosed")
	require.Nil(t, payload.Data.Stages[2].Decision, "only the decision stage may carry a decision")

	body := detail.Body.String()
	require.Contains(t, body, "model_mapping")
	require.NotContains(t, body, credentialSentinel)
	require.NotContains(t, body, "metadata")

	// The list response carries no stages at all, so no decision can leak from it.
	list := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(list)
	c.Set("auth_method", service.AuditAuthMethodJWT)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/request-traces", nil)
	h.List(c)
	require.Equal(t, http.StatusOK, list.Code)
	require.NotContains(t, list.Body.String(), "decision")
	require.NotContains(t, list.Body.String(), "stages")

	// An admin API key session cannot read the detail, so it cannot read a decision.
	key := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(key)
	c.Set("auth_method", service.AuditAuthMethodAdminAPIKey)
	c.Params = gin.Params{{Key: "trace_id", Value: id}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/request-traces/"+id, nil)
	h.Get(c)
	require.Equal(t, http.StatusForbidden, key.Code)
	require.NotContains(t, key.Body.String(), "model_mapping")
	require.NotContains(t, key.Body.String(), credentialSentinel)
}
