package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// GetRateLimit429AccountLimit 返回 N 与其同一功能区域内的可选跨请求冷却配置。
// 未配置时跨请求部分为默认值（关闭 / 会话级 / 60 秒），读取不写库。
func (h *SettingHandler) GetRateLimit429AccountLimit(c *gin.Context) {
	ctx := c.Request.Context()
	limit, err := h.settingService.GetRateLimit429AccountLimit(ctx)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	cooldown, err := h.settingService.GetRateLimit429AccountLimitCooldown(ctx)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, newRateLimit429AccountLimitResponse(limit, cooldown))
}

// UpdateRateLimit429AccountLimit 保存 N 与跨请求冷却配置。省略的冷却字段保持现值，
// 因此只提交 max_accounts 的旧客户端不会重置新配置；两者都提供时一次写入。
func (h *SettingHandler) UpdateRateLimit429AccountLimit(c *gin.Context) {
	var req dto.UpdateRateLimit429AccountLimitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	ctx := c.Request.Context()
	cooldown, err := h.settingService.GetRateLimit429AccountLimitCooldown(ctx)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// 只在请求携带冷却字段时合并；否则保持现值且不写该键。
	var cooldownUpdate *service.RateLimit429AccountLimitCooldown
	if req.HasCooldownFields() {
		if req.Enabled != nil {
			cooldown.Enabled = *req.Enabled
		}
		if req.Scope != nil {
			cooldown.Scope = *req.Scope
		}
		if req.CooldownSeconds != nil {
			cooldown.CooldownSeconds = *req.CooldownSeconds
		}
		cooldownUpdate = &cooldown
	}

	if err := h.settingService.SetRateLimit429AccountLimitWithCooldown(ctx, req.MaxAccounts, cooldownUpdate); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, newRateLimit429AccountLimitResponse(req.MaxAccounts, cooldown))
}

func newRateLimit429AccountLimitResponse(limit int, cooldown service.RateLimit429AccountLimitCooldown) dto.RateLimit429AccountLimit {
	return dto.RateLimit429AccountLimit{
		MaxAccounts:     limit,
		Enabled:         cooldown.Enabled,
		Scope:           cooldown.Scope,
		CooldownSeconds: cooldown.CooldownSeconds,
	}
}
