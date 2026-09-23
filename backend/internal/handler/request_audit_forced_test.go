//go:build unit

package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type requestAuditPreparerStub struct {
	err error
}

func (s requestAuditPreparerStub) PrepareRequestAudit(ctx context.Context, _ *gin.Context, _ service.RequestAuditRouteFamily) (context.Context, error) {
	return ctx, s.err
}

func TestPrepareRequestAuditOrRejectReturnsExplainable503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, messages := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		ok := prepareRequestAuditOrReject(c, requestAuditPreparerStub{err: &httpattempt.RequiredAuditError{Cause: errors.New("db down")}}, service.RequestAuditRouteResponses, messages)
		require.False(t, ok)
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
		require.Contains(t, recorder.Body.String(), "audit_unavailable")
		require.NotContains(t, recorder.Body.String(), "db down")
		if messages {
			require.Contains(t, recorder.Body.String(), `"type":"error"`)
		}
	}
}
