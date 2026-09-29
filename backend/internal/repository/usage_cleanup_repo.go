package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbusagecleanuptask "github.com/Wei-Shaw/sub2api/ent/usagecleanuptask"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type usageCleanupRepository struct {
	client *dbent.Client
	sql    sqlExecutor
}

func NewUsageCleanupRepository(client *dbent.Client, sqlDB *sql.DB) service.UsageCleanupRepository {
	return newUsageCleanupRepositoryWithSQL(client, sqlDB)
}

func newUsageCleanupRepositoryWithSQL(client *dbent.Client, sqlq sqlExecutor) *usageCleanupRepository {
	return &usageCleanupRepository{client: client, sql: sqlq}
}

func (r *usageCleanupRepository) CreateTask(ctx context.Context, task *service.UsageCleanupTask) error {
	if task == nil {
		return nil
	}
	if r.client != nil {
		return r.createTaskWithEnt(ctx, task)
	}
	return r.createTaskWithSQL(ctx, task)
}

func (r *usageCleanupRepository) ListTasks(ctx context.Context, params pagination.PaginationParams) ([]service.UsageCleanupTask, *pagination.PaginationResult, error) {
	if r.client != nil {
		return r.listTasksWithEnt(ctx, params)
	}
	var total int64
	if err := scanSingleRow(ctx, r.sql, "SELECT COUNT(*) FROM usage_cleanup_tasks", nil, &total); err != nil {
		return nil, nil, err
	}
	if total == 0 {
		return []service.UsageCleanupTask{}, paginationResultFromTotal(0, params), nil
	}

	query := `
		SELECT id, status, filters, created_by, deleted_rows, error_message,
			canceled_by, canceled_at,
			started_at, finished_at, created_at, updated_at
		FROM usage_cleanup_tasks
		ORDER BY created_at DESC, id DESC
		LIMIT $1 OFFSET $2
	`
	rows, err := r.sql.QueryContext(ctx, query, params.Limit(), params.Offset())
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	tasks := make([]service.UsageCleanupTask, 0)
	for rows.Next() {
		var task service.UsageCleanupTask
		var filtersJSON []byte
		var errMsg sql.NullString
		var canceledBy sql.NullInt64
		var canceledAt sql.NullTime
		var startedAt sql.NullTime
		var finishedAt sql.NullTime
		if err := rows.Scan(
			&task.ID,
			&task.Status,
			&filtersJSON,
			&task.CreatedBy,
			&task.DeletedRows,
			&errMsg,
			&canceledBy,
			&canceledAt,
			&startedAt,
			&finishedAt,
			&task.CreatedAt,
			&task.UpdatedAt,
		); err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(filtersJSON, &task.Filters); err != nil {
			return nil, nil, fmt.Errorf("parse cleanup filters: %w", err)
		}
		if errMsg.Valid {
			task.ErrorMsg = &errMsg.String
		}
		if canceledBy.Valid {
			v := canceledBy.Int64
			task.CanceledBy = &v
		}
		if canceledAt.Valid {
			task.CanceledAt = &canceledAt.Time
		}
		if startedAt.Valid {
			task.StartedAt = &startedAt.Time
		}
		if finishedAt.Valid {
			task.FinishedAt = &finishedAt.Time
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return tasks, paginationResultFromTotal(total, params), nil
}

func (r *usageCleanupRepository) ClaimNextPendingTask(ctx context.Context, staleRunningAfterSeconds int64) (*service.UsageCleanupTask, error) {
	if staleRunningAfterSeconds <= 0 {
		staleRunningAfterSeconds = 1800
	}
	query := `
		WITH next AS (
			SELECT id
			FROM usage_cleanup_tasks
			WHERE status = $1
				OR (
					status = $2
					AND started_at IS NOT NULL
					AND started_at < NOW() - ($3 * interval '1 second')
				)
			ORDER BY created_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE usage_cleanup_tasks AS tasks
		SET status = $4,
			started_at = NOW(),
			finished_at = NULL,
			error_message = NULL,
			updated_at = NOW()
		FROM next
		WHERE tasks.id = next.id
		RETURNING tasks.id, tasks.status, tasks.filters, tasks.created_by, tasks.deleted_rows, tasks.error_message,
			tasks.started_at, tasks.finished_at, tasks.created_at, tasks.updated_at
	`
	var task service.UsageCleanupTask
	var filtersJSON []byte
	var errMsg sql.NullString
	var startedAt sql.NullTime
	var finishedAt sql.NullTime
	if err := scanSingleRow(
		ctx,
		r.sql,
		query,
		[]any{
			service.UsageCleanupStatusPending,
			service.UsageCleanupStatusRunning,
			staleRunningAfterSeconds,
			service.UsageCleanupStatusRunning,
		},
		&task.ID,
		&task.Status,
		&filtersJSON,
		&task.CreatedBy,
		&task.DeletedRows,
		&errMsg,
		&startedAt,
		&finishedAt,
		&task.CreatedAt,
		&task.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(filtersJSON, &task.Filters); err != nil {
		return nil, fmt.Errorf("parse cleanup filters: %w", err)
	}
	if errMsg.Valid {
		task.ErrorMsg = &errMsg.String
	}
	if startedAt.Valid {
		task.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		task.FinishedAt = &finishedAt.Time
	}
	return &task, nil
}

func (r *usageCleanupRepository) GetTaskStatus(ctx context.Context, taskID int64) (string, error) {
	if r.client != nil {
		return r.getTaskStatusWithEnt(ctx, taskID)
	}
	var status string
	if err := scanSingleRow(ctx, r.sql, "SELECT status FROM usage_cleanup_tasks WHERE id = $1", []any{taskID}, &status); err != nil {
		return "", err
	}
	return status, nil
}

func (r *usageCleanupRepository) UpdateTaskProgress(ctx context.Context, taskID int64, deletedRows int64) error {
	if r.client != nil {
		return r.updateTaskProgressWithEnt(ctx, taskID, deletedRows)
	}
	query := `
		UPDATE usage_cleanup_tasks
		SET deleted_rows = $1,
			updated_at = NOW()
		WHERE id = $2
	`
	_, err := r.sql.ExecContext(ctx, query, deletedRows, taskID)
	return err
}

func (r *usageCleanupRepository) CancelTask(ctx context.Context, taskID int64, canceledBy int64) (bool, error) {
	if r.client != nil {
		return r.cancelTaskWithEnt(ctx, taskID, canceledBy)
	}
	query := `
		UPDATE usage_cleanup_tasks
		SET status = $1,
			canceled_by = $3,
			canceled_at = NOW(),
			finished_at = NOW(),
			error_message = NULL,
			updated_at = NOW()
		WHERE id = $2
			AND status IN ($4, $5)
		RETURNING id
	`
	var id int64
	err := scanSingleRow(ctx, r.sql, query, []any{
		service.UsageCleanupStatusCanceled,
		taskID,
		canceledBy,
		service.UsageCleanupStatusPending,
		service.UsageCleanupStatusRunning,
	}, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *usageCleanupRepository) MarkTaskSucceeded(ctx context.Context, taskID int64, deletedRows int64) error {
	if r.client != nil {
		return r.markTaskSucceededWithEnt(ctx, taskID, deletedRows)
	}
	query := `
		UPDATE usage_cleanup_tasks
		SET status = $1,
			deleted_rows = $2,
			finished_at = NOW(),
			updated_at = NOW()
		WHERE id = $3
	`
	_, err := r.sql.ExecContext(ctx, query, service.UsageCleanupStatusSucceeded, deletedRows, taskID)
	return err
}

func (r *usageCleanupRepository) MarkTaskFailed(ctx context.Context, taskID int64, deletedRows int64, errorMsg string) error {
	if r.client != nil {
		return r.markTaskFailedWithEnt(ctx, taskID, deletedRows, errorMsg)
	}
	query := `
		UPDATE usage_cleanup_tasks
		SET status = $1,
			deleted_rows = $2,
			error_message = $3,
			finished_at = NOW(),
			updated_at = NOW()
		WHERE id = $4
	`
	_, err := r.sql.ExecContext(ctx, query, service.UsageCleanupStatusFailed, deletedRows, errorMsg, taskID)
	return err
}

// errPlaintextOwnershipUnpinned 表示在没有自建事务可用的删除路径上，执行器留不住所有权
// 核对用的 ACCESS SHARE 锁，因而无法保证「核对时所有权外键在」这一结论在 DELETE 之前不被
// 并发的 ALTER TABLE ... DROP CONSTRAINT 改写。旁路表已部署时这种组合一律失败关闭、保留
// 数据。dashboard 的非事务分支用同样的规则（其哨兵为 errPlaintextOwnershipLockUnpinned）。
var errPlaintextOwnershipUnpinned = errors.New(
	"refusing usage deletion: plaintext sidecar tables are deployed but this executor cannot hold the ownership verification lock before DELETE")

// isPlaintextOwnershipPinnableExecutor 只认可调用方明确持有的事务（*sql.Tx / *dbent.Tx）：
// 只有它们能让 ACCESS SHARE 锁留到后续语句。自动提交的适配器取锁即释放，因此不算可钉住。
func isPlaintextOwnershipPinnableExecutor(q sqlExecutor) bool {
	switch q.(type) {
	case *sql.Tx, *dbent.Tx:
		return true
	default:
		return false
	}
}

// ensurePlaintextOwnershipForUnmanagedExecutor 必须在「没有自建事务」的删除路径执行任何
// usage 行 DELETE 之前调用：调用方事务上取 ACCESS SHARE 锁把「所有权外键仍在」钉到该事务
// 结束，使并发的 DROP CONSTRAINT 不能在核对与 DELETE 之间生效；无法持锁的适配器只在
// 所有明文旁路表都未部署时保持原有行为放行，部署了却钉不住一律拒绝。
func ensurePlaintextOwnershipForUnmanagedExecutor(ctx context.Context, q sqlExecutor) error {
	if isPlaintextOwnershipPinnableExecutor(q) {
		return ensureUsageCleanupPreservesPlaintextOwnership(ctx, q, true)
	}
	ownership, err := probePlaintextSidecarOwnership(ctx, q)
	if err != nil {
		return err
	}
	if err := ownership.verificationError(); err != nil {
		return err
	}
	if len(ownership.deployedSidecars()) > 0 {
		return errPlaintextOwnershipUnpinned
	}
	return nil
}

func (r *usageCleanupRepository) DeleteUsageLogsBatch(ctx context.Context, filters service.UsageCleanupFilters, limit int) (int64, error) {
	if filters.StartTime.IsZero() || filters.EndTime.IsZero() {
		return 0, fmt.Errorf("cleanup filters missing time range")
	}
	whereClause, args := buildUsageCleanupWhere(filters)
	if whereClause == "" {
		return 0, fmt.Errorf("cleanup filters missing time range")
	}
	args = append(args, limit)
	if db, ok := r.sql.(*sql.DB); ok {
		return r.deleteUsageLogsBatchWithRollupInvalidation(ctx, db, whereClause, args)
	}
	// 该分支没有自建事务，必须先确认能钉住所有权结论再删除，否则失败关闭、保留数据。
	if err := ensurePlaintextOwnershipForUnmanagedExecutor(ctx, r.sql); err != nil {
		return 0, err
	}
	query := fmt.Sprintf(`
		WITH target AS (
			SELECT id
			FROM usage_logs
			WHERE %s
			ORDER BY created_at ASC, id ASC
			LIMIT $%d
		)
		DELETE FROM usage_logs
		WHERE id IN (SELECT id FROM target)
		RETURNING created_at
	`, whereClause, len(args))

	rows, err := r.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	var deleted int64
	for rows.Next() {
		deleted++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return deleted, nil
}

func (r *usageCleanupRepository) deleteUsageLogsBatchWithRollupInvalidation(ctx context.Context, db *sql.DB, whereClause string, args []any) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	rollback := func(err error) (int64, error) {
		_ = tx.Rollback()
		return 0, err
	}

	if err := lockGroupUsageRollupState(ctx, tx); err != nil {
		return rollback(err)
	}
	// 逐行 DELETE 依赖所有权外键把随 usage 明文一起带走；手工去掉外键的分区配置下同一条
	// DELETE 会把值明细/已关联明文诊断留成孤儿，所以先在同一事务里核对所有权并钉住结论，
	// 缺失即整批拒绝、保留数据。
	if err := ensureUsageCleanupPreservesPlaintextOwnership(ctx, tx, true); err != nil {
		return rollback(err)
	}
	query := fmt.Sprintf(`
		WITH target AS (
			SELECT id
			FROM usage_logs
			WHERE %s
			ORDER BY created_at ASC, id ASC
			LIMIT $%d
		)
		DELETE FROM usage_logs
		WHERE id IN (SELECT id FROM target)
		RETURNING created_at
	`, whereClause, len(args))
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return rollback(err)
	}

	var deleted int64
	var earliestDeletedAt time.Time
	for rows.Next() {
		var deletedAt time.Time
		if err := rows.Scan(&deletedAt); err != nil {
			_ = rows.Close()
			return rollback(err)
		}
		deleted++
		if earliestDeletedAt.IsZero() || deletedAt.Before(earliestDeletedAt) {
			earliestDeletedAt = deletedAt
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return rollback(err)
	}
	if err := rows.Close(); err != nil {
		return rollback(err)
	}

	if deleted > 0 {
		if err := invalidateGroupUsageRollupsAt(ctx, tx, earliestDeletedAt); err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

func buildUsageCleanupWhere(filters service.UsageCleanupFilters) (string, []any) {
	conditions := make([]string, 0, 8)
	args := make([]any, 0, 8)
	idx := 1
	if !filters.StartTime.IsZero() {
		conditions = append(conditions, fmt.Sprintf("created_at >= $%d", idx))
		args = append(args, filters.StartTime)
		idx++
	}
	if !filters.EndTime.IsZero() {
		conditions = append(conditions, fmt.Sprintf("created_at <= $%d", idx))
		args = append(args, filters.EndTime)
		idx++
	}
	if filters.UserID != nil {
		conditions = append(conditions, fmt.Sprintf("user_id = $%d", idx))
		args = append(args, *filters.UserID)
		idx++
	}
	if filters.APIKeyID != nil {
		conditions = append(conditions, fmt.Sprintf("api_key_id = $%d", idx))
		args = append(args, *filters.APIKeyID)
		idx++
	}
	if filters.AccountID != nil {
		conditions = append(conditions, fmt.Sprintf("account_id = $%d", idx))
		args = append(args, *filters.AccountID)
		idx++
	}
	if filters.GroupID != nil {
		conditions = append(conditions, fmt.Sprintf("group_id = $%d", idx))
		args = append(args, *filters.GroupID)
		idx++
	}
	if filters.Model != nil {
		model := strings.TrimSpace(*filters.Model)
		if model != "" {
			conditions = append(conditions, fmt.Sprintf("model = $%d", idx))
			args = append(args, model)
			idx++
		}
	}
	if filters.RequestType != nil {
		condition, conditionArgs := buildRequestTypeFilterCondition(idx, *filters.RequestType)
		conditions = append(conditions, condition)
		args = append(args, conditionArgs...)
		idx += len(conditionArgs)
	} else if filters.Stream != nil {
		conditions = append(conditions, fmt.Sprintf("stream = $%d", idx))
		args = append(args, *filters.Stream)
		idx++
	}
	if filters.BillingType != nil {
		conditions = append(conditions, fmt.Sprintf("billing_type = $%d", idx))
		args = append(args, *filters.BillingType)
	}
	return strings.Join(conditions, " AND "), args
}

func (r *usageCleanupRepository) createTaskWithEnt(ctx context.Context, task *service.UsageCleanupTask) error {
	client := clientFromContext(ctx, r.client)
	filtersJSON, err := json.Marshal(task.Filters)
	if err != nil {
		return fmt.Errorf("marshal cleanup filters: %w", err)
	}
	created, err := client.UsageCleanupTask.
		Create().
		SetStatus(task.Status).
		SetFilters(json.RawMessage(filtersJSON)).
		SetCreatedBy(task.CreatedBy).
		SetDeletedRows(task.DeletedRows).
		Save(ctx)
	if err != nil {
		return err
	}
	task.ID = created.ID
	task.CreatedAt = created.CreatedAt
	task.UpdatedAt = created.UpdatedAt
	return nil
}

func (r *usageCleanupRepository) createTaskWithSQL(ctx context.Context, task *service.UsageCleanupTask) error {
	filtersJSON, err := json.Marshal(task.Filters)
	if err != nil {
		return fmt.Errorf("marshal cleanup filters: %w", err)
	}
	query := `
		INSERT INTO usage_cleanup_tasks (
			status,
			filters,
			created_by,
			deleted_rows
		) VALUES ($1, $2, $3, $4)
		RETURNING id, created_at, updated_at
	`
	if err := scanSingleRow(ctx, r.sql, query, []any{task.Status, filtersJSON, task.CreatedBy, task.DeletedRows}, &task.ID, &task.CreatedAt, &task.UpdatedAt); err != nil {
		return err
	}
	return nil
}

func (r *usageCleanupRepository) listTasksWithEnt(ctx context.Context, params pagination.PaginationParams) ([]service.UsageCleanupTask, *pagination.PaginationResult, error) {
	client := clientFromContext(ctx, r.client)
	query := client.UsageCleanupTask.Query()
	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, nil, err
	}
	if total == 0 {
		return []service.UsageCleanupTask{}, paginationResultFromTotal(0, params), nil
	}
	rows, err := query.
		Order(dbent.Desc(dbusagecleanuptask.FieldCreatedAt), dbent.Desc(dbusagecleanuptask.FieldID)).
		Offset(params.Offset()).
		Limit(params.Limit()).
		All(ctx)
	if err != nil {
		return nil, nil, err
	}
	tasks := make([]service.UsageCleanupTask, 0, len(rows))
	for _, row := range rows {
		task, err := usageCleanupTaskFromEnt(row)
		if err != nil {
			return nil, nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, paginationResultFromTotal(int64(total), params), nil
}

func (r *usageCleanupRepository) getTaskStatusWithEnt(ctx context.Context, taskID int64) (string, error) {
	client := clientFromContext(ctx, r.client)
	task, err := client.UsageCleanupTask.Query().
		Where(dbusagecleanuptask.IDEQ(taskID)).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return "", sql.ErrNoRows
		}
		return "", err
	}
	return task.Status, nil
}

func (r *usageCleanupRepository) updateTaskProgressWithEnt(ctx context.Context, taskID int64, deletedRows int64) error {
	client := clientFromContext(ctx, r.client)
	now := time.Now()
	_, err := client.UsageCleanupTask.Update().
		Where(dbusagecleanuptask.IDEQ(taskID)).
		SetDeletedRows(deletedRows).
		SetUpdatedAt(now).
		Save(ctx)
	return err
}

func (r *usageCleanupRepository) cancelTaskWithEnt(ctx context.Context, taskID int64, canceledBy int64) (bool, error) {
	client := clientFromContext(ctx, r.client)
	now := time.Now()
	affected, err := client.UsageCleanupTask.Update().
		Where(
			dbusagecleanuptask.IDEQ(taskID),
			dbusagecleanuptask.StatusIn(service.UsageCleanupStatusPending, service.UsageCleanupStatusRunning),
		).
		SetStatus(service.UsageCleanupStatusCanceled).
		SetCanceledBy(canceledBy).
		SetCanceledAt(now).
		SetFinishedAt(now).
		ClearErrorMessage().
		SetUpdatedAt(now).
		Save(ctx)
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (r *usageCleanupRepository) markTaskSucceededWithEnt(ctx context.Context, taskID int64, deletedRows int64) error {
	client := clientFromContext(ctx, r.client)
	now := time.Now()
	_, err := client.UsageCleanupTask.Update().
		Where(dbusagecleanuptask.IDEQ(taskID)).
		SetStatus(service.UsageCleanupStatusSucceeded).
		SetDeletedRows(deletedRows).
		SetFinishedAt(now).
		SetUpdatedAt(now).
		Save(ctx)
	return err
}

func (r *usageCleanupRepository) markTaskFailedWithEnt(ctx context.Context, taskID int64, deletedRows int64, errorMsg string) error {
	client := clientFromContext(ctx, r.client)
	now := time.Now()
	_, err := client.UsageCleanupTask.Update().
		Where(dbusagecleanuptask.IDEQ(taskID)).
		SetStatus(service.UsageCleanupStatusFailed).
		SetDeletedRows(deletedRows).
		SetErrorMessage(errorMsg).
		SetFinishedAt(now).
		SetUpdatedAt(now).
		Save(ctx)
	return err
}

func usageCleanupTaskFromEnt(row *dbent.UsageCleanupTask) (service.UsageCleanupTask, error) {
	task := service.UsageCleanupTask{
		ID:          row.ID,
		Status:      row.Status,
		CreatedBy:   row.CreatedBy,
		DeletedRows: row.DeletedRows,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
	if len(row.Filters) > 0 {
		if err := json.Unmarshal(row.Filters, &task.Filters); err != nil {
			return service.UsageCleanupTask{}, fmt.Errorf("parse cleanup filters: %w", err)
		}
	}
	if row.ErrorMessage != nil {
		task.ErrorMsg = row.ErrorMessage
	}
	if row.CanceledBy != nil {
		task.CanceledBy = row.CanceledBy
	}
	if row.CanceledAt != nil {
		task.CanceledAt = row.CanceledAt
	}
	if row.StartedAt != nil {
		task.StartedAt = row.StartedAt
	}
	if row.FinishedAt != nil {
		task.FinishedAt = row.FinishedAt
	}
	return task, nil
}
