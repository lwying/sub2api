package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"

	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) GetRateLimit429AccountLimit(c *gin.Context) {
	limit, err := h.settingService.GetRateLimit429AccountLimit(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.RateLimit429AccountLimit{MaxAccounts: limit})
}

func (h *SettingHandler) UpdateRateLimit429AccountLimit(c *gin.Context) {
	var req dto.RateLimit429AccountLimit
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.settingService.SetRateLimit429AccountLimit(c.Request.Context(), req.MaxAccounts); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, req)
}
