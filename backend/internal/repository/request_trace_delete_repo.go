package repository

import (
	"context"
	"database/sql"

	"github.com/lib/pq"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// requestTraceDeleteRepository 只负责手动清理写路径：预览、按明确集合删除、按已执行
// 筛选的有界批次删除。删除在事务内进行：request_trace_stages 通过外键级联删除，
// 同一事务显式删除对应 request_trace_usage_claims。它绝不删除／更新 usage、计费或
// 用户数据——链接的 Trace 允许删除，其使用记录与计费事实保持原样。
type requestTraceDeleteRepository struct {
	q  sqlQueryer
	db *sql.DB
}

func NewRequestTraceDeleteRepository(db *sql.DB) service.RequestTraceDeleteRepository {
	if db == nil {
		return &requestTraceDeleteRepository{}
	}
	return &requestTraceDeleteRepository{q: db, db: db}
}

var _ service.RequestTraceDeleteRepository = (*requestTraceDeleteRepository)(nil)

func requestTraceDeleteFilterListFilter(filter service.RequestTraceExportFilter) service.RequestTraceListFilter {
	listFilter := service.RequestTraceListFilter{
		TraceID:         filter.TraceID,
		RouteFamily:     service.RequestTraceRouteFamily(filter.RouteFamily),
		ClientStatus:    filter.ClientStatus,
		UsageLinked:     filter.UsageLinked,
		UsageLogID:      filter.UsageLogID,
		AccountID:       filter.AccountID,
		GroupID:         filter.GroupID,
		GroupUnknown:    filter.GroupUnknown,
		RequestedModel:  filter.RequestedModel,
		ModelUnknown:    filter.ModelUnknown,
		Platform:        filter.Platform,
		PlatformUnknown: filter.PlatformUnknown,
		UserID:          filter.UserID,
		UserUnknown:     filter.UserUnknown,
		APIKeyID:        filter.APIKeyID,
		APIKeyUnknown:   filter.APIKeyUnknown,
		Keyword:         filter.Keyword,
	}
	if filter.CreatedFrom != nil {
		listFilter.CreatedFrom = *filter.CreatedFrom
	}
	if filter.CreatedTo != nil {
		listFilter.CreatedTo = *filter.CreatedTo
	}
	return listFilter
}

// validateRequestTraceDeleteFilter 复用列表侧的同一封闭校验，避免删除接受一个
// 列表会拒绝的筛选，反之亦然。
func validateRequestTraceDeleteFilter(filter service.RequestTraceExportFilter) error {
	return validateRequestTraceFilter(requestTraceDeleteFilterListFilter(filter))
}

func (r *requestTraceDeleteRepository) PreviewRequestTraceDelete(ctx context.Context, filter service.RequestTraceExportFilter) (int64, int64, error) {
	if r == nil || r.q == nil {
		return 0, 0, service.ErrRequestTraceRepositoryUnavailable
	}
	if err := validateRequestTraceDeleteFilter(filter); err != nil {
		return 0, 0, err
	}
	var matched, snapshotMaxID int64
	const query = `SELECT COUNT(*), COALESCE(MAX(id), 0) FROM request_traces WHERE ` + requestTraceFilterWhere
	if err := scanSingleRow(ctx, r.q, query, requestTraceMetadataFilterArgs(filter), &matched, &snapshotMaxID); err != nil {
		return 0, 0, err
	}
	return matched, snapshotMaxID, nil
}

func (r *requestTraceDeleteRepository) DeleteRequestTracesByIDs(ctx context.Context, traceIDs []string) (int64, error) {
	if r == nil || r.db == nil {
		return 0, service.ErrRequestTraceRepositoryUnavailable
	}
	if len(traceIDs) == 0 || len(traceIDs) > service.RequestTraceDeleteMaxSelectedIDs {
		return 0, service.ErrRequestTraceInvalidRecord
	}
	for _, id := range traceIDs {
		if !traceIDShape.MatchString(id) {
			return 0, service.ErrRequestTraceInvalidRecord
		}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	deleted, deletedIDs, err := requestTraceDeleteByTraceIDsExec(ctx, tx, traceIDs)
	if err != nil {
		return 0, err
	}
	if err := requestTraceDeleteUsageClaimsExec(ctx, tx, deletedIDs); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

func (r *requestTraceDeleteRepository) DeleteRequestTracesByFilter(ctx context.Context, filter service.RequestTraceExportFilter, snapshotMaxID int64, batchSize int) (int64, bool, error) {
	if r == nil || r.db == nil {
		return 0, false, service.ErrRequestTraceRepositoryUnavailable
	}
	if err := validateRequestTraceDeleteFilter(filter); err != nil {
		return 0, false, err
	}
	if snapshotMaxID <= 0 {
		// 该筛选在预览时没有任何匹配行；边界为空就没有可删的记录。
		return 0, true, nil
	}
	if batchSize < 1 || batchSize > 1000 {
		batchSize = service.RequestTraceDeleteBatchSize
	}
	var total int64
	for {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			if total > 0 {
				return total, false, err
			}
			return 0, false, err
		}
		deleted, deletedIDs, err := requestTraceDeleteBatchExec(ctx, tx, filter, snapshotMaxID, batchSize)
		if err != nil {
			_ = tx.Rollback()
			if total > 0 {
				// 已经是部分完成：如实返回真实删除数与 completed=false，让调用方
				// 记下真实进度，而不是把已有删除当成整批失败。
				return total, false, err
			}
			return 0, false, err
		}
		if err := requestTraceDeleteUsageClaimsExec(ctx, tx, deletedIDs); err != nil {
			_ = tx.Rollback()
			if total > 0 {
				return total, false, err
			}
			return 0, false, err
		}
		if err := tx.Commit(); err != nil {
			if total > 0 {
				return total, false, err
			}
			return 0, false, err
		}
		total += deleted
		if deleted < int64(batchSize) {
			return total, true, nil
		}
	}
}

// requestTraceDeleteByTraceIDsExec 按外部 trace_id 删除并返回实际删除的外部 id。
func requestTraceDeleteByTraceIDsExec(ctx context.Context, exec sqlExecutor, traceIDs []string) (int64, []string, error) {
	rows, err := exec.QueryContext(ctx, `DELETE FROM request_traces WHERE trace_id = ANY($1) RETURNING trace_id`, pq.Array(traceIDs))
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = rows.Close() }()
	var deleted int64
	deletedIDs := make([]string, 0, len(traceIDs))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, nil, err
		}
		deleted++
		deletedIDs = append(deletedIDs, id)
	}
	return deleted, deletedIDs, rows.Err()
}

// requestTraceDeleteBatchExec 在一个执行器内删除一批 id <= snapshotMaxID 的匹配行，
// 返回删除数与外部 trace_id。快照边界保证预览之后新入队的记录不被卷入。
//
// 这里刻意**不用** SKIP LOCKED：被并发事务锁住但仍匹配的行若被跳过，本批返回数会
// 小于 batchSize，循环会把"还有没删掉的匹配行"误报成 completed=true。不加 SKIP
// LOCKED 时这样的行会让语句等待而不是被跳过；等到 ctx 超时则以错误结束并如实返回
// 部分完成，绝不谎报全成功。ORDER BY id 让并发清理按同一顺序取锁，避免死锁。
func requestTraceDeleteBatchExec(ctx context.Context, exec sqlExecutor, filter service.RequestTraceExportFilter, snapshotMaxID int64, limit int) (int64, []string, error) {
	args := append(requestTraceMetadataFilterArgs(filter), snapshotMaxID, limit)
	const query = `
		WITH selected AS (
			SELECT id FROM request_traces
			WHERE ` + requestTraceFilterWhere + `
			  AND id <= $20::bigint
			ORDER BY id
			LIMIT $21
			FOR UPDATE
		), deleted AS (
			DELETE FROM request_traces t USING selected s WHERE t.id = s.id
			RETURNING t.trace_id
		)
		SELECT trace_id FROM deleted`
	rows, err := exec.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = rows.Close() }()
	var deleted int64
	deletedIDs := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, nil, err
		}
		deleted++
		deletedIDs = append(deletedIDs, id)
	}
	return deleted, deletedIDs, rows.Err()
}

// requestTraceDeleteUsageClaimsExec 在同一事务里显式删除链路断言。claims 表没有指向
// request_traces 的外键（它只指向 usage_logs），因此删除 Trace 不会自动清理它；
// 这里用外部 trace_id 显式删除，且绝不触碰 usage_logs。
func requestTraceDeleteUsageClaimsExec(ctx context.Context, exec sqlExecutor, traceIDs []string) error {
	if len(traceIDs) == 0 {
		return nil
	}
	_, err := exec.ExecContext(ctx, `DELETE FROM request_trace_usage_claims WHERE trace_id = ANY($1)`, pq.Array(traceIDs))
	return err
}
