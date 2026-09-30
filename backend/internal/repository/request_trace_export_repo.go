package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type requestTraceExportRepository struct{ q sqlExecutor }

func NewRequestTraceExportRepository(db *sql.DB) service.RequestTraceExportStore {
	if db == nil {
		return &requestTraceExportRepository{}
	}
	return &requestTraceExportRepository{q: db}
}

var _ service.RequestTraceExportStore = (*requestTraceExportRepository)(nil)

func isRequestTraceExportUniqueConstraint(err error) bool {
	var pgErr *pq.Error
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.Constraint == "request_trace_exports_one_active_instance_idx"
}

const requestTraceExportColumns = `export_id, status, filters, created_by, session_digest, instance_id,
	filename, rows_exported, rows_skipped, bytes_exported, created_at, completed_at, download_until,
	limits_snapshot`

func scanRequestTraceExport(row interface{ Scan(...any) error }) (service.RequestTraceExportTask, error) {
	var task service.RequestTraceExportTask
	var filters []byte
	var filename sql.NullString
	var completed, until sql.NullTime
	var limits []byte
	err := row.Scan(&task.ID, &task.Status, &filters, &task.AdminUserID, &task.SessionDigest, &task.InstanceID,
		&filename, &task.RowsExported, &task.RowsSkipped, &task.BytesExported, &task.CreatedAt, &completed, &until, &limits)
	if err != nil {
		return service.RequestTraceExportTask{}, err
	}
	if err := json.Unmarshal(filters, &task.Filter); err != nil {
		return service.RequestTraceExportTask{}, err
	}
	if filename.Valid {
		task.Filename = filename.String
	}
	if completed.Valid {
		task.CompletedAt = &completed.Time
	}
	if until.Valid {
		task.DownloadUntil = &until.Time
	}
	// 只有 NULL 才代表"这一行没有快照"（升级前创建的任务）：执行侧据此退回当前
	// 生效配置。非 NULL 却读不出来不是"没有快照"，而是这个任务的预算被损坏——
	// 必须如实报错，绝不能静默退回，否则一个新建任务的预算会被悄悄换掉。
	if limits != nil {
		var snapshot service.RequestTraceExportLimits
		if err := json.Unmarshal(limits, &snapshot); err != nil {
			return service.RequestTraceExportTask{}, service.ErrRequestTraceExportUnavailable
		}
		task.LimitsSnapshot = &snapshot
	}
	return task, nil
}

func (r *requestTraceExportRepository) Create(ctx context.Context, task service.RequestTraceExportTask, maxInFlight int) error {
	if r == nil || r.q == nil {
		return service.ErrRequestTraceExportUnavailable
	}
	filters, err := json.Marshal(task.Filter)
	if err != nil {
		return service.ErrRequestTraceExportUnavailable
	}
	if maxInFlight < 1 || maxInFlight > 8 {
		return service.ErrRequestTraceExportLimit
	}
	// 创建时固定的资源上限快照与任务行在同一条 INSERT 里落库：任务一旦可被
	// 认领，它的预算就已经确定，不存在"先排队、后补快照"的窗口。
	var snapshot any
	if task.LimitsSnapshot != nil {
		encoded, err := json.Marshal(task.LimitsSnapshot)
		if err != nil {
			return service.ErrRequestTraceExportUnavailable
		}
		snapshot = string(encoded)
	}
	// Serialize the capacity check across processes sharing this database.
	var id string
	err = scanSingleRow(ctx, r.q, `WITH lock AS (SELECT pg_advisory_xact_lock(hashtext('request_trace_export_admit'))),
		admission AS (SELECT COUNT(*) AS active FROM request_trace_exports, lock
			WHERE instance_id=$6 AND status IN ('pending','running'))
		INSERT INTO request_trace_exports
		(export_id,status,filters,created_by,session_digest,instance_id,created_at,limits_snapshot)
		SELECT $1,$2,$3,$4,$5,$6,$7,$9::jsonb FROM admission WHERE active < $8 RETURNING export_id`,
		[]any{task.ID, task.Status, filters, task.AdminUserID, task.SessionDigest, task.InstanceID, task.CreatedAt, maxInFlight, snapshot}, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrRequestTraceExportLimit
	}
	if err != nil && isRequestTraceExportUniqueConstraint(err) {
		return service.ErrRequestTraceExportLimit
	}
	return err
}

func (r *requestTraceExportRepository) Get(ctx context.Context, id string) (service.RequestTraceExportTask, error) {
	if r == nil || r.q == nil {
		return service.RequestTraceExportTask{}, service.ErrRequestTraceExportUnavailable
	}
	rows, err := r.q.QueryContext(ctx, `SELECT `+requestTraceExportColumns+` FROM request_trace_exports WHERE export_id=$1`, id)
	if err != nil {
		return service.RequestTraceExportTask{}, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return service.RequestTraceExportTask{}, err
		}
		return service.RequestTraceExportTask{}, service.ErrRequestTraceExportNotFound
	}
	task, err := scanRequestTraceExport(rows)
	if err != nil {
		return service.RequestTraceExportTask{}, err
	}
	return task, rows.Err()
}

// requestTraceExportMaxRecallTasks 是"同会话找回"一页最多读回的任务数。
// 它同时是**行**的上界与游标的边界：超过即拒绝，绝不静默截断成"看起来就这么多"。
const requestTraceExportMaxRecallTasks = 100

// 找回的分页读是生产路径上必须存在的能力：本仓库实现了它，服务层在没有它的存储上
// 会明确失败而不是退回"只有一页"。见 service.RequestTraceExportPagedRecallStore。
var _ service.RequestTraceExportPagedRecallStore = (*requestTraceExportRepository)(nil)

// ListForSession 返回某个管理员在**某个登录会话**下、留在**本实例**上的导出任务，
// 最近创建的在前；after 为 nil 时是第一页，否则从该键集边界之后继续下一页。
// 它是 ticket09 的找回接缝：离开或刷新页面后，操作员仍能拿回自己的句柄，
// 而其它会话、其它管理员、其它实例的行永远不在结果里。
//
// 三个过滤条件缺一不可，且都由**调用方已验签的身份**推出，不接受请求参数：
//   - created_by  —— 同一管理员
//   - session_digest —— 同一登录会话（库里只有摘要，原文永不落库）
//   - instance_id —— 本实例；本机临时文件只属于产出它的实例
//
// 结果行不含任何路径或会话原文：filename 与 session_digest 在服务层被剥掉，
// 这里读回它们只是为了复用同一套扫描，不代表它们可以离开进程。
//
// 次序是 (created_at DESC, export_id DESC)，续页用同一组键做**严格小于**的键集比较，
// 因此两页之间不会重复也不会漏行，且边界是一个值而不是行引用——作为边界的那一行
// 即使随后被清理，续页依然从它之后继续，不会重头开始。页大小有界
// （见 requestTraceExportMaxRecallTasks）。
//
// 第二个返回值回答"边界之后还有行"：读回 limit 行既可能是最后一页、也可能是刚好
// 装满一页，只有多读一行才能区分；服务层据它决定要不要给出下一页的游标，而不是把
// "刚好一页"说成"没有更多"。
func (r *requestTraceExportRepository) ListForSession(ctx context.Context, adminUserID int64, sessionDigest, instanceID string, after *service.RequestTraceExportRecallCursor, limit int) ([]service.RequestTraceExportTask, bool, error) {
	if r == nil || r.q == nil {
		return nil, false, service.ErrRequestTraceExportUnavailable
	}
	if limit <= 0 || limit > requestTraceExportMaxRecallTasks {
		return nil, false, service.ErrRequestTraceExportLimit
	}
	// 第一页没有边界：两个参数都是 NULL，由 $4 IS NULL 分支放行整批行。
	var afterCreated, afterID any
	if after != nil {
		afterCreated = after.CreatedAt.UTC()
		afterID = after.ExportID
	}
	rows, err := r.q.QueryContext(ctx, `SELECT `+requestTraceExportColumns+` FROM request_trace_exports
		WHERE created_by=$1 AND session_digest=$2 AND instance_id=$3
			AND ($4::timestamptz IS NULL OR (created_at, export_id) < ($4::timestamptz, $5::text))
		ORDER BY created_at DESC, export_id DESC LIMIT $6`,
		adminUserID, sessionDigest, instanceID, afterCreated, afterID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	tasks := make([]service.RequestTraceExportTask, 0, limit+1)
	for rows.Next() {
		task, err := scanRequestTraceExport(rows)
		if err != nil {
			return nil, false, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	// 多读的那一行只用来回答"还有没有下一页"，本身不属于这一页。
	more := len(tasks) > limit
	if more {
		tasks = tasks[:limit]
	}
	return tasks, more, nil
}

func (r *requestTraceExportRepository) Claim(ctx context.Context, instanceID string) (service.RequestTraceExportTask, error) {
	if r == nil || r.q == nil {
		return service.RequestTraceExportTask{}, service.ErrRequestTraceExportUnavailable
	}
	rows, err := r.q.QueryContext(ctx, `WITH next AS (
		SELECT export_id FROM request_trace_exports
		WHERE status='pending' AND instance_id=$1 ORDER BY created_at,export_id
		LIMIT 1 FOR UPDATE SKIP LOCKED
	) UPDATE request_trace_exports e SET status='running',started_at=NOW()
	FROM next WHERE e.export_id=next.export_id RETURNING e.export_id, e.status, e.filters, e.created_by,
		e.session_digest, e.instance_id, e.filename, e.rows_exported, e.rows_skipped,
		e.bytes_exported, e.created_at, e.completed_at, e.download_until, e.limits_snapshot`, instanceID)
	if err != nil {
		return service.RequestTraceExportTask{}, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return service.RequestTraceExportTask{}, err
		}
		return service.RequestTraceExportTask{}, service.ErrRequestTraceExportNotFound
	}
	task, err := scanRequestTraceExport(rows)
	if err != nil {
		return service.RequestTraceExportTask{}, err
	}
	return task, rows.Err()
}

func (r *requestTraceExportRepository) Finish(ctx context.Context, task service.RequestTraceExportTask) error {
	if r == nil || r.q == nil {
		return service.ErrRequestTraceExportUnavailable
	}
	var filename, completed, until any
	if task.Filename != "" {
		filename = task.Filename
	}
	if task.CompletedAt != nil {
		completed = task.CompletedAt.UTC()
	}
	if task.DownloadUntil != nil {
		until = task.DownloadUntil.UTC()
	}
	var id string
	err := scanSingleRow(ctx, r.q, `UPDATE request_trace_exports SET status=$2, filename=$3,
		rows_exported=$4,rows_skipped=$5,bytes_exported=$6,completed_at=$7,download_until=$8
		WHERE export_id=$1 AND status IN ('running','pending') RETURNING export_id`,
		[]any{task.ID, task.Status, filename, task.RowsExported, task.RowsSkipped, task.BytesExported, completed, until}, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrRequestTraceExportNotFound
	}
	return err
}

func (r *requestTraceExportRepository) ListStale(ctx context.Context, instanceID string, before time.Time, limit int) ([]service.RequestTraceExportTask, error) {
	if r == nil || r.q == nil {
		return nil, service.ErrRequestTraceExportUnavailable
	}
	if limit <= 0 || limit > 500 {
		return nil, service.ErrRequestTraceExportLimit
	}
	rows, err := r.q.QueryContext(ctx, `SELECT `+requestTraceExportColumns+` FROM request_trace_exports
		WHERE status IN ('running','pending') AND COALESCE(started_at,created_at) < $1
		ORDER BY COALESCE(started_at,created_at),export_id LIMIT $2`, before.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	tasks := make([]service.RequestTraceExportTask, 0)
	for rows.Next() {
		task, err := scanRequestTraceExport(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (r *requestTraceExportRepository) Expired(ctx context.Context, before time.Time, limit int) ([]service.RequestTraceExportTask, error) {
	if r == nil || r.q == nil {
		return nil, service.ErrRequestTraceExportUnavailable
	}
	if limit <= 0 || limit > 500 {
		return nil, service.ErrRequestTraceExportLimit
	}
	rows, err := r.q.QueryContext(ctx, `SELECT `+requestTraceExportColumns+` FROM request_trace_exports
		WHERE download_until <= $1 OR (status = 'failed' AND COALESCE(completed_at, created_at) <= $1 - INTERVAL '7 days')
		ORDER BY COALESCE(download_until, created_at + INTERVAL '7 days'), export_id LIMIT $2`, before.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	tasks := make([]service.RequestTraceExportTask, 0)
	for rows.Next() {
		task, err := scanRequestTraceExport(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// ExpiredAfter pages the expired set strictly after the given export_id in
// ascending id order. It is the cursor form the expiry sweep uses so a run of
// non-deletable rows cannot starve later ones; an empty cursor starts at the
// head and the primary key index makes the scan bounded by limit.
func (r *requestTraceExportRepository) ExpiredAfter(ctx context.Context, before time.Time, after string, limit int) ([]service.RequestTraceExportTask, error) {
	if r == nil || r.q == nil {
		return nil, service.ErrRequestTraceExportUnavailable
	}
	if limit <= 0 || limit > 500 {
		return nil, service.ErrRequestTraceExportLimit
	}
	rows, err := r.q.QueryContext(ctx, `SELECT `+requestTraceExportColumns+` FROM request_trace_exports
		WHERE (download_until <= $1 OR (status = 'failed' AND COALESCE(completed_at, created_at) <= $1 - INTERVAL '7 days'))
			AND export_id > $2
		ORDER BY export_id LIMIT $3`, before.UTC(), after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	tasks := make([]service.RequestTraceExportTask, 0)
	for rows.Next() {
		task, err := scanRequestTraceExport(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (r *requestTraceExportRepository) Delete(ctx context.Context, id string) error {
	if r == nil || r.q == nil {
		return service.ErrRequestTraceExportUnavailable
	}
	_, err := r.q.ExecContext(ctx, `DELETE FROM request_trace_exports WHERE export_id=$1`, id)
	return err
}
