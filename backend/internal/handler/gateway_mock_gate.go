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

// resolveDownstreamTestMockMatch 只做判定：读一次设置快照并匹配，不写响应、
// 不产生任何副作用。需要"先判定、命中后才接管"的调用点（例如 compact 的下游
// 心跳必须在命中后才停拍）用它预检，再把结论交给 serveResolvedDownstreamTestMock，
// 从而保证整条路径只读一次规则快照，判定结论与写回内容不会出现分歧。
//
// 设置未注入时按关闭处理，与 GatewayMockRuleset 的空快照一致（fail-closed）。
func (h *OpenAIGatewayHandler) resolveDownstreamTestMockMatch(c *gin.Context, protocol service.GatewayMockProtocol, body []byte) GatewayMockMatch {
	if h == nil || h.settingService == nil {
		return GatewayMockMatch{}
	}
	return MatchDownstreamTestRequest(body, protocol, h.settingService.GatewayMockRuleset(c.Request.Context()))
}

// serveResolvedDownstreamTestMock 用预检得到的匹配结论写回本地 mock，不再读设置。
func (h *OpenAIGatewayHandler) serveResolvedDownstreamTestMock(c *gin.Context, protocol service.GatewayMockProtocol, model string, accountID int64, match GatewayMockMatch) bool {
	if h == nil {
		return false
	}
	return serveMatchedDownstreamTestMock(c, h.gatewayMockEvents, protocol, model, accountID, match)
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
	return serveMatchedDownstreamTestMock(c, events, protocol, model, accountID,
		MatchDownstreamTestRequest(body, protocol, ruleset))
}

// serveMatchedDownstreamTestMock 按已得到的匹配结论命中原子的三件事：记录命中
// 事件、记录本地 mock 的网关决策、写回固定回复。match 未命中时什么也不做。
// 结论必须来自本请求、且只能读一次规则快照——见 resolveDownstreamTestMockMatch。
func serveMatchedDownstreamTestMock(
	c *gin.Context,
	events service.GatewayMockEventStore,
	protocol service.GatewayMockProtocol,
	model string,
	accountID int64,
	match GatewayMockMatch,
) bool {
	if !match.Matched {
		return false
	}
	recordGatewayMockEvent(c, events, protocol, model, accountID, match)
	recordLocalMockTraceDecision(c)
	sendGatewayMockReply(c, protocol, model, match)
	return true
}

// recordLocalMockTraceDecision 在请求 Trace 已开启且命中范围时，把这次"用本地
// mock 应答、不发上游"记成网关自己的决策事实：kind=mock、outcome=not_sent、
// source=inbound。
//
// 它描述的是网关的决定，不是一次上游尝试，因此只写 body-less 的决策阶段；调用点
// 在每个入口"已选到账号、尚未发出上游"的位置，命中后调用方立即返回，不会产生
// wire 尝试。Trace 未开启或不在采集范围时，RecordRequestTraceDecision 自会 no-op。
func recordLocalMockTraceDecision(c *gin.Context) {
	RecordRequestTraceDecision(c, 0, "mock_served", service.RequestTraceDecisionFacts{
		Decision: service.RequestTraceDecisionMock,
		Outcome:  service.RequestTraceDecisionNotSent,
		Source:   service.RequestTraceDecisionSourceInbound,
	})
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
