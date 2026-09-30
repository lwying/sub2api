package admin

import (
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 下游测试请求 mock 的管理端接口。
//
// 规则回复会直接发给下游，因此只有管理员登录会话可以修改开关与规则；
// 管理员 API Key 与普通用户都不得改动。

var errGatewayMockAdminSessionRequired = infraerrors.Forbidden(
	"GATEWAY_MOCK_ADMIN_SESSION_REQUIRED", "an admin login session is required to change gateway mock rules",
)

type gatewayMockRuleRequest struct {
	ID      string `json:"id"`
	Keyword string `json:"keyword"`
	Reply   string `json:"reply"`
	Enabled bool   `json:"enabled"`
}

type gatewayMockSettingsRequest struct {
	Enabled bool                     `json:"enabled"`
	Rules   []gatewayMockRuleRequest `json:"rules"`
}

// GetGatewayMockOperatorSettings 返回完整规则集合（含未启用规则）与总开关。
func (h *SettingHandler) GetGatewayMockOperatorSettings(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if h == nil || h.settingService == nil {
		response.ErrorFrom(c, service.ErrGatewayMockSettingsUnavailable)
		return
	}
	status, err := h.settingService.GetGatewayMockOperatorStatus(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, service.ErrGatewayMockSettingsUnavailable)
		return
	}
	response.Success(c, status)
}

// UpdateGatewayMockOperatorSettings 原子替换完整规则集合与总开关。
func (h *SettingHandler) UpdateGatewayMockOperatorSettings(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if c.GetString("auth_method") != service.AuditAuthMethodJWT {
		response.ErrorFrom(c, errGatewayMockAdminSessionRequired)
		return
	}
	var req gatewayMockSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if h == nil || h.settingService == nil {
		response.ErrorFrom(c, service.ErrGatewayMockSettingsUnavailable)
		return
	}
	rules := make([]service.GatewayMockRuleInput, 0, len(req.Rules))
	for _, rule := range req.Rules {
		rules = append(rules, service.GatewayMockRuleInput{
			ID:      strings.TrimSpace(rule.ID),
			Keyword: rule.Keyword,
			Reply:   rule.Reply,
			Enabled: rule.Enabled,
		})
	}
	status, err := h.settingService.UpdateGatewayMockOperatorSettings(c.Request.Context(), service.GatewayMockOperatorUpdateInput{
		Enabled: req.Enabled,
		Rules:   rules,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}

// SeedGatewayMockPresets 在规则集合为空时写入预置词（默认不启用），供管理员按需启用。
func (h *SettingHandler) SeedGatewayMockPresets(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if c.GetString("auth_method") != service.AuditAuthMethodJWT {
		response.ErrorFrom(c, errGatewayMockAdminSessionRequired)
		return
	}
	if h == nil || h.settingService == nil {
		response.ErrorFrom(c, service.ErrGatewayMockSettingsUnavailable)
		return
	}
	status, created, err := h.settingService.SeedGatewayMockPresets(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"status": status, "created": created})
}
