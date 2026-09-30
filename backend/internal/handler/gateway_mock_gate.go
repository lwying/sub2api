package handler

import (
	"context"
	"log/slog"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// SetGatewayMockEventStore 注入最小 mock 事件存储；未注入或为 nil 时不记录事件，
// 但 mock 回复本身照常工作（事件只是排查辅助，不是业务前置条件）。
func (h *GatewayHandler) SetGatewayMockEventStore(store service.GatewayMockEventStore) {
	if h != nil {
		h.gatewayMockEvents = store
	}
}

// SetGatewayMockEventStore 见 GatewayHandler 的同名方法。
func (h *OpenAIGatewayHandler) SetGatewayMockEventStore(store service.GatewayMockEventStore) {
	if h != nil {
		h.gatewayMockEvents = store
	}
}

// maybeServeDownstreamTestMock 在每个入口"已选到可用账号、尚未发出上游"的位置调用。
// 命中管理员配置的下游测试关键词时写出本地 mock 回复并返回 true，调用方随即返回，
// 不发出上游请求、不写使用记录、不构造 wire 尝试。
//
// 该判断与账号级预热拦截相互独立：调用点位于预热判断之后，因此旧预热语义保持优先。
func (h *GatewayHandler) maybeServeDownstreamTestMock(c *gin.Context, protocol service.GatewayMockProtocol, model string, accountID int64, body []byte) bool {
	if h == nil || h.settingService == nil {
		return false
	}
	return serveDownstreamTestMock(c, h.settingService, h.gatewayMockEvents, protocol, model, accountID, body)
}

// maybeServeDownstreamTestMock 见 GatewayHandler 的同名方法。OpenAI 侧未注入设置时按关闭处理。
func (h *OpenAIGatewayHandler) maybeServeDownstreamTestMock(c *gin.Context, protocol service.GatewayMockProtocol, model string, accountID int64, body []byte) bool {
	if h == nil || h.settingService == nil {
		return false
	}
	return serveDownstreamTestMock(c, h.settingService, h.gatewayMockEvents, protocol, model, accountID, body)
}

// serveDownstreamTestMock 是两种处理器共用的判定与响应逻辑。
func serveDownstreamTestMock(
	c *gin.Context,
	settingService *service.SettingService,
	events service.GatewayMockEventStore,
	protocol service.GatewayMockProtocol,
	model string,
	accountID int64,
	body []byte,
) bool {
	if settingService == nil {
		return false
	}
	ruleset := settingService.GatewayMockRuleset(c.Request.Context())
	match := MatchDownstreamTestRequest(body, protocol, ruleset)
	if !match.Matched {
		return false
	}
	recordGatewayMockEvent(c, events, protocol, model, accountID, match)
	sendGatewayMockReply(c, protocol, model, match)
	return true
}

// recordGatewayMockEvent 记录最小命中事件。事件写入失败只写日志，不影响已返回的 mock。
func recordGatewayMockEvent(c *gin.Context, events service.GatewayMockEventStore, protocol service.GatewayMockProtocol, model string, accountID int64, match GatewayMockMatch) {
	if events == nil || c == nil {
		return
	}
	event := service.GatewayMockEventInput{
		RuleID:      match.RuleID,
		RuleVersion: match.RuleVersion,
		Protocol:    string(protocol),
		Model:       model,
		AccountID:   accountID,
		ClientIP:    ip.GetClientIP(c),
	}
	if apiKey, ok := middleware2.GetAPIKeyFromContext(c); ok && apiKey != nil {
		event.APIKeyID = apiKey.ID
		event.UserID = apiKey.UserID
		if apiKey.GroupID != nil {
			event.GroupID = *apiKey.GroupID
		}
	}
	if c.Request != nil {
		event.TraceID = service.RequestTraceIDFromContext(c.Request.Context())
	}
	// 事件写入使用独立上下文：客户端断连不应丢掉已经发生过的命中事实。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 3*time.Second)
	defer cancel()
	if err := events.RecordGatewayMockEvent(ctx, event); err != nil {
		// 只记录失败，不改变已经决定返回的本地 mock：事件是排查辅助而非业务前置条件。
		slog.Warn("gateway.mock_event_record_failed", "rule_id", event.RuleID, "protocol", event.Protocol, "error", err)
	}
}
