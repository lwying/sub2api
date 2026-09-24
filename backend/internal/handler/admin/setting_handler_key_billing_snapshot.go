package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

type keyBillingSnapshotSettingsRequest struct {
	Enabled       bool `json:"enabled"`
	MaxStaleHours int  `json:"max_stale_hours"`
}

func (h *SettingHandler) GetKeyBillingSnapshotSettings(c *gin.Context) {
	settings, err := h.settingService.GetKeyBillingSnapshotSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.KeyBillingSnapshotSettings{
		Enabled:       settings.Enabled,
		MaxStaleHours: settings.MaxStaleHours,
	})
}

func (h *SettingHandler) UpdateKeyBillingSnapshotSettings(c *gin.Context) {
	var req keyBillingSnapshotSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.settingService.SetKeyBillingSnapshotSettings(c.Request.Context(), req.Enabled, req.MaxStaleHours); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, dto.KeyBillingSnapshotSettings{Enabled: req.Enabled, MaxStaleHours: req.MaxStaleHours})
}
