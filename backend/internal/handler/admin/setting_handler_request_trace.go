package admin

import (
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

var errRequestTraceAdminSessionRequired = infraerrors.Forbidden(
	"REQUEST_TRACE_ADMIN_SESSION_REQUIRED", "an admin login session is required to enable request traces",
)

type requestTraceSettingsRequest struct {
	Enabled  bool   `json:"enabled"`
	Language string `json:"language"`
	Phrase   string `json:"phrase"`
}

// GetRequestTraceOperatorSettings reports both the stored switch and the effective gate.
func (h *SettingHandler) GetRequestTraceOperatorSettings(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if h == nil || h.settingService == nil {
		response.ErrorFrom(c, service.ErrRequestTraceSettingsUnavailable)
		return
	}
	status, err := h.settingService.GetRequestTraceOperatorStatus(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, service.ErrRequestTraceSettingsUnavailable)
		return
	}
	response.Success(c, status)
}

// UpdateRequestTraceOperatorSettings requires an admin session only when enabling.
// Disabling remains available to admins even if the ownership probe is unavailable.
func (h *SettingHandler) UpdateRequestTraceOperatorSettings(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	var req requestTraceSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if req.Enabled && c.GetString("auth_method") != service.AuditAuthMethodJWT {
		response.ErrorFrom(c, errRequestTraceAdminSessionRequired)
		return
	}
	if h == nil || h.settingService == nil {
		response.ErrorFrom(c, service.ErrRequestTraceSettingsUnavailable)
		return
	}
	adminUserID := int64(0)
	if subject, ok := middleware.GetAuthSubjectFromContext(c); ok {
		adminUserID = subject.UserID
	}
	status, err := h.settingService.UpdateRequestTraceOperatorSettings(c.Request.Context(), service.RequestTraceOperatorUpdateInput{
		Enabled: req.Enabled, Language: req.Language, Phrase: req.Phrase, AdminUserID: adminUserID,
		IPAddress: ip.GetClientIP(c), UserAgent: strings.TrimSpace(c.GetHeader("User-Agent")),
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}
