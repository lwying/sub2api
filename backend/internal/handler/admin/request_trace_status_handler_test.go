package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type requestTraceStatusReaderStub struct {
	status service.RequestTraceOpsStatus
	calls  int
}

func (s *requestTraceStatusReaderStub) Status(context.Context) service.RequestTraceOpsStatus {
	s.calls++
	return s.status
}

func serveRequestTraceStatus(t *testing.T, handler *RequestTraceStatusHandler) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/request-traces/status", nil)
	handler.Get(c)
	return recorder
}

func TestRequestTraceStatusHandlerReportsValueFreeCounts(t *testing.T) {
	stub := &requestTraceStatusReaderStub{status: service.RequestTraceOpsStatus{
		StorageProbe: service.RequestTraceStorageProbeReachable,
		Capture: service.RequestTraceOpsCapture{
			Storage: service.RequestTraceStorageFailing, RepositoryAvailable: true,
			QueueDepth: 3, QueueCapacity: 8, Accepted: 41, Stored: 38, WriteFailed: 3, Dropped: 1, Rejected: 2,
		},
		Export:  service.RequestTraceOpsExport{WorkerStarted: true, Ticks: 9, TasksCompleted: 4},
		Cleanup: service.RequestTraceOpsCleanup{Runs: 6, Deleted: 12, Backlog: 40, BacklogLimit: 1000, BacklogState: service.RequestTraceBacklogMeasured},
	}}

	recorder := serveRequestTraceStatus(t, NewRequestTraceStatusHandler(stub))

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store, private", recorder.Header().Get("Cache-Control"))
	require.Equal(t, 1, stub.calls)

	var body struct {
		Data service.RequestTraceOpsStatus `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, service.RequestTraceStorageFailing, body.Data.Capture.Storage)
	require.Equal(t, uint64(3), body.Data.Capture.WriteFailed)
	require.Equal(t, 3, body.Data.Capture.QueueDepth)
	require.Equal(t, service.RequestTraceBacklogMeasured, body.Data.Cleanup.BacklogState)
	require.Equal(t, int64(40), body.Data.Cleanup.Backlog)
	require.Equal(t, service.RequestTraceStorageProbeReachable, body.Data.StorageProbe)

	// The payload is a closed set of counts and enums: no key may advertise a
	// body, a header value, a credential or a raw query string.
	lowered := strings.ToLower(recorder.Body.String())
	for _, forbidden := range []string{
		"payload", "body", "header", "authorization", "bearer", "token", "secret",
		"api_key", "apikey", "credential", "query", "filename",
	} {
		require.NotContains(t, lowered, forbidden, "the status payload must not carry %q", forbidden)
	}
	require.Contains(t, lowered, "unlinked_backlog", "the bounded backlog must be reported")
}

// Zero counters from an unwired deployment look exactly like a healthy idle
// process, so this handler must refuse instead of answering with zeros.
func TestRequestTraceStatusHandlerUnavailableWithoutReader(t *testing.T) {
	cases := []struct {
		name    string
		handler *RequestTraceStatusHandler
	}{
		{"nil handler", nil},
		{"nil reader", NewRequestTraceStatusHandler(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := serveRequestTraceStatus(t, tc.handler)
			require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
			require.Contains(t, recorder.Body.String(), "REQUEST_TRACE_STATUS_UNAVAILABLE")
		})
	}
}
