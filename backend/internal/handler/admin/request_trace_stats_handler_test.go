//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type traceStatsReader struct{ requestTraceReaderStub }

func (r *traceStatsReader) ListRequestTraces(_ context.Context, filter service.RequestTraceListFilter) ([]service.RequestTrace, int64, error) {
	if filter.SkipCount {
		return nil, 0, nil
	}
	return nil, 3, nil
}

func (r *traceStatsReader) RequestTraceQueryStats(context.Context, service.RequestTraceListFilter) (service.RequestTraceQueryStats, error) {
	return service.RequestTraceQueryStats{MatchedTotal: 3, Status: service.RequestTraceQueryStatusCounts{OK: 1, Other: 2},
		Capture: service.RequestTraceQueryCaptureCounts{Stored: 1, NotObserved: 2},
		Usage:   service.RequestTraceQueryUsageCounts{Unlinked: 3}}, nil
}

func TestRequestTraceAdminListCanIncludeFilteredStatsWithoutBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("auth_method", service.AuditAuthMethodJWT)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/request-traces?include_stats=true", nil)
	h := NewRequestTraceHandler(&traceStatsReader{})
	h.List(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	var body struct {
		Data struct {
			Total int64                          `json:"total"`
			Stats service.RequestTraceQueryStats `json:"stats"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, int64(3), body.Data.Stats.MatchedTotal)
	require.Equal(t, body.Data.Stats.MatchedTotal, body.Data.Total)
	require.Equal(t, int64(1), body.Data.Stats.Status.OK)
	require.NotContains(t, recorder.Body.String(), "payload_text")
}
