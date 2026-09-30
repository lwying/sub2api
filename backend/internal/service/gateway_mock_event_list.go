package service

import (
	"context"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 下游测试请求 mock 的最小命中事件：管理端只读列表接缝。
//
// 事件只回答"哪条规则在何时因哪个请求命中过"：规则 ID 与版本、协议、模型、当时的
// API Key/用户/分组/账号、客户端 IP、Trace ID 与清理截止。它**不保存**管理员配置的
// 关键词与回复正文，也不保存任何模型正文，因此这里既没有内容检索，也没有正文投影；
// 规则被改写或删除后，事件也不会把已经撤销的配置内容读回来。
//
// 事件本身由网关写入（见 GatewayMockEventRepo.RecordGatewayMockEvent），本文件只定义
// 管理端读取它所需的类型与接缝。

// GatewayMockEventMaxPageSize 是管理端列表一次可取的条数上限。
// 列表是本进程唯一会把事件成批读出的入口，因此上限在这里显式收窄。
const GatewayMockEventMaxPageSize = 100

// ErrGatewayMockEventsUnavailable 表示最小事件列表当前不可读：
// 未接线（没有数据库）或读取本身失败。它不区分两者，因为两者对管理端是同一件事。
var ErrGatewayMockEventsUnavailable = infraerrors.New(503,
	"GATEWAY_MOCK_EVENTS_UNAVAILABLE", "gateway mock events are temporarily unavailable")

// GatewayMockEventListFilter 是列表的唯一筛选条件：分页。
// 关键词与回复正文从未落库，所以不存在"按内容检索命中"这种条件。
type GatewayMockEventListFilter struct {
	Page     int
	PageSize int
}

// GatewayMockEventRecord 是列表投影：字段与落库的最小事实一一对应。
// 这里刻意不含数据库主键：管理端按时间倒序读，行标识不是它需要的事实。
// 这里也不含 cleanup_after：该列是 legacy 内部字段，写入时的估算不是可披露的实际清理
// 时间，因此没有把它读进投影、再转手给管理端的路径。
type GatewayMockEventRecord struct {
	OccurredAt  time.Time
	RuleID      string
	RuleVersion string
	Protocol    string
	Model       string
	APIKeyID    int64
	UserID      int64
	GroupID     int64
	AccountID   int64
	ClientIP    string
	TraceID     string
}

// GatewayMockEventReader 只提供"新到在前"的最小事件分页读取。
type GatewayMockEventReader interface {
	ListGatewayMockEvents(ctx context.Context, filter GatewayMockEventListFilter) ([]GatewayMockEventRecord, int64, error)
}
