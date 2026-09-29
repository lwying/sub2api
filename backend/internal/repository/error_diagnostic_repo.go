package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 独立错误诊断的**历史留存存储**（票据 10 退役后只剩这一段）。
//
// 写入与读取语句已随之删除：这个仓储只能清除已存在的在线主库副本，无法再写入、
// 列出或读出任何正文／头值。到期语义分两段，都由本层按 created_at 计算：
//   - metadata_expires_at = now + 30 天：清理物理删除整行（未关联的行）；
//   - body_expires_at／header_expires_at = now + 7 天：清理物理清除密文列。
//
// 本层只做在线主库的物理删除；副本、备份与 PITR 的留存由部署方决定，
// 这里不声称、也无法证明那些存储层的到期不可恢复。
type errorDiagnosticRepository struct {
	db *sql.DB
}

// NewErrorDiagnosticRepository 构造诊断留存仓储。
//
// 不再需要正文加密器：写入与解密读取路径都已移除，密文只会被清除。
func NewErrorDiagnosticRepository(db *sql.DB) service.ErrorDiagnosticRepository {
	return &errorDiagnosticRepository{db: db}
}

// ClearExpiredErrorDiagnosticBodies 在在线主库物理清除已到第 7 天的正文密文。
//
// 只置空密文列与密钥代，保留整行元数据与 body_expires_at，
// 使「曾留存、现已清除」与「从未留存」在状态上可区分。
func (r *errorDiagnosticRepository) ClearExpiredErrorDiagnosticBodies(ctx context.Context, now time.Time, batch int) (int64, error) {
	if r == nil || r.db == nil {
		return 0, service.ErrErrorDiagnosticUnavailable
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE error_diagnostic_records
		SET body_ciphertext = NULL, body_key_version = 0, body_state = 'purged'
		WHERE diagnostic_id IN (
			SELECT diagnostic_id FROM error_diagnostic_records
			WHERE body_ciphertext IS NOT NULL
			  AND body_expires_at IS NOT NULL
			  AND body_expires_at <= $1
			ORDER BY body_expires_at ASC
			LIMIT $2
		)
	`, now.UTC(), batch)
	if err != nil {
		return 0, fmt.Errorf("clear expired error diagnostic bodies: %w", err)
	}
	cleared, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("clear expired error diagnostic bodies: %w", err)
	}
	return cleared, nil
}

// ClearExpiredErrorDiagnosticHeaderValues 在在线主库物理清除已到第 7 天的头值密文。
//
// 与正文同一约定：只置空密文列与密钥代并记 purged，保留整行元数据与 header_expires_at，
// 使「曾留存、现已清除」与「从未留存」在状态上可区分。
func (r *errorDiagnosticRepository) ClearExpiredErrorDiagnosticHeaderValues(ctx context.Context, now time.Time, batch int) (int64, error) {
	if r == nil || r.db == nil {
		return 0, service.ErrErrorDiagnosticUnavailable
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE error_diagnostic_records
		SET header_ciphertext = NULL, header_key_version = 0, header_state = 'purged'
		WHERE diagnostic_id IN (
			SELECT diagnostic_id FROM error_diagnostic_records
			WHERE header_ciphertext IS NOT NULL
			  AND header_expires_at IS NOT NULL
			  AND header_expires_at <= $1
			ORDER BY header_expires_at ASC
			LIMIT $2
		)
	`, now.UTC(), batch)
	if err != nil {
		return 0, fmt.Errorf("clear expired error diagnostic header values: %w", err)
	}
	cleared, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("clear expired error diagnostic header values: %w", err)
	}
	return cleared, nil
}

// ReadErrorDiagnosticCleanupBacklog 读取「已过保留期但仍未清理」的积压量与最老到期时刻。
//
// 只用于监控：应用层在到期时刻就已经拒绝读取（该读取入口已随退役移除），因此积压只反映
// 在线主库上的物理残留，不代表这段时间内内容可被访问。各段聚合都命中清理索引。
func (r *errorDiagnosticRepository) ReadErrorDiagnosticCleanupBacklog(ctx context.Context, now time.Time) (service.ErrorDiagnosticCleanupBacklog, error) {
	if r == nil || r.db == nil {
		return service.ErrorDiagnosticCleanupBacklog{}, service.ErrErrorDiagnosticUnavailable
	}
	now = now.UTC()
	var backlog service.ErrorDiagnosticCleanupBacklog

	var oldestBody sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*), MIN(body_expires_at)
		FROM error_diagnostic_records
		WHERE body_ciphertext IS NOT NULL
		  AND body_expires_at IS NOT NULL
		  AND body_expires_at <= $1
	`, now).Scan(&backlog.BodiesOverdue, &oldestBody)
	if err != nil {
		return service.ErrorDiagnosticCleanupBacklog{}, fmt.Errorf("read error diagnostic body backlog: %w", err)
	}
	if oldestBody.Valid {
		backlog.OldestBodyOverdueAt = oldestBody.Time
	}

	// 已关联 usage 的明文行不构成记录积压：它们的三十天列只是「未关联时的读取上限」，
	// 不是删除时刻。把它们算成积压会让监控长期报出一个永远不会被清理的数字。
	var oldestRecord sql.NullTime
	err = r.db.QueryRowContext(ctx, `
		SELECT COUNT(*), MIN(metadata_expires_at)
		FROM error_diagnostic_records
		WHERE metadata_expires_at <= $1
		  AND NOT (plain_record AND plain_owner_usage_log_id IS NOT NULL)
	`, now).Scan(&backlog.RecordsOverdue, &oldestRecord)
	if err != nil {
		return service.ErrorDiagnosticCleanupBacklog{}, fmt.Errorf("read error diagnostic record backlog: %w", err)
	}
	if oldestRecord.Valid {
		backlog.OldestRecordOverdueAt = oldestRecord.Time
	}

	var oldestHeaderValue sql.NullTime
	err = r.db.QueryRowContext(ctx, `
		SELECT COUNT(*), MIN(header_expires_at)
		FROM error_diagnostic_records
		WHERE header_ciphertext IS NOT NULL
		  AND header_expires_at IS NOT NULL
		  AND header_expires_at <= $1
	`, now).Scan(&backlog.HeaderValuesOverdue, &oldestHeaderValue)
	if err != nil {
		return service.ErrorDiagnosticCleanupBacklog{}, fmt.Errorf("read error diagnostic header value backlog: %w", err)
	}
	if oldestHeaderValue.Valid {
		backlog.OldestHeaderOverdueAt = oldestHeaderValue.Time
	}

	// 明文层的积压单独观测：它们是**明文**残留，与旧密文残留的处置不同，
	// 合并成一个数会让运维看不出哪一层在落后。只统计未关联且已过 metadata_expires_at 的行——
	// 已关联的明文随使用记录存在，没有自有到期时刻。
	var oldestPlainBody sql.NullTime
	err = r.db.QueryRowContext(ctx, `
		SELECT COUNT(*), MIN(metadata_expires_at)
		FROM error_diagnostic_records
		WHERE plain_record
		  AND plain_owner_usage_log_id IS NULL
		  AND plain_body_payload IS NOT NULL
		  AND metadata_expires_at <= $1
	`, now).Scan(&backlog.PlainBodiesOverdue, &oldestPlainBody)
	if err != nil {
		return service.ErrorDiagnosticCleanupBacklog{}, fmt.Errorf("read error diagnostic plain body backlog: %w", err)
	}
	if oldestPlainBody.Valid {
		backlog.OldestPlainBodyOverdueAt = oldestPlainBody.Time
	}

	var oldestPlainHeader sql.NullTime
	err = r.db.QueryRowContext(ctx, `
		SELECT COUNT(*), MIN(metadata_expires_at)
		FROM error_diagnostic_records
		WHERE plain_record
		  AND plain_owner_usage_log_id IS NULL
		  AND plain_header_payload IS NOT NULL
		  AND metadata_expires_at <= $1
	`, now).Scan(&backlog.PlainHeaderValuesOverdue, &oldestPlainHeader)
	if err != nil {
		return service.ErrorDiagnosticCleanupBacklog{}, fmt.Errorf("read error diagnostic plain header value backlog: %w", err)
	}
	if oldestPlainHeader.Valid {
		backlog.OldestPlainHeaderOverdueAt = oldestPlainHeader.Time
	}
	return backlog, nil
}

// DeleteExpiredErrorDiagnostics 在在线主库物理删除已到第 30 天的整行元数据。
//
// 不触碰 usage_logs：诊断的过期与使用记录的清理互不阻塞。
//
// **已关联的明文行不在范围内**：它们的所有者是使用记录，三十天只是「未关联时」的读取
// 上限，不是删除时刻。把它们按三十天删掉会让明文早于 usage 消失（更糟的是，那会让
// 「已关联」看起来和「未关联」没有区别）。它们的物理删除由 usage 级联负责。
func (r *errorDiagnosticRepository) DeleteExpiredErrorDiagnostics(ctx context.Context, now time.Time, batch int) (int64, error) {
	if r == nil || r.db == nil {
		return 0, service.ErrErrorDiagnosticUnavailable
	}
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM error_diagnostic_records
		WHERE diagnostic_id IN (
			SELECT diagnostic_id FROM error_diagnostic_records
			WHERE metadata_expires_at <= $1
			  AND NOT (plain_record AND plain_owner_usage_log_id IS NOT NULL)
			ORDER BY metadata_expires_at ASC
			LIMIT $2
		)
	`, now.UTC(), batch)
	if err != nil {
		return 0, fmt.Errorf("delete expired error diagnostics: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired error diagnostics: %w", err)
	}
	return deleted, nil
}
