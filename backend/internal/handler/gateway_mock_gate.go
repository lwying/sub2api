package handler

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
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

// gatewayMockGateResult 是早期 Mock 闸门的三态结论。未命中必须由调用方原样继续
// 后续审计／计费／选号／转发，另两态表示响应已经写出、调用方立即返回。
type gatewayMockGateResult int

const (
	// gatewayMockGateMiss：未命中，无任何副作用，调用方继续原有顺序。
	gatewayMockGateMiss gatewayMockGateResult = iota
	// gatewayMockGateServed：已写出本地 mock 回复。
	gatewayMockGateServed
	// gatewayMockGateRejected：已按权限／冷却／并发限制写出标准拒绝响应。
	gatewayMockGateRejected
)

// earlyMockServeRequest 汇集入口在"必要鉴权与格式校验之后、内容审计之前"提前判定
// 并写回本地 mock 所需的一切。只有命中分支才会触碰冷却与用户并发准入；未命中时
// 不读取冷却、不取槽、不记录任何事实，原样继续。
type earlyMockServeRequest struct {
	protocol        service.GatewayMockProtocol
	model           string
	rawBody         []byte
	stream          bool
	settingService  *service.SettingService
	events          service.GatewayMockEventStore
	concurrency     *ConcurrencyHelper
	apiKeyID        int64
	userID          int64
	userConcurrency int
	cooldown        claude429CooldownRequest
	// metadataReject 在命中后执行强制请求审计元数据准备（非内容扫描）；返回 true
	// 表示已按既有政策写出拒绝响应。nil 表示该入口已在更早位置执行 prepare。
	metadataReject func() bool
	// precheckReject 在命中后、写回前执行权限／封禁／格式预检；返回 true 表示已写出拒绝响应。
	precheckReject func() bool
	// writeCooldownReject 按本入口协议写出标准冷却 429。
	writeCooldownReject func(seconds int)
	// writeConcurrencyReject 按本入口既有语义写出并发取槽失败响应。
	writeConcurrencyReject func(err error)
	// billingCheck 复用既有 CheckBillingEligibility 做计费/配额/RPM 准入（不扣费、
	// 不产生用量、不选号）。每次请求最多调用一次：命中分支调用后立即返回，
	// 未命中不调用，因此不会重复计数。nil 视为依赖缺失，fail-closed。
	billingCheck func() error
	// writeBillingReject 按本入口协议与 billingErrorDetails 写出 429/403/503。
	writeBillingReject func(err error)
}

// errEarlyMockAdmissionUnavailable 表示命中分支所需的准入依赖缺失：必须 fail-closed，
// 既不放行本地回复，也不回退到内容审计。
var errEarlyMockAdmissionUnavailable = errors.New("downstream test mock admission dependency unavailable")

// serveEarlyDownstreamTestMock 是各 HTTP 入口共用的早期严格 Mock 闸门：
// 身份认证、权限与必要请求格式校验通过后、内容审计之前调用。严格命中则本地应答，
// 不执行两类内容审计、不选号、不发模型请求、不产生用量；未命中无副作用返回 Miss，
// 调用方继续原有顺序（审计 -> 计费 -> 选号 -> 转发）。
func serveEarlyDownstreamTestMock(c *gin.Context, req earlyMockServeRequest) gatewayMockGateResult {
	if req.settingService == nil || c == nil {
		return gatewayMockGateMiss
	}
	// 每请求只读一次规则快照：判定与写回共用同一结论，未命中之后不得再读新规则。
	match := MatchDownstreamTestRequest(req.rawBody, req.protocol, req.settingService.GatewayMockRuleset(c.Request.Context()))
	if !match.Matched {
		return gatewayMockGateMiss
	}
	// 强制请求审计元数据政策对命中请求同样生效：这不是内容扫描，不得因本地
	// Mock 而取消；未配置强制审计时是 no-op。
	if req.metadataReject != nil && req.metadataReject() {
		return gatewayMockGateRejected
	}
	// 权限／封禁／格式预检只在命中分支执行，未命中保持原有顺序不受影响。
	if req.precheckReject != nil && req.precheckReject() {
		return gatewayMockGateRejected
	}
	// 冷却：与正常入口一致地返回标准 429。
	if seconds, blocked := req.cooldown.retryAfter(c.Request.Context()); blocked {
		req.cooldown.prepareLocalResponse(c, seconds)
		req.writeCooldownReject(seconds)
		return gatewayMockGateRejected
	}
	// 并发依赖缺失时 fail-closed：不静默绕过并发约束，也不回退内容审计，
	// 直接按服务不可用拒绝。
	if req.concurrency == nil {
		if req.writeConcurrencyReject != nil {
			req.writeConcurrencyReject(errEarlyMockAdmissionUnavailable)
		}
		return gatewayMockGateRejected
	}
	// 用户并发准入：非等待取槽，取不到立即标准 429，绝不发送等待心跳
	// （避免在本地回复前提前写出 SSE ping）。
	release, acquired, err := req.concurrency.TryAcquireUserSlotForAPIKey(c.Request.Context(), req.userID, req.userConcurrency, req.apiKeyID)
	if err != nil {
		req.writeConcurrencyReject(err)
		return gatewayMockGateRejected
	}
	if !acquired {
		req.writeConcurrencyReject(&WaitQueueFullError{SlotType: "user"})
		return gatewayMockGateRejected
	}
	// 与正常路径一致：请求结束或客户端断连都归还槽位。
	release = wrapReleaseOnDone(c.Request.Context(), release)
	if release != nil {
		defer release()
	}
	// 基础计费/配额/RPM 准入：跳过内容审计不等于跳过限流配额。命中分支只调用
	// 一次既有 CheckBillingEligibility（不扣费、不 inflight reserve、不选号）。
	if req.billingCheck == nil {
		if req.writeConcurrencyReject != nil {
			req.writeConcurrencyReject(errEarlyMockAdmissionUnavailable)
		}
		return gatewayMockGateRejected
	}
	if err := req.billingCheck(); err != nil {
		if req.writeBillingReject != nil {
			req.writeBillingReject(err)
		}
		return gatewayMockGateRejected
	}
	markGatewayMockStream(c, req.stream)
	recordGatewayMockEvent(c, req.events, req.protocol, req.model, 0, match)
	recordLocalMockTraceDecision(c)
	sendGatewayMockReply(c, req.protocol, req.model, match)
	return gatewayMockGateServed
}

// recordLocalMockTraceDecision 在请求 Trace 已开启且命中范围时，把这次"用本地
// mock 应答、不发上游"记成网关自己的决策事实：kind=mock、outcome=not_sent、
// source=inbound。
//
// reason 使用 mock_served_without_content_audit：早期严格命中在两类内容审计之前
// 本地应答，因此这条决策明确表示内容审计没有执行。它不设置 securityAuditCompleted，
// 也不生成安全审计 Allow／Pass 事件。
//
// 它描述的是网关的决定，不是一次上游尝试，因此只写 body-less 的决策阶段；命中后
// 调用方立即返回，不会产生 wire 尝试。Trace 未开启或不在采集范围时自会 no-op。
func recordLocalMockTraceDecision(c *gin.Context) {
	RecordRequestTraceDecision(c, 0, "mock_served_without_content_audit", service.RequestTraceDecisionFacts{
		Decision: service.RequestTraceDecisionMock,
		Outcome:  service.RequestTraceDecisionNotSent,
		Source:   service.RequestTraceDecisionSourceInbound,
	})
}

// gatewayMockBillingReject 把 CheckBillingEligibility 的错误按既有语义写成
// 429/403/503：复用 billingErrorDetails，并保留 Retry-After。
func gatewayMockBillingReject(c *gin.Context, err error, write func(status int, code, message string)) {
	status, code, message, retryAfter := billingErrorDetails(err)
	if retryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(retryAfter))
	}
	write(status, code, message)
}

// mockBillingCheck 构造命中分支的基础准入回调：复用既有 CheckBillingEligibility
// 执行余额/订阅/平台配额/API Key 窗口/RPM 检查，不扣费、不 inflight reserve、不选号。
func (h *GatewayHandler) mockBillingCheck(c *gin.Context, apiKey *service.APIKey) func() error {
	return func() error {
		if h.billingCacheService == nil {
			return service.ErrBillingServiceUnavailable
		}
		subscription, _ := middleware2.GetSubscriptionFromContext(c)
		return h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey))
	}
}

// mockBillingCheck 见 GatewayHandler 的同名方法。
func (h *OpenAIGatewayHandler) mockBillingCheck(c *gin.Context, apiKey *service.APIKey) func() error {
	return func() error {
		if h.billingCacheService == nil {
			return service.ErrBillingServiceUnavailable
		}
		subscription, _ := middleware2.GetSubscriptionFromContext(c)
		return h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey))
	}
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
		// 本地 mock 在两类内容审计之前直接应答：显式记录审计未执行，
		// 不追溯猜测历史记录（旧记录在写入时保持 unknown）。
		ContentAuditState: service.GatewayMockContentAuditSkippedLocalMock,
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
