package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.ErrorDiagnosticPlaintextReader = (*errorDiagnosticRepository)(nil)

// 新明文诊断的存储实现（票据 08／09）。
//
// 与旧密文层分开成另一个文件，因为两条路径的到期规则不同：
//   - 旧密文：body_ciphertext／header_ciphertext，第 7 天起不可读并清列；
//   - 新明文：plain_*_payload，**已关联 usage 时随 usage 删除、没有自有窗口**；
//     未关联时 metadata_expires_at 整点起不可读，随后由周期清理清列、到期后由整行删除清行。
//
// 读取的每一个谓词都写在 SQL 里（不是先查出来再在 Go 里判断）：应用层与存储层都要拒绝，
// 且「已到期」不得因为清理延迟而变成可读。
//
// 明文不是密文：这里没有任何解密、也没有任何密钥。缺密钥不影响本层。

// ReadErrorDiagnosticPlainBody 返回未加密的明文出站正文。
//
// 未留存、已清理、未关联且已过 metadata_expires_at 一律返回 ErrErrorDiagnosticBodyGone，
// 使读取结果不能作为「这条尝试是否留过正文」的探针。
func (r *errorDiagnosticRepository) ReadErrorDiagnosticPlainBody(ctx context.Context, id string, now time.Time) ([]byte, error) {
	if r == nil || r.db == nil {
		return nil, service.ErrErrorDiagnosticUnavailable
	}
	if !service.ValidErrorDiagnosticID(id) {
		return nil, service.ErrErrorDiagnosticBodyGone
	}
	var payload []byte
	err := r.db.QueryRowContext(ctx, `
		SELECT plain_body_payload
		FROM error_diagnostic_records
		WHERE diagnostic_id = $1
		  AND plain_record
		  AND plain_body_payload IS NOT NULL
		  AND (plain_owner_usage_log_id IS NOT NULL OR metadata_expires_at > $2)
	`, id, now.UTC()).Scan(&payload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrErrorDiagnosticBodyGone
		}
		return nil, fmt.Errorf("read error diagnostic plain body: %w", err)
	}
	if len(payload) == 0 || len(payload) > service.ErrorDiagnosticMaxBodyReadBytes {
		// 空载荷与越界载荷都不是可披露的正文：前者说明行与列不一致，后者说明存储被改写。
		return nil, service.ErrErrorDiagnosticBodyGone
	}
	return payload, nil
}

// ReadErrorDiagnosticPlainHeaderValues 返回未加密的明文 429 头值。
//
// 载荷同样要重新过一遍白名单与有界校验：库里存着不等于内容可信（改库、旧载荷、
// 被人为写入的半份快照都可能落在这里），不合格一律按不可读处理，绝不返回部分结果。
func (r *errorDiagnosticRepository) ReadErrorDiagnosticPlainHeaderValues(ctx context.Context, id string, now time.Time) (service.ErrorDiagnosticHeaderValues, error) {
	if r == nil || r.db == nil {
		return service.ErrorDiagnosticHeaderValues{}, service.ErrErrorDiagnosticUnavailable
	}
	if !service.ValidErrorDiagnosticID(id) {
		return service.ErrorDiagnosticHeaderValues{}, service.ErrErrorDiagnosticHeaderValuesGone
	}
	var payload []byte
	err := r.db.QueryRowContext(ctx, `
		SELECT plain_header_payload
		FROM error_diagnostic_records
		WHERE diagnostic_id = $1
		  AND plain_record
		  AND plain_header_payload IS NOT NULL
		  AND (plain_owner_usage_log_id IS NOT NULL OR metadata_expires_at > $2)
	`, id, now.UTC()).Scan(&payload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrorDiagnosticHeaderValues{}, service.ErrErrorDiagnosticHeaderValuesGone
		}
		return service.ErrorDiagnosticHeaderValues{}, fmt.Errorf("read error diagnostic plain header values: %w", err)
	}
	values, err := service.DecodeErrorDiagnosticHeaderValues(payload)
	if err != nil {
		return service.ErrorDiagnosticHeaderValues{}, service.ErrErrorDiagnosticHeaderValuesGone
	}
	return values, nil
}

// ClearExpiredErrorDiagnosticPlainBodies 在在线主库物理清除已过三十天的**未关联**明文正文。
//
// 只置空载荷列并把状态记成 purged，保留原因码与整行元数据：
// 使「曾留存、现已清除」与「从未留存」在状态上可区分（与旧密文层同一约定）。
// 已关联 usage 的行不在范围内：它们的所有者是使用记录，整行随 usage 删除。
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
