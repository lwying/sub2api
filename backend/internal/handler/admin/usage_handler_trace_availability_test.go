//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type adminUsageDiagnosticsRepo struct {
	service.UsageLogRepository
	includeDiagnostics bool
}

func (r *adminUsageDiagnosticsRepo) ListWithFilters(_ context.Context, _ pagination.PaginationParams, filters usagestats.UsageLogFilters) ([]service.UsageLog, *pagination.PaginationResult, error) {
	r.includeDiagnostics = filters.IncludeAdminDiagnostics
	id := "abcdabcdabcdabcdabcdabcdabcdabcd"
	return []service.UsageLog{{ID: 42, RequestTraceID: &id, RequestTraceAvailable: true, RequestAuditForcedAvailable: true}},
		&pagination.PaginationResult{Total: 1, Page: 1, PageSize: 20}, nil
}

func TestAdminUsageListExposesReadableDiagnosticsOnlyToLoginSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		method string
		trace  bool
	}{
		{method: service.AuditAuthMethodJWT, trace: true},
		{method: "admin_api_key", trace: false},
	} {
		t.Run(tc.method, func(t *testing.T) {
			r := gin.New()
			r.Use(func(c *gin.Context) { c.Set("auth_method", tc.method); c.Next() })
			repo := &adminUsageDiagnosticsRepo{}
			h := NewUsageHandler(service.NewUsageService(repo, nil, nil, nil), nil, nil, nil, nil)
			r.GET("/admin/usage", h.List)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/usage", nil))
			require.Equal(t, http.StatusOK, w.Code)
			require.Equal(t, tc.trace, repo.includeDiagnostics)
			var body struct {
				Data struct {
					Items []struct {
						RequestTraceID              *string `json:"request_trace_id"`
						RequestTraceAvailable       bool    `json:"request_trace_available"`
						RequestAuditForcedAvailable bool    `json:"request_audit_forced_available"`
					} `json:"items"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.Len(t, body.Data.Items, 1)
			item := body.Data.Items[0]
			require.Equal(t, tc.trace, item.RequestTraceAvailable)
			require.Equal(t, tc.trace, item.RequestAuditForcedAvailable)
			if tc.trace {
				require.NotNil(t, item.RequestTraceID)
			} else {
				require.Nil(t, item.RequestTraceID)
			}
		})
	}
}
