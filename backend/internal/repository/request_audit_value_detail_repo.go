package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 值明细的**历史留存存储**（票据 10 退役后只剩这一段）。
//
// 采集与读取语句已随之删除：这个仓储只能清除旧的七密文，无法再写入或读出任何值。
// 明文行没有自己的到期时刻，它们的唯一所有者是使用记录（随 usage 删除），
// 所以这里既不统计它们，也不清理它们。
type requestAuditValueDetailRepository struct {
	db *sql.DB
}

// NewRequestAuditValueDetailRepository 构造值明细留存仓储。
//
// 不再需要加密器：值明细的读写路径都已移除，密文只会被清除。
func NewRequestAuditValueDetailRepository(db *sql.DB) service.RequestAuditValueDetailRepository {
	return &requestAuditValueDetailRepository{db: db}
}

const requestAuditValueDetailClearStatement = `
	UPDATE request_audit_value_details
	SET ciphertext = NULL, key_version = 0, state = 'purged'
	WHERE id IN (
		SELECT id FROM request_audit_value_details
		WHERE ciphertext IS NOT NULL
		  AND expires_at <= $1
		ORDER BY expires_at ASC
		LIMIT $2
	)
`

// ReadRequestAuditValueDetailCleanupBacklog counts legacy ciphertext whose
// seven-day deadline has passed. New plaintext has no deadline; it is removed
// when its owning usage is deleted.
func (r *requestAuditValueDetailRepository) ReadRequestAuditValueDetailCleanupBacklog(ctx context.Context, now time.Time) (int64, time.Time, error) {
	if r == nil || r.db == nil {
		return 0, time.Time{}, service.ErrRequestAuditValueDetailUnavailable
	}
	var oldest sql.NullTime
	var count int64
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(expires_at) FROM request_audit_value_details
		WHERE ciphertext IS NOT NULL AND expires_at <= $1`, now.UTC()).Scan(&count, &oldest)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("request audit value detail cleanup backlog: %w", err)
	}
	if oldest.Valid {
		return count, oldest.Time, nil
	}
	return count, time.Time{}, nil
}

// Only legacy ciphertext has a seven-day cleanup deadline. Plaintext rows are
// deleted when their owning usage log is removed.
func (r *requestAuditValueDetailRepository) ClearExpiredRequestAuditValueDetails(ctx context.Context, now time.Time, limit int) (int64, error) {
	if r == nil || r.db == nil {
		return 0, service.ErrRequestAuditValueDetailUnavailable
	}
	if limit <= 0 {
		return 0, nil
	}
	result, err := r.db.ExecContext(ctx, requestAuditValueDetailClearStatement, now.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("clear expired request audit value details: %w", err)
	}
	cleared, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("clear expired request audit value details: %w", err)
	}
	return cleared, nil
}
