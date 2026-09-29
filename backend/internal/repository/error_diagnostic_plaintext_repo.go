package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.ErrorDiagnosticPlaintextReader = (*errorDiagnosticRepository)(nil)

// 历史明文诊断载荷的**清理**（票据 10 退役后只剩这一段）。
//
// 读取方法已随揭示入口一并移除；这里只把未关联的到期明文载荷清空。已关联 usage 的行
// 不在范围内：它们的所有者是使用记录，整行随 usage 删除，没有自有到期时刻。

// ClearExpiredErrorDiagnosticPlainBodies 在在线主库物理清除已过三十天的**未关联**明文正文。
//
// 只置空载荷列并把状态记成 purged，保留原因码与整行元数据：
// 使「曾留存、现已清除」与「从未留存」在状态上可区分（与旧密文层同一约定）。
func (r *errorDiagnosticRepository) ClearExpiredErrorDiagnosticPlainBodies(ctx context.Context, now time.Time, batch int) (int64, error) {
	if r == nil || r.db == nil {
		return 0, service.ErrErrorDiagnosticUnavailable
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE error_diagnostic_records
		SET plain_body_payload = NULL, plain_body_state = 'purged', plain_body_bytes = 0
		WHERE diagnostic_id IN (
			SELECT diagnostic_id FROM error_diagnostic_records
			WHERE plain_record
			  AND plain_owner_usage_log_id IS NULL
			  AND plain_body_payload IS NOT NULL
			  AND metadata_expires_at <= $1
			ORDER BY metadata_expires_at ASC
			LIMIT $2
		)
	`, now.UTC(), batch)
	if err != nil {
		return 0, fmt.Errorf("clear expired error diagnostic plain bodies: %w", err)
	}
	cleared, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("clear expired error diagnostic plain bodies: %w", err)
	}
	return cleared, nil
}

// ClearExpiredErrorDiagnosticPlainHeaderValues 同形，属 429 头值层。
func (r *errorDiagnosticRepository) ClearExpiredErrorDiagnosticPlainHeaderValues(ctx context.Context, now time.Time, batch int) (int64, error) {
	if r == nil || r.db == nil {
		return 0, service.ErrErrorDiagnosticUnavailable
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE error_diagnostic_records
		SET plain_header_payload = NULL, plain_header_state = 'purged',
		    plain_header_bytes = 0, plain_header_entry_count = 0
		WHERE diagnostic_id IN (
			SELECT diagnostic_id FROM error_diagnostic_records
			WHERE plain_record
			  AND plain_owner_usage_log_id IS NULL
			  AND plain_header_payload IS NOT NULL
			  AND metadata_expires_at <= $1
			ORDER BY metadata_expires_at ASC
			LIMIT $2
		)
	`, now.UTC(), batch)
	if err != nil {
		return 0, fmt.Errorf("clear expired error diagnostic plain header values: %w", err)
	}
	cleared, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("clear expired error diagnostic plain header values: %w", err)
	}
	return cleared, nil
}
