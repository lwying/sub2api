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
	filename, rows_exported, rows_skipped, bytes_exported, created_at, completed_at, download_until`

func scanRequestTraceExport(row interface{ Scan(...any) error }) (service.RequestTraceExportTask, error) {
	var task service.RequestTraceExportTask
	var filters []byte
	var filename sql.NullString
	var completed, until sql.NullTime
	err := row.Scan(&task.ID, &task.Status, &filters, &task.AdminUserID, &task.SessionDigest, &task.InstanceID,
		&filename, &task.RowsExported, &task.RowsSkipped, &task.BytesExported, &task.CreatedAt, &completed, &until)
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
	// Serialize the capacity check across processes sharing this database.
	var id string
	err = scanSingleRow(ctx, r.q, `WITH lock AS (SELECT pg_advisory_xact_lock(hashtext('request_trace_export_admit'))),
		admission AS (SELECT COUNT(*) AS active FROM request_trace_exports, lock
			WHERE instance_id=$6 AND status IN ('pending','running'))
		INSERT INTO request_trace_exports
		(export_id,status,filters,created_by,session_digest,instance_id,created_at)
		SELECT $1,$2,$3,$4,$5,$6,$7 FROM admission WHERE active < $8 RETURNING export_id`,
		[]any{task.ID, task.Status, filters, task.AdminUserID, task.SessionDigest, task.InstanceID, task.CreatedAt, maxInFlight}, &id)
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
		e.bytes_exported, e.created_at, e.completed_at, e.download_until`, instanceID)
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
