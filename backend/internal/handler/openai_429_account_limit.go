package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func (h *OpenAIGatewayHandler) request429AccountLimit(c *gin.Context) *Request429AccountLimit {
	if h == nil || h.gatewayService == nil {
		return NewRequest429AccountLimit(2)
	}
	return NewRequest429AccountLimit(h.gatewayService.GetRateLimit429AccountLimit(c.Request.Context()))
}

func (h *OpenAIGatewayHandler) stopAfter429Accounts(c *gin.Context, budget *Request429AccountLimit, accountID int64, failoverErr *service.UpstreamFailoverError) bool {
	if !budget.Record(accountID, failoverErr) {
		return false
	}
	requestLogger(c, "handler.openai.failover").Warn("openai.failover_429_account_limit_reached",
		zap.Int("failed_429_accounts", budget.Count()))
	return true
}
