package admin

import (
	"context"
	"net/http"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// errRequestTraceStatusUnavailable is a deployment fact, not a transient read
// failure: nothing is wired to report on. Its own code keeps it distinguishable
// from the trace reader's temporary unavailability.
var errRequestTraceStatusUnavailable = infraerrors.New(http.StatusServiceUnavailable,
	"REQUEST_TRACE_STATUS_UNAVAILABLE", "request trace status is not available on this deployment")

// requestTraceStatusReader is the whole surface this handler needs: one
// value-free snapshot. It cannot read a trace, a stage, a body or a filter.
type requestTraceStatusReader interface {
	Status(ctx context.Context) service.RequestTraceOpsStatus
}

// RequestTraceStatusHandler exposes the request-trace operational state an
// operator needs to tell "capturing" from "silently failing": capture queue
// depth and write failures, the export worker's counters, and the bounded
// unlinked-cleanup backlog.
//
// It is deliberately weaker than the trace reader: the answer is counts and
// closed-set enums only, so it needs no admin-session gate and discloses no
// body, header value, credential or raw query string. The storage probe inside
// it is what keeps a database outage visible while no request is in flight,
// which a pure counter read cannot do.
type RequestTraceStatusHandler struct {
	reader requestTraceStatusReader
}

func NewRequestTraceStatusHandler(reader requestTraceStatusReader) *RequestTraceStatusHandler {
	return &RequestTraceStatusHandler{reader: reader}
}

// Get returns the current value-free status.
// GET /api/v1/admin/request-traces/status
//
// The path is a static sibling of /request-traces/:trace_id; gin resolves the
// static segment first, so "status" can never be read as a trace id.
func (h *RequestTraceStatusHandler) Get(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if h == nil || h.reader == nil {
		// An unwired deployment must say so rather than answer with zeros that
		// look like a healthy, idle process.
		response.ErrorFrom(c, errRequestTraceStatusUnavailable)
		return
	}
	response.Success(c, h.reader.Status(c.Request.Context()))
}
