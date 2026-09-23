//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type requestAuditLookupStub struct {
	rec *service.RequestAuditRecord
	err error
}

func (s *requestAuditLookupStub) CreateRequestAudit(context.Context, *service.RequestAuditRecord) error {
	return nil
}

func (s *requestAuditLookupStub) GetByUsageLogID(_ context.Context, usageLogID int64) (*service.RequestAuditRecord, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.rec == nil || s.rec.UsageLogID != usageLogID {
		return nil, nil
	}
	return s.rec, nil
}

func setupRequestAuditRouter(repo service.RequestAuditRepository) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewUsageHandler(nil, nil, nil, nil, repo)
	r.GET("/admin/usage/:id/request-audit", h.GetRequestAudit)
	return r
}

func TestAdminUsageGetRequestAuditNotFound(t *testing.T) {
	router := setupRequestAuditRouter(&requestAuditLookupStub{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/usage/7/request-audit", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestAdminUsageGetRequestAuditReturnsSanitizedHeaders(t *testing.T) {
	router := setupRequestAuditRouter(&requestAuditLookupStub{
		rec: &service.RequestAuditRecord{
			UsageLogID: 7,
			Headers: map[string]any{
				"X-Stainless-Lang": "js",
				"Authorization":    map[string]any{"present": true},
			},
		},
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/usage/7/request-audit", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var envelope struct {
		Code int `json:"code"`
		Data struct {
			UsageLogID int64          `json:"usage_log_id"`
			Headers    map[string]any `json:"headers"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Equal(t, int64(7), envelope.Data.UsageLogID)
	require.Equal(t, "js", envelope.Data.Headers["X-Stainless-Lang"])
	auth, ok := envelope.Data.Headers["Authorization"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, auth["present"])
	require.NotContains(t, w.Body.String(), "sk-")
}

func TestAdminUsageGetRequestAuditReturnsEventSkeletonWithoutDelta(t *testing.T) {
	router := setupRequestAuditRouter(&requestAuditLookupStub{
		rec: &service.RequestAuditRecord{
			UsageLogID: 7,
			Headers: map[string]any{
				"X-Stainless-Lang": "js",
			},
			Events: []service.RequestAuditEventSkeleton{
				{Type: "message_start", Index: 0, Bytes: 12},
				{Type: "content_block_delta", Index: 1, Bytes: 40},
				{Type: "message_stop", Index: 2, Bytes: 8},
			},
		},
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/usage/7/request-audit", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var envelope struct {
		Code int `json:"code"`
		Data struct {
			UsageLogID int64          `json:"usage_log_id"`
			Headers    map[string]any `json:"headers"`
			Events     []struct {
				Type  string `json:"type"`
				Index int    `json:"index"`
				Bytes int    `json:"bytes"`
			} `json:"events"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Equal(t, int64(7), envelope.Data.UsageLogID)
	require.Len(t, envelope.Data.Events, 3)
	require.Equal(t, "message_start", envelope.Data.Events[0].Type)
	require.Equal(t, 0, envelope.Data.Events[0].Index)
	require.Equal(t, "content_block_delta", envelope.Data.Events[1].Type)
	require.Equal(t, 1, envelope.Data.Events[1].Index)
	require.Equal(t, 40, envelope.Data.Events[1].Bytes)
	require.Equal(t, "message_stop", envelope.Data.Events[2].Type)
	require.NotContains(t, w.Body.String(), "secret delta")
	require.NotContains(t, w.Body.String(), "text_delta")
}

func TestAdminUsageGetRequestAuditReturnsNotCapturedReason(t *testing.T) {
	router := setupRequestAuditRouter(&requestAuditLookupStub{
		rec: &service.RequestAuditRecord{
			UsageLogID:          7,
			CaptureCompleteness: service.RequestAuditCaptureNotCaptured,
			CaptureReason:       service.RequestAuditNotCapturedReasonPhase1Uncovered,
		},
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/usage/7/request-audit", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var envelope struct {
		Data struct {
			CaptureCompleteness string `json:"capture_completeness"`
			CaptureReason       string `json:"capture_reason"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Equal(t, service.RequestAuditCaptureNotCaptured, envelope.Data.CaptureCompleteness)
	require.Equal(t, service.RequestAuditNotCapturedReasonPhase1Uncovered, envelope.Data.CaptureReason)
}

func TestAdminUsageGetRequestAuditReturnsCaptureCompleteness(t *testing.T) {
	router := setupRequestAuditRouter(&requestAuditLookupStub{
		rec: &service.RequestAuditRecord{
			UsageLogID:          7,
			CaptureCompleteness: service.RequestAuditCaptureTruncated,
			Headers:             map[string]any{"X-Stainless-Lang": "js"},
			Events: []service.RequestAuditEventSkeleton{
				{Type: "truncated", Index: 1, Truncated: true, Dropped: 3, Reason: "max_events"},
			},
		},
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/usage/7/request-audit", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var envelope struct {
		Code int `json:"code"`
		Data struct {
			UsageLogID          int64  `json:"usage_log_id"`
			CaptureCompleteness string `json:"capture_completeness"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Equal(t, int64(7), envelope.Data.UsageLogID)
	require.Equal(t, service.RequestAuditCaptureTruncated, envelope.Data.CaptureCompleteness)
}

func TestAdminUsageGetRequestAuditSanitizesHistoricalRecordFromRepository(t *testing.T) {
	const secret = "sk-historical-admin-secret"
	rec := &service.RequestAuditRecord{
		UsageLogID: 7,
		Headers: map[string]any{
			"X-Stainless-Lang": "js",
			"Authorization":    "Bearer " + secret,
			"X-Untrusted":      secret,
		},
		Events: []service.RequestAuditEventSkeleton{
			{Type: "message_start", Index: 0, Bytes: 12},
			{Type: "secret-event-" + secret, Index: 1, Bytes: 30},
		},
		Attempts: []service.RequestAuditAttempt{{
			AccountID: 4,
			Model:     secret,
			Protocol:  service.RequestAuditProtocolOpenAIResp,
			Stage:     service.RequestAuditStageWire,
		}},
		Metadata: service.RequestAuditMetadata{
			Routes: map[string]string{
				"inbound":  "/v1/responses",
				"upstream": "/v1/responses?api_key=" + secret,
				"extra":    secret,
			},
			IDs: map[string]string{"session": secret, "response": "resp-" + secret},
		},
	}
	router := setupRequestAuditRouter(&requestAuditLookupStub{rec: rec})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/usage/7/request-audit", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var envelope struct {
		Data struct {
			Headers  map[string]any                      `json:"headers"`
			Events   []service.RequestAuditEventSkeleton `json:"events"`
			Attempts []service.RequestAuditAttempt       `json:"attempts"`
			Metadata service.RequestAuditMetadata        `json:"metadata"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Equal(t, "js", envelope.Data.Headers["X-Stainless-Lang"])
	require.NotContains(t, envelope.Data.Headers, "X-Untrusted")
	require.Len(t, envelope.Data.Events, 2)
	require.Equal(t, "message_start", envelope.Data.Events[0].Type)
	require.Equal(t, "unknown", envelope.Data.Events[1].Type)
	require.Equal(t, 1, envelope.Data.Events[1].Index)
	require.Equal(t, 30, envelope.Data.Events[1].Bytes)
	require.Len(t, envelope.Data.Attempts, 1)
	require.Empty(t, envelope.Data.Attempts[0].Model)
	require.Equal(t, service.RequestAuditProtocolOpenAIResp, envelope.Data.Attempts[0].Protocol)
	require.Equal(t, map[string]string{"inbound": "/v1/responses"}, envelope.Data.Metadata.Routes)
	require.Empty(t, envelope.Data.Metadata.IDs)
	require.NotContains(t, w.Body.String(), secret)
	require.NotContains(t, w.Body.String(), "api_key")
}

func TestAdminUsageGetRequestAuditReturnsAttempts(t *testing.T) {
	router := setupRequestAuditRouter(&requestAuditLookupStub{
		rec: &service.RequestAuditRecord{
			UsageLogID: 7,
			Headers:    map[string]any{"X-Stainless-Lang": "js"},
			Attempts: []service.RequestAuditAttempt{
				{AccountID: 701, Model: "gpt-5.1", Protocol: service.RequestAuditProtocolOpenAIChat, Stage: service.RequestAuditStageWire},
				{AccountID: 702, Model: "gpt-5.1", Protocol: service.RequestAuditProtocolOpenAIChat, Stage: service.RequestAuditStageWire},
			},
		},
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/usage/7/request-audit", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var envelope struct {
		Code int `json:"code"`
		Data struct {
			UsageLogID int64 `json:"usage_log_id"`
			Attempts   []struct {
				AccountID int64  `json:"account_id"`
				Model     string `json:"model"`
				Protocol  string `json:"protocol"`
				Stage     string `json:"stage"`
			} `json:"attempts"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Equal(t, int64(7), envelope.Data.UsageLogID)
	require.Len(t, envelope.Data.Attempts, 2)
	require.Equal(t, int64(701), envelope.Data.Attempts[0].AccountID)
	require.Equal(t, int64(702), envelope.Data.Attempts[1].AccountID)
	require.Empty(t, envelope.Data.Attempts[0].Model)
	require.Empty(t, envelope.Data.Attempts[1].Model)
	require.Equal(t, service.RequestAuditProtocolOpenAIChat, envelope.Data.Attempts[0].Protocol)
	require.NotContains(t, w.Body.String(), "secret prompt")
	require.NotContains(t, w.Body.String(), "sk-")
}

func TestAdminUsageGetRequestAuditReturnsSanitizedProtocolFields(t *testing.T) {
	const (
		toolNameCanary = "raw-tool-name-canary"
		nameCanary     = "raw-name-canary"
		saltCanary     = "request-audit-key-salt-canary"
	)
	stream := false
	router := setupRequestAuditRouter(&requestAuditLookupStub{
		rec: &service.RequestAuditRecord{
			UsageLogID:      7,
			FingerprintSalt: []byte(saltCanary),
			Metadata: service.RequestAuditMetadata{
				ProtocolFields: &service.RequestAuditProtocolFields{
					Stream:           &stream,
					ThinkingType:     "adaptive",
					PresentFields:    []string{"model", "thinking", "tool_name:" + toolNameCanary, "name:" + nameCanary, "thinking_type:unknown-enum-canary"},
					NormalizedFields: []string{"thinking.extra_fields_removed", "thinking." + nameCanary},
				},
			},
		},
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/usage/7/request-audit", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var envelope struct {
		Data struct {
			UsageLogID int64 `json:"usage_log_id"`
			Metadata   struct {
				ProtocolFields *service.RequestAuditProtocolFields `json:"protocol_fields"`
			} `json:"metadata"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Equal(t, int64(7), envelope.Data.UsageLogID)
	fields := envelope.Data.Metadata.ProtocolFields
	require.NotNil(t, fields)
	require.NotNil(t, fields.Stream)
	require.False(t, *fields.Stream, "explicit false must remain distinguishable from unknown")
	require.Equal(t, "adaptive", fields.ThinkingType)
	require.Equal(t, []string{"model", "thinking"}, fields.PresentFields)
	require.Equal(t, []string{"thinking.extra_fields_removed"}, fields.NormalizedFields)
	require.NotContains(t, w.Body.String(), toolNameCanary)
	require.NotContains(t, w.Body.String(), nameCanary)
	require.NotContains(t, w.Body.String(), "unknown-enum-canary")
	require.NotContains(t, w.Body.String(), saltCanary)
	require.NotContains(t, w.Body.String(), "fingerprint_salt")
}
