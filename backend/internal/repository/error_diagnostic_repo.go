package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 错误诊断记录的存储实现：原生 SQL + database/sql。
//
// 该表由编号迁移建立，不经过 Ent 生成代码，因此不进入 ent 事务。
// 到期语义分两段，都由本层按 created_at 计算：
//   - metadata_expires_at = now + 30 天：读取路径拒绝，清理物理删除整行；
//   - body_expires_at     = now + 7 天：读取路径拒绝，清理物理清除密文列。
//
// 本层只做在线主库的物理删除；副本、备份与 PITR 的留存由部署方决定，
// 这里不声称、也无法证明那些存储层的到期不可恢复。
type errorDiagnosticRepository struct {
	db     *sql.DB
	cipher service.ErrorDiagnosticBodyCipher
}

// NewErrorDiagnosticRepository 构造诊断仓储；cipher 可为 nil，此时正文一律不可读。
func NewErrorDiagnosticRepository(db *sql.DB, cipher service.ErrorDiagnosticBodyCipher) service.ErrorDiagnosticRepository {
	return &errorDiagnosticRepository{db: db, cipher: cipher}
}

const errorDiagnosticRecordColumns = `
	diagnostic_id, usage_log_id, protocol, attempt_index, stage, upstream_status,
	body_state, body_reason, (body_ciphertext IS NOT NULL), body_bytes, body_key_version,
	created_at, metadata_expires_at, body_expires_at,
	header_state, header_reason, (header_ciphertext IS NOT NULL), header_bytes, header_key_version,
	header_entry_count, header_expires_at
`

type errorDiagnosticRowScanner interface {
	Scan(dest ...any) error
}

func scanErrorDiagnosticRecord(row errorDiagnosticRowScanner) (service.ErrorDiagnosticRecord, error) {
	var record service.ErrorDiagnosticRecord
	var usageLogID sql.NullInt64
	var bodyExpiresAt sql.NullTime
	var headerExpiresAt sql.NullTime
	err := row.Scan(
		&record.ID, &usageLogID, &record.Protocol, &record.AttemptIndex, &record.Stage,
		&record.UpstreamStatusCode, &record.BodyState, &record.BodyReason, &record.BodyStored,
		&record.BodyBytes, &record.BodyKeyVersion, &record.CreatedAt, &record.MetadataExpiresAt,
		&bodyExpiresAt,
		&record.HeaderState, &record.HeaderReason, &record.HeaderStored, &record.HeaderBytes,
		&record.HeaderKeyVersion, &record.HeaderEntryCount, &headerExpiresAt,
	)
	if err != nil {
		return service.ErrorDiagnosticRecord{}, err
	}
	if usageLogID.Valid {
		record.UsageLogID = usageLogID.Int64
		record.HasUsage = true
	}
	if bodyExpiresAt.Valid {
		record.BodyExpiresAt = bodyExpiresAt.Time
	}
	if headerExpiresAt.Valid {
		record.HeaderExpiresAt = headerExpiresAt.Time
	}
	return record, nil
}

func (r *errorDiagnosticRepository) CreateErrorDiagnostic(ctx context.Context, write service.ErrorDiagnosticWrite, now time.Time) (service.ErrorDiagnosticRecord, error) {
	if r == nil || r.db == nil {
		return service.ErrorDiagnosticRecord{}, service.ErrErrorDiagnosticUnavailable
	}
	if !service.ValidErrorDiagnosticID(write.ID) {
		return service.ErrorDiagnosticRecord{}, service.ErrErrorDiagnosticInvalidAttempt
	}

	now = now.UTC()
	metadataExpiresAt := now.Add(service.ErrorDiagnosticMetadataRetention)

	bodyState := write.BodyState
	bodyReason := write.BodyReason
	if bodyState == "" {
		bodyState = service.ErrorDiagnosticBodyStateNotObserved
	}
	if bodyReason == "" {
		bodyReason = service.ErrorDiagnosticBodyNotObserved
	}

	// 防御性收敛：自称已留存却没有密文的写入必须落成「未留存」，
	// 否则会违反数据库的留存约束，也会污染「已加密留存」的事实。
	bodyCiphertext := write.BodyCiphertext
	bodyKeyVersion := write.BodyKeyVersion
	if bodyState == service.ErrorDiagnosticBodyStateStored && len(bodyCiphertext) == 0 {
		bodyState = service.ErrorDiagnosticBodyStateSkipped
		bodyReason = service.ErrorDiagnosticBodySkippedEncryptionUnavailable
		bodyKeyVersion = 0
	}

	// 头值列与正文列同构，但**独立**判定：正文可以不留而头值留存，反之亦然。
	headerState := write.HeaderState
	headerReason := write.HeaderReason
	if headerState == "" {
		headerState = service.ErrorDiagnosticHeaderStateNotObserved
	}
	if headerReason == "" {
		headerReason = service.ErrorDiagnosticHeaderNotObserved
	}
	headerCiphertext := write.HeaderCiphertext
	headerKeyVersion := write.HeaderKeyVersion
	headerEntryCount := write.HeaderEntryCount
	headerPayloadBytes := write.HeaderPayloadBytes
	if headerState == service.ErrorDiagnosticHeaderStateStored && len(headerCiphertext) == 0 {
		headerState = service.ErrorDiagnosticHeaderStateSkipped
		headerReason = service.ErrorDiagnosticHeaderSkippedEncryptionUnavailable
	}
	if headerState != service.ErrorDiagnosticHeaderStateStored || len(headerCiphertext) == 0 {
		headerKeyVersion = 0
		headerEntryCount = 0
		headerPayloadBytes = 0
	}

	var usageLogID any
	if write.Attempt.UsageLogID > 0 {
		usageLogID = write.Attempt.UsageLogID
	}
	// ciphertext 保持 any：nil 才会被绑定成 SQL NULL。
	// 空的 []byte 会被编码成空 bytea（非 NULL），从而违反「密文与到期时刻成对」的约束。
	var ciphertext any
	var bodyBytes int
	// bodyExpiresAt 用 sql.NullTime 而不是 any：既让无效值绑定为 SQL NULL，
	// 也彻底消除对参数做无检查类型断言的必要。
	var bodyExpiresAt sql.NullTime
	if bodyState == service.ErrorDiagnosticBodyStateStored && len(bodyCiphertext) > 0 {
		ciphertext = bodyCiphertext
		bodyBytes = len(write.Attempt.Body)
		bodyExpiresAt = sql.NullTime{Time: now.Add(service.ErrorDiagnosticBodyRetention), Valid: true}
	} else {
		bodyKeyVersion = 0
	}

	// ciphertext 保持 any：nil 才会被绑定成 SQL NULL（空 []byte 会变成非 NULL 的空 bytea，
	// 从而违反「密文与到期时刻成对」的约束）。
	var headerCiphertextParam any
	var headerExpiresAt sql.NullTime
	if headerState == service.ErrorDiagnosticHeaderStateStored && len(headerCiphertext) > 0 {
		headerCiphertextParam = headerCiphertext
		headerExpiresAt = sql.NullTime{Time: now.Add(service.ErrorDiagnosticHeaderRetention), Valid: true}
	}

	record := service.ErrorDiagnosticRecord{
		ID:                 write.ID,
		UsageLogID:         write.Attempt.UsageLogID,
		HasUsage:           write.Attempt.UsageLogID > 0,
		Protocol:           write.Attempt.Protocol,
		AttemptIndex:       write.Attempt.AttemptIndex,
		Stage:              write.Attempt.Stage,
		UpstreamStatusCode: write.Attempt.UpstreamStatusCode,
		BodyState:          bodyState,
		BodyReason:         bodyReason,
		BodyBytes:          bodyBytes,
		BodyKeyVersion:     bodyKeyVersion,
		BodyStored:         ciphertext != nil,
		MetadataExpiresAt:  metadataExpiresAt,
		// 只有真的绑定了正文时才带到期时刻；否则保持零值（与绑定到 SQL 的 NULL 一致）。
		BodyExpiresAt:    bodyExpiresAt.Time,
		HeaderState:      headerState,
		HeaderReason:     headerReason,
		HeaderBytes:      headerPayloadBytes,
		HeaderKeyVersion: headerKeyVersion,
		HeaderEntryCount: headerEntryCount,
		HeaderStored:     headerCiphertextParam != nil,
		HeaderExpiresAt:  headerExpiresAt.Time,
	}

	err := r.db.QueryRowContext(ctx, `
		INSERT INTO error_diagnostic_records (
			diagnostic_id, usage_log_id, protocol, attempt_index, stage, upstream_status,
			body_state, body_reason, body_ciphertext, body_key_version, body_bytes,
			created_at, metadata_expires_at, body_expires_at,
			header_state, header_reason, header_ciphertext, header_key_version, header_bytes,
			header_entry_count, header_expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
		RETURNING created_at
	`,
		record.ID, usageLogID, record.Protocol, record.AttemptIndex, record.Stage, record.UpstreamStatusCode,
		record.BodyState, record.BodyReason, ciphertext, bodyKeyVersion, bodyBytes,
		now, metadataExpiresAt, bodyExpiresAt,
		headerState, headerReason, headerCiphertextParam, headerKeyVersion, headerPayloadBytes,
		headerEntryCount, headerExpiresAt,
	).Scan(&record.CreatedAt)
	if err != nil {
		return service.ErrorDiagnosticRecord{}, fmt.Errorf("create error diagnostic: %w", err)
	}
	return record, nil
}

func (r *errorDiagnosticRepository) GetErrorDiagnostic(ctx context.Context, id string) (service.ErrorDiagnosticRecord, error) {
	if r == nil || r.db == nil {
		return service.ErrorDiagnosticRecord{}, service.ErrErrorDiagnosticUnavailable
	}
	if !service.ValidErrorDiagnosticID(id) {
		return service.ErrorDiagnosticRecord{}, service.ErrErrorDiagnosticNotFound
	}
	record, err := scanErrorDiagnosticRecord(r.db.QueryRowContext(ctx,
		`SELECT `+errorDiagnosticRecordColumns+` FROM error_diagnostic_records WHERE diagnostic_id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrorDiagnosticRecord{}, service.ErrErrorDiagnosticNotFound
	}
	if err != nil {
		return service.ErrorDiagnosticRecord{}, fmt.Errorf("get error diagnostic: %w", err)
	}
	return record, nil
}

func (r *errorDiagnosticRepository) ListErrorDiagnosticsByUsageLog(ctx context.Context, usageLogID int64, limit int) ([]service.ErrorDiagnosticRecord, error) {
	if r == nil || r.db == nil {
		return nil, service.ErrErrorDiagnosticUnavailable
	}
	if usageLogID <= 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+errorDiagnosticRecordColumns+`
		FROM error_diagnostic_records
		WHERE usage_log_id = $1
		ORDER BY created_at ASC, attempt_index ASC, diagnostic_id ASC
		LIMIT $2
	`, usageLogID, limit)
	if err != nil {
		return nil, fmt.Errorf("list error diagnostics by usage log: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectErrorDiagnosticRecords(rows)
}

func (r *errorDiagnosticRepository) ListRecentErrorDiagnostics(ctx context.Context, protocol string, limit int) ([]service.ErrorDiagnosticRecord, error) {
	if r == nil || r.db == nil {
		return nil, service.ErrErrorDiagnosticUnavailable
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+errorDiagnosticRecordColumns+`
		FROM error_diagnostic_records
		WHERE ($1 = '' OR protocol = $1)
		ORDER BY created_at DESC, diagnostic_id DESC
		LIMIT $2
	`, protocol, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent error diagnostics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectErrorDiagnosticRecords(rows)
}

// ListRecentErrorDiagnosticPage 是分页版本的无 usage 入口。
//
// 与 ListRecentErrorDiagnostics 不同，这里在 SQL 层就排除已过第 30 天的行，
// 因此翻页时不会出现「第 1 页少了几条、第 2 页又补回来」的错位。
func (r *errorDiagnosticRepository) ListRecentErrorDiagnosticPage(ctx context.Context, protocol string, now time.Time, offset, limit int) ([]service.ErrorDiagnosticRecord, error) {
	if r == nil || r.db == nil {
		return nil, service.ErrErrorDiagnosticUnavailable
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+errorDiagnosticRecordColumns+`
		FROM error_diagnostic_records
		WHERE metadata_expires_at > $1
		  AND ($2 = '' OR protocol = $2)
		ORDER BY created_at DESC, diagnostic_id DESC
		OFFSET $3 LIMIT $4
	`, now.UTC(), protocol, offset, limit)
	if err != nil {
		return nil, fmt.Errorf("list error diagnostic page: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectErrorDiagnosticRecords(rows)
}

// CountRecentErrorDiagnostics 统计仍可读（未过第 30 天）的诊断条数。
func (r *errorDiagnosticRepository) CountRecentErrorDiagnostics(ctx context.Context, protocol string, now time.Time) (int64, error) {
	if r == nil || r.db == nil {
		return 0, service.ErrErrorDiagnosticUnavailable
	}
	var count int64
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM error_diagnostic_records
		WHERE metadata_expires_at > $1
		  AND ($2 = '' OR protocol = $2)
	`, now.UTC(), protocol).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count error diagnostics: %w", err)
	}
	return count, nil
}

func collectErrorDiagnosticRecords(rows *sql.Rows) ([]service.ErrorDiagnosticRecord, error) {
	records := make([]service.ErrorDiagnosticRecord, 0, 16)
	for rows.Next() {
		record, err := scanErrorDiagnosticRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan error diagnostic: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate error diagnostics: %w", err)
	}
	return records, nil
}

// ReadErrorDiagnosticBody 只在记录仍持有未到期密文且本层持有密钥时才解密。
//
// 未留存、已物理清除、已到期、密钥缺失或认证失败统一返回 ErrErrorDiagnosticBodyGone，
// 使读取结果不能作为「这条尝试是否曾经留存过正文」的探针。
func (r *errorDiagnosticRepository) ReadErrorDiagnosticBody(ctx context.Context, id string, now time.Time) ([]byte, error) {
	if r == nil || r.db == nil || r.cipher == nil {
		return nil, service.ErrErrorDiagnosticBodyGone
	}
	if !service.ValidErrorDiagnosticID(id) {
		return nil, service.ErrErrorDiagnosticBodyGone
	}
	var ciphertext []byte
	var bodyExpiresAt, metadataExpiresAt sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT body_ciphertext, body_expires_at, metadata_expires_at
		FROM error_diagnostic_records WHERE diagnostic_id = $1
	`, id).Scan(&ciphertext, &bodyExpiresAt, &metadataExpiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrErrorDiagnosticBodyGone
		}
		return nil, fmt.Errorf("read error diagnostic body: %w", err)
	}
	now = now.UTC()
	if !bodyExpiresAt.Valid || !metadataExpiresAt.Valid ||
		!bodyExpiresAt.Time.After(now) || !metadataExpiresAt.Time.After(now) || len(ciphertext) == 0 {
		return nil, service.ErrErrorDiagnosticBodyGone
	}
	plaintext, err := r.cipher.Decrypt(ciphertext)
	if err != nil || len(plaintext) == 0 {
		return nil, service.ErrErrorDiagnosticBodyGone
	}
	return plaintext, nil
}

// ReadErrorDiagnosticHeaderValues 只在记录仍持有未到期头值密文且本层持有密钥时才解密。
//
// 与正文同一约定：未留存、已物理清除、已到期、密钥缺失、认证失败或**解密结果不合格**
// 统一返回 ErrErrorDiagnosticHeaderValuesGone，使读取结果不能作为「这条尝试留过什么」的探针。
// 解密结果还要过一遍白名单与有界校验（DecodeErrorDiagnosticHeaderValues）：
// 解密成功不等于内容可信。
func (r *errorDiagnosticRepository) ReadErrorDiagnosticHeaderValues(ctx context.Context, id string, now time.Time) (service.ErrorDiagnosticHeaderValues, error) {
	if r == nil || r.db == nil || r.cipher == nil {
		return service.ErrorDiagnosticHeaderValues{}, service.ErrErrorDiagnosticHeaderValuesGone
	}
	if !service.ValidErrorDiagnosticID(id) {
		return service.ErrorDiagnosticHeaderValues{}, service.ErrErrorDiagnosticHeaderValuesGone
	}
	var ciphertext []byte
	var headerExpiresAt, metadataExpiresAt sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT header_ciphertext, header_expires_at, metadata_expires_at
		FROM error_diagnostic_records WHERE diagnostic_id = $1
	`, id).Scan(&ciphertext, &headerExpiresAt, &metadataExpiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrorDiagnosticHeaderValues{}, service.ErrErrorDiagnosticHeaderValuesGone
		}
		return service.ErrorDiagnosticHeaderValues{}, fmt.Errorf("read error diagnostic header values: %w", err)
	}
	now = now.UTC()
	if !headerExpiresAt.Valid || !metadataExpiresAt.Valid ||
		!headerExpiresAt.Time.After(now) || !metadataExpiresAt.Time.After(now) || len(ciphertext) == 0 {
		return service.ErrorDiagnosticHeaderValues{}, service.ErrErrorDiagnosticHeaderValuesGone
	}
	plaintext, err := r.cipher.Decrypt(ciphertext)
	if err != nil || len(plaintext) == 0 {
		return service.ErrorDiagnosticHeaderValues{}, service.ErrErrorDiagnosticHeaderValuesGone
	}
	values, err := service.DecodeErrorDiagnosticHeaderValues(plaintext)
	if err != nil {
		return service.ErrorDiagnosticHeaderValues{}, service.ErrErrorDiagnosticHeaderValuesGone
	}
	return values, nil
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
// 只用于监控：应用层在到期时刻就已经拒绝读取，因此积压只反映在线主库上的物理残留，
// 不代表这段时间内内容可被访问。两次聚合走同一连接，且都命中清理索引。
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

	var oldestRecord sql.NullTime
	err = r.db.QueryRowContext(ctx, `
		SELECT COUNT(*), MIN(metadata_expires_at)
		FROM error_diagnostic_records
		WHERE metadata_expires_at <= $1
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
	return backlog, nil
}

// DeleteExpiredErrorDiagnostics 在在线主库物理删除已到第 30 天的整行元数据。
//
// 不触碰 usage_logs：诊断的过期与使用记录的清理互不阻塞。
func (r *errorDiagnosticRepository) DeleteExpiredErrorDiagnostics(ctx context.Context, now time.Time, batch int) (int64, error) {
	if r == nil || r.db == nil {
		return 0, service.ErrErrorDiagnosticUnavailable
	}
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM error_diagnostic_records
		WHERE diagnostic_id IN (
			SELECT diagnostic_id FROM error_diagnostic_records
			WHERE metadata_expires_at <= $1
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
