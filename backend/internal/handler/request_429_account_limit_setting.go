package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func request429AccountLimit(c *gin.Context, settings *service.SettingService) *Request429AccountLimit {
	limit := 2
	if settings != nil {
		configured, err := settings.GetRateLimit429AccountLimit(c.Request.Context())
		if err == nil {
			limit = configured
		} else {
			requestLogger(c, "handler.gateway.failover").Warn("gateway.failover_429_account_limit_setting_unavailable", zap.Error(err))
		}
	}
	return NewRequest429AccountLimit(limit)
}
