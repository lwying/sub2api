package admin

import (
	"net/http"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// 错误诊断运维开关的管理员入口。
//
// 路由（挂在既有 admin 组，由 adminAuth 中间件保证仅管理员可访问）：
//
//	GET /api/v1/admin/settings/error-diagnostic   读取门控与书面确认状态
//	PUT /api/v1/admin/settings/error-diagnostic   开启／关闭（开启必须逐字风险确认）
//
// 响应只含门控结论、必填语句与确认元数据（版本／语句原文／管理员 ID／确认时间）：
// 不回显加密密钥、诊断正文，也不回显确认记录的来源 IP／User-Agent；入口不接受任何
// 过滤参数，因此无法被用作按身份检索客户数据的通道。设置类响应禁止中间缓存。

type errorDiagnosticOperatorSettingsRequest struct {
	Enabled              bool   `json:"enabled"`
	BodyRetentionEnabled bool   `json:"body_retention_enabled"`
	Language             string `json:"language"`
	Phrase               string `json:"phrase"`
}

var (
	// errErrorDiagnosticSettingsUnavailable 用于「问不出状态」的情形：宁可显式不可用，
	// 也不把读取失败渲染成「已关闭」。
	errErrorDiagnosticSettingsUnavailable = infraerrors.New(
		http.StatusServiceUnavailable,
		"ERROR_DIAGNOSTIC_SETTINGS_UNAVAILABLE",
		"error diagnostic settings are temporarily unavailable",
	)
	// errErrorDiagnosticAdminAPIKeyForbidden 拒绝用机器凭证代替操作员的书面确认。
	// 与 step-up 开关同理：一次性、自锁风险的门控只能由管理员会话显式开启。
	errErrorDiagnosticAdminAPIKeyForbidden = infraerrors.Forbidden(
		"ERROR_DIAGNOSTIC_ADMIN_API_KEY_FORBIDDEN",
		"enabling error diagnostics requires an admin session, not an admin API key",
	)
)

// GetErrorDiagnosticOperatorSettings 读取错误诊断运维开关状态。
// GET /api/v1/admin/settings/error-diagnostic
func (h *SettingHandler) GetErrorDiagnosticOperatorSettings(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if h == nil || h.settingService == nil {
		response.ErrorFrom(c, errErrorDiagnosticSettingsUnavailable)
		return
	}
	status, err := h.settingService.GetErrorDiagnosticOperatorStatus(c.Request.Context())
	if err != nil {
		// 读取失败不是「关闭」：显式返回不可用，不假装门控是关的。
		response.ErrorFrom(c, errErrorDiagnosticSettingsUnavailable)
		return
	}
	response.Success(c, status)
}

// UpdateErrorDiagnosticOperatorSettings 更新错误诊断运维开关。
// PUT /api/v1/admin/settings/error-diagnostic
//
// 这是整体状态更新（与 /settings 大对象的部分更新语义不同）：未提交的字段按关闭处理，
// 不做隐式保留——开启与正文留存都必须由本次请求显式表达。开启由服务层强制逐字风险确认
// （每次更新都校验）；关闭不需要确认、不需要操作员身份，也不能被任何前置校验挡住，
// 且会把正文留存一并关掉。
func (h *SettingHandler) UpdateErrorDiagnosticOperatorSettings(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if h == nil || h.settingService == nil {
		response.ErrorFrom(c, errErrorDiagnosticSettingsUnavailable)
		return
	}
	var req errorDiagnosticOperatorSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	// 机器凭证可以读、可以关，但不能开：书面确认是不可由常量代替的操作员动作。
	if req.Enabled && c.GetString("auth_method") == service.AuditAuthMethodAdminAPIKey {
		response.ErrorFrom(c, errErrorDiagnosticAdminAPIKeyForbidden)
		return
	}

	adminUserID := int64(0)
	if subject, ok := middleware.GetAuthSubjectFromContext(c); ok {
		adminUserID = subject.UserID
	}

	status, err := h.settingService.UpdateErrorDiagnosticOperatorSettings(c.Request.Context(), service.ErrorDiagnosticOperatorUpdateInput{
		Enabled:              req.Enabled,
		BodyRetentionEnabled: req.BodyRetentionEnabled,
		Language:             req.Language,
		Phrase:               req.Phrase,
		AdminUserID:          adminUserID,
		IPAddress:            ip.GetClientIP(c),
		UserAgent:            strings.TrimSpace(c.GetHeader("User-Agent")),
	})
	if err != nil {
		// 校验类错误带稳定 reason 码；其余（存储写入失败等）按不可用处理，
		// 不把失败渲染成成功，也不回显内部细节。
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}
