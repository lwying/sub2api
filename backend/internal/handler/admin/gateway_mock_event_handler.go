package admin

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 下游测试请求 mock 的最小命中事件：管理端只读列表。
//
// 列表只回答"哪条规则在何时因哪个请求命中过"，答案是一组可观察元数据与总数，
// 不含管理员配置的关键词与回复正文（它们根本没有落库），也不含任何模型正文。
// 因此它可以用普通管理员凭据读取，不要求管理员登录会话——与 request-trace 的运维
// 状态端点同一先例（见 request_trace_status_handler.go）；改动开关与规则仍然只允许
// 登录会话（见 setting_handler_gateway_mock.go）。

// gatewayMockEventReader 是本处理器需要的最小能力：一页新到在前的事件与总数。
type gatewayMockEventReader interface {
	ListGatewayMockEvents(ctx context.Context, filter service.GatewayMockEventListFilter) ([]service.GatewayMockEventRecord, int64, error)
}

// GatewayMockEventHandler 暴露最小命中事件的只读列表。
type GatewayMockEventHandler struct {
	reader gatewayMockEventReader
}

func NewGatewayMockEventHandler(reader gatewayMockEventReader) *GatewayMockEventHandler {
	return &GatewayMockEventHandler{reader: reader}
}

// gatewayMockEventView 是列表投影：字段与落库的最小事实一一对应。
// 关键词、回复正文与数据库主键都不在这里，因此也不会出现在响应里。
type gatewayMockEventView struct {
	OccurredAt  time.Time `json:"occurred_at"`
	RuleID      string    `json:"rule_id"`
	RuleVersion string    `json:"rule_version"`
	Protocol    string    `json:"protocol"`
	Model       string    `json:"model"`
	APIKeyID    int64     `json:"api_key_id"`
	UserID      int64     `json:"user_id"`
	GroupID     int64     `json:"group_id"`
	AccountID   int64     `json:"account_id"`
	ClientIP    string    `json:"client_ip"`
	TraceID     string    `json:"trace_id"`
	// CleanupAfter 恒为 null：落库的 cleanup_after 只是 legacy 内部字段（写入时的估算），
	// 不是可披露的实际清理时间。键保留下来是为了让管理端能区分"没有披露期限"与
	// "服务端漏了这个字段"；实际清理按 occurred_at 与当次保留策略决定。
	CleanupAfter *time.Time `json:"cleanup_after"`
}

func gatewayMockEventViewOf(record service.GatewayMockEventRecord) gatewayMockEventView {
	// CleanupAfter 不在这里赋值：落库的期限是内部估算，任何对外投影都不披露它。
	return gatewayMockEventView{
		OccurredAt:  record.OccurredAt,
		RuleID:      record.RuleID,
		RuleVersion: record.RuleVersion,
		Protocol:    record.Protocol,
		Model:       record.Model,
		APIKeyID:    record.APIKeyID,
		UserID:      record.UserID,
		GroupID:     record.GroupID,
		AccountID:   record.AccountID,
		ClientIP:    record.ClientIP,
		TraceID:     record.TraceID,
	}
}

// List 返回最小命中事件的一页，新的在前。
// GET /api/v1/admin/settings/gateway-mock/events
//
// 页大小在这里就收窄到有界上限，仓储再收敛一次：一条请求不该能把整表拉出来。
func (h *GatewayMockEventHandler) List(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if h == nil || h.reader == nil {
		// 未接线与读取失败对管理端是同一件事：先看到"不可读"，而不是一片空列表。
		response.ErrorFrom(c, service.ErrGatewayMockEventsUnavailable)
		return
	}
	page, pageSize := response.ParsePagination(c)
	if pageSize > service.GatewayMockEventMaxPageSize {
		pageSize = service.GatewayMockEventMaxPageSize
	}
	records, total, err := h.reader.ListGatewayMockEvents(c.Request.Context(), service.GatewayMockEventListFilter{
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		// 读取失败不把原始报文带回管理端：返回的是有界原因，不是数据库错误文本。
		response.ErrorFrom(c, service.ErrGatewayMockEventsUnavailable)
		return
	}
	views := make([]gatewayMockEventView, 0, len(records))
	for _, record := range records {
		views = append(views, gatewayMockEventViewOf(record))
	}
	response.Paginated(c, views, total, page, pageSize)
}
