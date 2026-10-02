//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type requestTraceDeleteStub struct {
	preview    *service.RequestTraceDeletePreview
	previewErr error

	byFilterResult *service.RequestTraceDeleteResult
	byFilterErr    error
	receivedReq    service.RequestTraceDeleteByFilterRequest
	receivedAdmin  int64

	byIDsResult *service.RequestTraceDeleteResult
	byIDsErr    error
	receivedIDs []string
	receivedCon bool
}

func (s *requestTraceDeleteStub) PreviewDelete(_ context.Context, _ service.RequestTraceExportFilter, adminID int64) (*service.RequestTraceDeletePreview, error) {
	s.receivedAdmin = adminID
	return s.preview, s.previewErr
}

func (s *requestTraceDeleteStub) DeleteByFilter(_ context.Context, request service.RequestTraceDeleteByFilterRequest, adminID int64) (*service.RequestTraceDeleteResult, error) {
	s.receivedReq, s.receivedAdmin = request, adminID
	return s.byFilterResult, s.byFilterErr
}

func (s *requestTraceDeleteStub) DeleteByIDs(_ context.Context, traceIDs []string, confirm bool) (*service.RequestTraceDeleteResult, error) {
	s.receivedIDs, s.receivedCon = traceIDs, confirm
	return s.byIDsResult, s.byIDsErr
}

func newRequestTraceDeleteHandlerContext(method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("auth_method", service.AuditAuthMethodJWT)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
	c.Set(middleware.ContextKeySessionID, "session-1")
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder
}

func TestRequestTraceDeleteEndpointsRequireAdminLoginSession(t *testing.T) {
	h := NewRequestTraceHandler(&requestTraceReaderStub{}, &requestTraceDeleteStub{})

	for _, tc := range []struct {
		name string
		run  func(*gin.Context)
	}{
		{"preview", h.DeletePreview},
		{"by-filter", h.DeleteByFilter},
		{"batch", h.BatchDelete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := newRequestTraceDeleteHandlerContext(http.MethodPost, "/api/v1/admin/request-traces/x", `{}`)
			c.Set("auth_method", service.AuditAuthMethodAdminAPIKey)
			tc.run(c)
			require.Equal(t, http.StatusForbidden, recorder.Code)
			require.Equal(t, "no-store, private", recorder.Header().Get("Cache-Control"))
		})
	}

	// A JWT session without a bound session id is not a login session.
	c, recorder := newRequestTraceDeleteHandlerContext(http.MethodPost, "/api/v1/admin/request-traces/delete-preview", `{"filter":{"platform":"openai"}}`)
	c.Set(middleware.ContextKeySessionID, "")
	h.DeletePreview(c)
	require.Equal(t, http.StatusForbidden, recorder.Code)
}

func TestRequestTraceDeletePreviewReturnsBoundSnapshot(t *testing.T) {
	stub := &requestTraceDeleteStub{preview: &service.RequestTraceDeletePreview{
		MatchedCount: 3, SnapshotMaxID: 77, FilterHash: "hash", ConfirmationToken: "token",
		ExpiresAt: time.Date(2026, 10, 2, 12, 5, 0, 0, time.UTC),
	}}
	h := NewRequestTraceHandler(&requestTraceReaderStub{}, stub)
	c, recorder := newRequestTraceDeleteHandlerContext(http.MethodPost, "/api/v1/admin/request-traces/delete-preview", `{"filter":{"platform":"openai"}}`)
	h.DeletePreview(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, int64(7), stub.receivedAdmin)
	var payload struct {
		Data service.RequestTraceDeletePreview `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.Equal(t, int64(3), payload.Data.MatchedCount)
	require.Equal(t, int64(77), payload.Data.SnapshotMaxID)
	require.Equal(t, "token", payload.Data.ConfirmationToken)
}

func TestRequestTraceDeleteByFilterReportsPartialAsIncomplete(t *testing.T) {
	stub := &requestTraceDeleteStub{
		byFilterResult: &service.RequestTraceDeleteResult{DeletedCount: 12, Completed: false},
		byFilterErr:    context.DeadlineExceeded,
	}
	h := NewRequestTraceHandler(&requestTraceReaderStub{}, stub)
	c, recorder := newRequestTraceDeleteHandlerContext(http.MethodPost, "/api/v1/admin/request-traces/delete-by-filter",
		`{"filter":{"platform":"openai"},"snapshot_max_id":77,"filter_hash":"hash","confirmation_token":"token","confirm":true}`)
	h.DeleteByFilter(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	var payload struct {
		Data service.RequestTraceDeleteResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.Equal(t, int64(12), payload.Data.DeletedCount)
	require.False(t, payload.Data.Completed)
	require.Equal(t, "token", stub.receivedReq.ConfirmationToken)
}

func TestRequestTraceDeleteByFilterMapsInvalidConfirmationTo400(t *testing.T) {
	stub := &requestTraceDeleteStub{byFilterErr: service.ErrRequestTraceDeleteConfirmationInvalid}
	h := NewRequestTraceHandler(&requestTraceReaderStub{}, stub)
	c, recorder := newRequestTraceDeleteHandlerContext(http.MethodPost, "/api/v1/admin/request-traces/delete-by-filter", `{"confirm":true}`)
	h.DeleteByFilter(c)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "REQUEST_TRACE_DELETE_CONFIRMATION_INVALID")
}

func TestRequestTraceBatchDeleteReturnsRealCount(t *testing.T) {
	stub := &requestTraceDeleteStub{byIDsResult: &service.RequestTraceDeleteResult{DeletedCount: 2, Completed: true}}
	h := NewRequestTraceHandler(&requestTraceReaderStub{}, stub)
	c, recorder := newRequestTraceDeleteHandlerContext(http.MethodPost, "/api/v1/admin/request-traces/batch-delete",
		`{"trace_ids":["0123456789abcdef0123456789abcdef","fedcba9876543210fedcba9876543210"],"confirm":true}`)
	h.BatchDelete(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.True(t, stub.receivedCon)
	require.Len(t, stub.receivedIDs, 2)
	var payload struct {
		Data service.RequestTraceDeleteResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.Equal(t, int64(2), payload.Data.DeletedCount)
}

func TestRequestTraceDeleteEndpointsUnavailableWithoutService(t *testing.T) {
	h := NewRequestTraceHandler(&requestTraceReaderStub{})
	c, recorder := newRequestTraceDeleteHandlerContext(http.MethodPost, "/api/v1/admin/request-traces/batch-delete", `{"trace_ids":[],"confirm":true}`)
	h.BatchDelete(c)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

func TestRequestTraceDeleteRejectsMalformedBody(t *testing.T) {
	h := NewRequestTraceHandler(&requestTraceReaderStub{}, &requestTraceDeleteStub{})
	c, recorder := newRequestTraceDeleteHandlerContext(http.MethodPost, "/api/v1/admin/request-traces/delete-preview", `{"filter":`)
	h.DeletePreview(c)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "REQUEST_TRACE_DELETE_REQUEST_MALFORMED")
}
