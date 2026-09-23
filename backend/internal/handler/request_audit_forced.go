package handler

import (
	"context"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const requestAuditUnavailableMessage = "Required request audit metadata is unavailable"

type requestAuditPreparer interface {
	PrepareRequestAudit(context.Context, *gin.Context, service.RequestAuditRouteFamily) (context.Context, error)
}

func prepareRequestAuditOrReject(c *gin.Context, preparer requestAuditPreparer, family service.RequestAuditRouteFamily, messages bool) bool {
	if c == nil || c.Request == nil || preparer == nil {
		return true
	}
	ctx, err := preparer.PrepareRequestAudit(c.Request.Context(), c, family)
	if err != nil {
		writeRequestAuditUnavailable(c, messages)
		return false
	}
	c.Request = c.Request.WithContext(ctx)
	return true
}

func requestAuditIsForced(c *gin.Context) bool {
	scope, ok := service.RequestAuditReservationScopeFromGin(c)
	return ok && scope.Forced
}

func writeRequestAuditUnavailable(c *gin.Context, messages bool) {
	errorBody := gin.H{
		"type":    "server_error",
		"code":    "audit_unavailable",
		"message": requestAuditUnavailableMessage,
	}
	body := gin.H{"error": errorBody}
	if messages {
		body["type"] = "error"
	}
	c.JSON(http.StatusServiceUnavailable, body)
}
