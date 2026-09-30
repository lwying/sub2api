package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 下游测试请求 mock 的最小事件存储。
//
// 这里刻意不保存管理员配置的关键词与回复正文，也不保存模型正文：事件仅用于回答
// "哪条规则在何时因哪个请求命中过"，从而在规则被改动或删除后仍能解释历史命中。
type GatewayMockEvent struct {
	ID           int64
	OccurredAt   time.Time
	RuleID       string
	RuleVersion  string
	Protocol     string
	Model        string
	APIKeyID     int64
	UserID       int64
	GroupID      int64
	AccountID    int64
	ClientIP     string
	TraceID      string
	CleanupAfter time.Time
}

// GatewayMockEventRepository 是仓库自身的读写能力集合（含仅供内部与测试使用的插入）。
// 对外的写入接缝是 service.GatewayMockEventStore，对外的读取接缝是 service.GatewayMockEventReader。
type GatewayMockEventRepository interface {
	// insertGatewayMockEvent 写入一条完整事件（含清理期限）。写入失败不得影响网关业务结果。
	insertGatewayMockEvent(ctx context.Context, event GatewayMockEvent) error
	// DeleteMockEventsBefore 有界批量删除已到期的旧事件，返回本轮删除行数。
	DeleteMockEventsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

// GatewayMockEventRepo 是具体仓库类型，供 wire 直接构造。
type GatewayMockEventRepo struct {
	db *sql.DB
}

// 管理端只依赖读取接缝。
var _ service.GatewayMockEventReader = (*GatewayMockEventRepo)(nil)

// 网关侧写入接缝的载荷类型由 service 定义，跨包转换只发生在这里一处。
var _ service.GatewayMockEventStore = (*GatewayMockEventRepo)(nil)

// RecordGatewayMockEvent 实现 service.GatewayMockEventStore。
func (r *GatewayMockEventRepo) RecordGatewayMockEvent(ctx context.Context, input service.GatewayMockEventInput) error {
	return r.insertGatewayMockEvent(ctx, GatewayMockEvent{
		RuleID:      input.RuleID,
		RuleVersion: input.RuleVersion,
		Protocol:    input.Protocol,
		Model:       input.Model,
		APIKeyID:    input.APIKeyID,
		UserID:      input.UserID,
		GroupID:     input.GroupID,
		AccountID:   input.AccountID,
		ClientIP:    input.ClientIP,
		TraceID:     input.TraceID,
		// OccurredAt 留零值，由存储按当前时刻写入（网关不参与时间来源）。
	})
}

// NewGatewayMockEventRepo 创建最小 mock 事件仓库；未提供数据库时返回 nil（该能力不生效）。
func NewGatewayMockEventRepo(db *sql.DB) *GatewayMockEventRepo {
	if db == nil {
		return nil
	}
	return &GatewayMockEventRepo{db: db}
}

const gatewayMockEventInsertSQL = `
INSERT INTO gateway_mock_events (
    rule_id, rule_version, protocol, model,
    api_key_id, user_id, group_id, account_id, client_ip, trace_id, cleanup_after
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

// insertGatewayMockEvent 写入一条事件。
//
// 清理期限直接沿用当前使用记录保留策略算出的截止时刻；策略被停用时由调用方改用兜底期限。
// 这里不读配置，避免网关热路径为了写入事件再去查设置。
func (r *GatewayMockEventRepo) insertGatewayMockEvent(ctx context.Context, event GatewayMockEvent) error {
	cleanupAfter := event.CleanupAfter
	if cleanupAfter.IsZero() {
		cleanupAfter = time.Now().UTC().AddDate(0, 0, gatewayMockEventDefaultRetentionDays)
	}
	_, err := r.db.ExecContext(ctx, gatewayMockEventInsertSQL,
		event.RuleID, event.RuleVersion, event.Protocol, event.Model,
		event.APIKeyID, event.UserID, event.GroupID, event.AccountID, event.ClientIP, event.TraceID,
		cleanupAfter,
	)
	return err
}

// gatewayMockEventDefaultRetentionDays 是调用方未给出截止时刻时的兜底保留天数。
const gatewayMockEventDefaultRetentionDays = 90

// 删除按 (cleanup_after, id) 有界推进：先取一批主键再删除，避免长事务锁住整段扫描。
const gatewayMockEventDeleteSQL = `
DELETE FROM gateway_mock_events
WHERE id IN (
    SELECT id FROM gateway_mock_events
    WHERE cleanup_after <= $1
    ORDER BY cleanup_after ASC, id ASC
    LIMIT $2
)`

// 管理端列表的列投影：与最小事件一一对应，绝不含关键词与回复正文。
const gatewayMockEventListColumns = `occurred_at, rule_id, rule_version, protocol, model,
	api_key_id, user_id, group_id, account_id, client_ip, trace_id, cleanup_after`

// 列表按 (occurred_at, id) 倒序稳定推进，与 gateway_mock_events_occurred_idx 一致：
// 同一时刻的多条事件也有确定的先后，翻页不会重复或漏读。
const gatewayMockEventListSQL = `SELECT ` + gatewayMockEventListColumns + `
FROM gateway_mock_events
ORDER BY occurred_at DESC, id DESC
LIMIT $1 OFFSET $2`

const gatewayMockEventCountSQL = `SELECT COUNT(*) FROM gateway_mock_events`

// ListGatewayMockEvents 返回一页最小事件与总数，新的在前。
//
// 这里没有内容条件：关键词与回复正文从未落库，因此不存在"按内容查命中"，也不会因为
// 规则被改写而把已撤销的配置读回来。错误一律上抛，由调用方决定按不可读处理。
func (r *GatewayMockEventRepo) ListGatewayMockEvents(ctx context.Context, filter service.GatewayMockEventListFilter) ([]service.GatewayMockEventRecord, int64, error) {
	if r == nil || r.db == nil {
		return nil, 0, service.ErrGatewayMockEventsUnavailable
	}
	page, pageSize := normalizeGatewayMockEventPaging(filter.Page, filter.PageSize)
	var total int64
	if err := r.db.QueryRowContext(ctx, gatewayMockEventCountSQL).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx, gatewayMockEventListSQL, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	list := make([]service.GatewayMockEventRecord, 0, pageSize)
	for rows.Next() {
		var record service.GatewayMockEventRecord
		if err := rows.Scan(
			&record.OccurredAt, &record.RuleID, &record.RuleVersion, &record.Protocol, &record.Model,
			&record.APIKeyID, &record.UserID, &record.GroupID, &record.AccountID, &record.ClientIP,
			&record.TraceID, &record.CleanupAfter,
		); err != nil {
			return nil, 0, err
		}
		list = append(list, record)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// normalizeGatewayMockEventPaging 把分页收敛到有界范围：页码从 1 起，
// 页大小落在 [1, GatewayMockEventMaxPageSize]，缺省与其它管理端列表一致。
func normalizeGatewayMockEventPaging(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > service.GatewayMockEventMaxPageSize {
		pageSize = service.GatewayMockEventMaxPageSize
	}
	return page, pageSize
}

// DeleteMockEventsBefore 有界批量删除已到期的旧事件。
func (r *GatewayMockEventRepo) DeleteMockEventsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1
	}
	result, err := r.db.ExecContext(ctx, gatewayMockEventDeleteSQL, cutoff, limit)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return affected, nil
}
