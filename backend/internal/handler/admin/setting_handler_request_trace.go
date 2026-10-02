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

	// 采集内容／限时字段全部指针可选：缺字段（nil）保留既有值，显式 false／0 必须
	// 能与"未提交"区分开。capture_until 不接受客户端提交，由服务端计算。
	CaptureBody            *bool  `json:"capture_body"`
	CaptureHTTP200         *bool  `json:"capture_http_200"`
	SampleRateHTTP200      *int   `json:"sample_rate_http_200"`
	SampleRateOther        *int   `json:"sample_rate_other"`
	BodyMaxBytes           *int64 `json:"body_max_bytes"`
	CaptureDurationSeconds *int64 `json:"capture_duration_seconds"`
	RenewCaptureWindow     bool   `json:"renew_capture_window"`

	// 采集范围：未提交（scope_provided 为 false）时保留既有范围，避免开关动作顺手清空范围。
	ScopeProvided bool     `json:"scope_provided"`
	AllGroups     bool     `json:"all_groups"`
	GroupIDs      []int64  `json:"group_ids"`
	ModelScope    string   `json:"model_scope"`
	Models        []string `json:"models"`
	PlatformScope string   `json:"platform_scope"`
	Platforms     []string `json:"platforms"`
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

// GetRequestTraceExportRisk 返回明文导出副本的独立风险确认状态。
// 它与采集确认分开：开启采集不等于接受"可把明文副本写到本机临时文件"。
func (h *SettingHandler) GetRequestTraceExportRisk(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if h == nil || h.settingService == nil {
		response.ErrorFrom(c, service.ErrRequestTraceSettingsUnavailable)
		return
	}
	status, err := h.settingService.GetRequestTraceExportRiskStatus(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, service.ErrRequestTraceSettingsUnavailable)
		return
	}
	response.Success(c, status)
}

// GetRequestTraceExportLimits 返回当前生效的导出任务上限。
func (h *SettingHandler) GetRequestTraceExportLimits(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if h == nil || h.settingService == nil {
		response.ErrorFrom(c, service.ErrRequestTraceSettingsUnavailable)
		return
	}
	response.Success(c, h.settingService.GetRequestTraceExportLimits())
}

// requestTraceExportLimitsRequest 是管理端提交的任务上限。所有值都必须有限且为正，
// 服务端会夹到允许区间内，绝不接受"无限制"。
type requestTraceExportLimitsRequest struct {
	MaxRows       int64 `json:"max_rows"`
	MaxBytes      int64 `json:"max_bytes"`
	MaxRuntimeSec int64 `json:"max_runtime_seconds"`
	MaxShardRows  int64 `json:"max_shard_rows"`
	MaxShardBytes int64 `json:"max_shard_bytes"`
}

// UpdateRequestTraceExportLimits 保存导出任务上限。仅管理员登录会话可改。
func (h *SettingHandler) UpdateRequestTraceExportLimits(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if c.GetString("auth_method") != service.AuditAuthMethodJWT {
		response.ErrorFrom(c, errRequestTraceAdminSessionRequired)
		return
	}
	var req requestTraceExportLimitsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if h == nil || h.settingService == nil {
		response.ErrorFrom(c, service.ErrRequestTraceSettingsUnavailable)
		return
	}
	limits, err := h.settingService.UpdateRequestTraceExportLimits(c.Request.Context(), service.RequestTraceExportLimits{
		MaxRows: req.MaxRows, MaxBytes: req.MaxBytes, MaxRuntimeSec: req.MaxRuntimeSec,
		MaxShardRows: req.MaxShardRows, MaxShardBytes: req.MaxShardBytes,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, limits)
}

// AcknowledgeRequestTraceExportRisk 记录一次逐字确认。只有管理员登录会话可提交。
func (h *SettingHandler) AcknowledgeRequestTraceExportRisk(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if c.GetString("auth_method") != service.AuditAuthMethodJWT {
		response.ErrorFrom(c, errRequestTraceAdminSessionRequired)
		return
	}
	var req requestTraceSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
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
	status, err := h.settingService.AcknowledgeRequestTraceExportRisk(c.Request.Context(), service.RequestTraceExportRiskUpdateInput{
		Language: req.Language, Phrase: req.Phrase, AdminUserID: adminUserID,
		IPAddress: ip.GetClientIP(c), UserAgent: strings.TrimSpace(c.GetHeader("User-Agent")),
	})
	if err != nil {
		response.ErrorFrom(c, err)
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

		CaptureBody:            req.CaptureBody,
		CaptureHTTP200:         req.CaptureHTTP200,
		SampleRateHTTP200:      req.SampleRateHTTP200,
		SampleRateOther:        req.SampleRateOther,
		BodyMaxBytes:           req.BodyMaxBytes,
		CaptureDurationSeconds: req.CaptureDurationSeconds,
		RenewCaptureWindow:     req.RenewCaptureWindow,

		ScopeProvided: req.ScopeProvided,
		AllGroups:     req.AllGroups,
		GroupIDs:      req.GroupIDs,
		ModelScope:    req.ModelScope,
		Models:        req.Models,
		PlatformScope: req.PlatformScope,
		Platforms:     req.Platforms,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}
