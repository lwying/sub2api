package service

import "context"

// 下游测试请求 mock 的最小事件：接口与输入定义在 service 层，具体仓库与 wire 装配
// 由 wire.go 负责，因此 handler 与 service 都不必依赖 repository 包
// （见 .golangci.yml 的 depguard 规则）。
//
// 事件刻意不含管理员配置的关键词与回复正文，也不含任何模型正文。

// GatewayMockEventInput 描述一条待写入事件。
type GatewayMockEventInput struct {
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

// GatewayMockEventStore 持久化最小 mock 事件。写入失败只记录日志，绝不改变网关业务结果。
type GatewayMockEventStore interface {
	RecordGatewayMockEvent(ctx context.Context, input GatewayMockEventInput) error
}
