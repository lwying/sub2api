package service

import "context"

// 下游测试请求 mock 的最小事件：接口与输入定义在 service 层，具体仓库与 wire 装配
// 由 wire.go 负责，因此 handler 与 service 都不必依赖 repository 包
// （见 .golangci.yml 的 depguard 规则）。
//
// 事件刻意不含管理员配置的关键词与回复正文，也不含任何模型正文。

// 内容审计状态是一个有界闭集，描述"这条本地 mock 命中时，提示词／内容审计有没有执行"：
//   - GatewayMockContentAuditSkippedLocalMock：新的早期严格命中：本地直接应答，
//     两类内容审计都没有执行（命中不选号、不发模型请求）。
//   - GatewayMockContentAuditUnknown：旧记录，或调用方没有给出状态。历史数据不追溯猜测，
//     因此不会被回填成 skipped_local_mock。
//
// 新增取值时两端（常量与迁移内的 CHECK 约束）必须同步，前端对未知取值安全降级为 unknown。
const (
	GatewayMockContentAuditUnknown          = "unknown"
	GatewayMockContentAuditSkippedLocalMock = "skipped_local_mock"
)

// gatewayMockContentAuditStates 是闭集查找表；NormalizeGatewayMockContentAuditState 用它把
// 闭集之外的取值（含空串、未来版本新增的枚举）收敛成 unknown。
var gatewayMockContentAuditStates = map[string]struct{}{
	GatewayMockContentAuditUnknown:          {},
	GatewayMockContentAuditSkippedLocalMock: {},
}

// NormalizeGatewayMockContentAuditState 把内容审计状态收敛到有界闭集。闭集之外一律返回
// unknown：既不猜测旧记录的语义，也不把未知枚举回显给管理端。
func NormalizeGatewayMockContentAuditState(state string) string {
	if _, ok := gatewayMockContentAuditStates[state]; ok {
		return state
	}
	return GatewayMockContentAuditUnknown
}

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
	// ContentAuditState 是这次命中时的内容审计状态，取上面闭集中的值。
	// 空串表示调用方未给出（旧调用方），写入时按 unknown 处理，绝不伪造成 skipped_local_mock。
	ContentAuditState string
}

// GatewayMockEventStore 持久化最小 mock 事件。写入失败只记录日志，绝不改变网关业务结果。
type GatewayMockEventStore interface {
	RecordGatewayMockEvent(ctx context.Context, input GatewayMockEventInput) error
}
