package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 值明细旁路的存储实现：原生 SQL + database/sql。
//
// 该表由编号迁移（253）建立，不经过 Ent 生成代码，因此不进入 ent 事务。
//
// 两个不变量在本层强制：
//   - 「先有 request_audits 行」：写入是同一条语句里的 INSERT ... SELECT ... WHERE EXISTS
//     （request_audits.usage_log_id = 本次 usage_log_id），审计行不存在时一个字节都不写；
//   - 「值只以密文存在」：明文载荷只在本层与加密器之间传递，落库的只有密文与密钥代。
//
// 到期语义由本层与 7 天窗口共同保证：读取在 expires_at 时刻立即拒绝；
// 在线主库上的密文由周期清理置空（每轮有批量上限，停机或积压会推迟它）。
// 副本、备份与 PITR 的留存由部署方决定，这里不声称、也无法证明那些存储层的到期不可恢复。
type requestAuditValueDetailRepository struct {
	db     *sql.DB
	cipher service.RequestAuditValueDetailCipher
}

// NewRequestAuditValueDetailRepository 构造值明细仓储；cipher 可为 nil，此时值一律不可读、
// 也一律不留存（写入会退化成 skipped_encryption_unavailable）。
func NewRequestAuditValueDetailRepository(db *sql.DB, valueCipher service.RequestAuditValueDetailCipher) service.RequestAuditValueDetailRepository {
	return &requestAuditValueDetailRepository{db: db, cipher: valueCipher}
}

const requestAuditValueDetailColumns = `
	usage_log_id, state, reason, route, protocol, client_status,
	attempt_count, entry_count, payload_bytes, key_version,
	(ciphertext IS NOT NULL), started_at, completed_at, expires_at, created_at
`

// requestAuditValueDetailInsertStatement 是唯一的写入语句。
//
// EXISTS 守卫与插入在同一条语句里，因此「先有 request_audits 行」不存在竞态窗口；
// ON CONFLICT DO NOTHING 让重复写入成为幂等空操作，而不是覆盖已经留存的证据。
const requestAuditValueDetailInsertStatement = `
	INSERT INTO request_audit_value_details (
		usage_log_id, state, reason, route, protocol, client_status,
		attempt_count, entry_count, payload_bytes, key_version, ciphertext,
		started_at, completed_at, expires_at, created_at
	)
	SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
	WHERE EXISTS (SELECT 1 FROM request_audits WHERE usage_log_id = $1)
	ON CONFLICT (usage_log_id) DO NOTHING
	RETURNING created_at
`

// requestAuditValueDetailClearStatement 只清除已到期的密文列并记 purged。
//
// 它不删除整行：信封与 expires_at 保留，「曾留存、现已清除」才与「从未留存」可区分。
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

type requestAuditValueDetailRowScanner interface {
	Scan(dest ...any) error
}

func scanRequestAuditValueDetail(row requestAuditValueDetailRowScanner) (service.RequestAuditValueDetail, error) {
	var detail service.RequestAuditValueDetail
	var startedAt, completedAt, expiresAt sql.NullTime
	err := row.Scan(
		&detail.UsageLogID, &detail.State, &detail.Reason, &detail.Fields.Route,
		&detail.Fields.Protocol, &detail.Fields.ClientStatus,
		&detail.AttemptCount, &detail.EntryCount, &detail.PayloadBytes, &detail.KeyVersion,
		&detail.Stored, &startedAt, &completedAt, &expiresAt, &detail.CreatedAt,
	)
	if err != nil {
		return service.RequestAuditValueDetail{}, err
	}
	if startedAt.Valid {
		detail.Fields.StartedAt = startedAt.Time
	}
	if completedAt.Valid {
		detail.Fields.CompletedAt = completedAt.Time
	}
	if expiresAt.Valid {
		detail.ExpiresAt = expiresAt.Time
	}
	return detail, nil
}

// CreateRequestAuditValueDetail 写入值明细，且只在 request_audits 行已存在时真正写入。
//
// 加密失败一律退化为 skipped_encryption_unavailable：宁可留下「要求过留存但做不到」的
// 稳定事实，也绝不回退为明文。同一个 usage_log_id 已经有一行时不覆盖（ON CONFLICT DO NOTHING），
// 因此重复调用是幂等的，也不会把后来的值盖掉已经留存的证据。
func (r *requestAuditValueDetailRepository) CreateRequestAuditValueDetail(ctx context.Context, write service.RequestAuditValueDetailWrite) (service.RequestAuditValueDetail, error) {
	if r == nil || r.db == nil {
		return service.RequestAuditValueDetail{}, service.ErrRequestAuditValueDetailUnavailable
	}
	if write.UsageLogID <= 0 {
		return service.RequestAuditValueDetail{}, service.ErrRequestAuditValueDetailNotFound
	}

	state := write.State
	reason := write.Reason
	if state == "" {
		state = service.RequestAuditValueDetailStateNotObserved
	}
	if reason == "" {
		reason = service.RequestAuditValueDetailNotObserved
	}
	expiresAt := write.ExpiresAt.UTC()
	if expiresAt.IsZero() {
		expiresAt = time.Now().UTC().Add(service.RequestAuditValueDetailRetention)
	}
	createdAt := time.Now().UTC()
	if !expiresAt.After(createdAt) {
		// 迁移有 expires_at > created_at 的约束：把不可能的窗口收敛成合法值，
		// 而不是让一次写入失败。
		expiresAt = createdAt.Add(service.RequestAuditValueDetailRetention)
	}

	attemptCount := write.AttemptCount
	entryCount := write.EntryCount
	payloadBytes := len(write.Payload)
	var ciphertext any
	keyVersion := 0

	if state == service.RequestAuditValueDetailStateStored {
		if r.cipher == nil || len(write.Payload) == 0 {
			// 自称已留存却拿不出密文：落成「缺密钥的跳过」，绝不落成 stored。
			state = service.RequestAuditValueDetailStateSkipped
			reason = service.RequestAuditValueDetailSkippedEncryptionUnavailable
			payloadBytes = 0
			entryCount = 0
		} else {
			encrypted, err := r.cipher.Encrypt(write.Payload)
			if err != nil || len(encrypted) == 0 {
				state = service.RequestAuditValueDetailStateSkipped
				reason = service.RequestAuditValueDetailSkippedEncryptionUnavailable
				payloadBytes = 0
				entryCount = 0
			} else {
				ciphertext = encrypted
				keyVersion = r.cipher.KeyVersion()
			}
		}
	}
	if state != service.RequestAuditValueDetailStateStored {
		payloadBytes = 0
		keyVersion = 0
	}

	// started_at / completed_at 用 sql.NullTime：零值必须绑定成 SQL NULL，
	// 而不是公元 1 年，否则界面上会出现一个不存在的时刻。
	var startedAt, completedAt sql.NullTime
	if !write.Fields.StartedAt.IsZero() {
		startedAt = sql.NullTime{Time: write.Fields.StartedAt.UTC(), Valid: true}
	}
	if !write.Fields.CompletedAt.IsZero() {
		completedAt = sql.NullTime{Time: write.Fields.CompletedAt.UTC(), Valid: true}
	}

	detail := service.RequestAuditValueDetail{
		UsageLogID:   write.UsageLogID,
		State:        state,
		Reason:       reason,
		Fields:       write.Fields,
		Stored:       ciphertext != nil,
		KeyVersion:   keyVersion,
		AttemptCount: attemptCount,
		EntryCount:   entryCount,
		PayloadBytes: payloadBytes,
		ExpiresAt:    expiresAt,
	}

	// EXISTS 守卫是「先有审计行、再有值明细」这条不变量的唯一执行点：
	// 没有 request_audits 行时 SELECT 不产出任何行，因此一个字节都不会写入。
	err := r.db.QueryRowContext(ctx, requestAuditValueDetailInsertStatement,
		detail.UsageLogID, detail.State, detail.Reason, detail.Fields.Route,
		detail.Fields.Protocol, detail.Fields.ClientStatus,
		detail.AttemptCount, detail.EntryCount, detail.PayloadBytes, detail.KeyVersion,
		ciphertext, startedAt, completedAt, detail.ExpiresAt, createdAt,
	).Scan(&detail.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// 审计行不存在，或这一行已经存在：两者都是「这次没有新写入」，
		// 不是失败，也不该被当成写入成功。
		return service.RequestAuditValueDetail{}, nil
	}
	if err != nil {
		return service.RequestAuditValueDetail{}, fmt.Errorf("create request audit value detail: %w", err)
	}
	return detail, nil
}

// GetRequestAuditValueDetail 读取信封（不含明文值）。
func (r *requestAuditValueDetailRepository) GetRequestAuditValueDetail(ctx context.Context, usageLogID int64) (service.RequestAuditValueDetail, error) {
	if r == nil || r.db == nil {
		return service.RequestAuditValueDetail{}, service.ErrRequestAuditValueDetailUnavailable
	}
	if usageLogID <= 0 {
		return service.RequestAuditValueDetail{}, service.ErrRequestAuditValueDetailNotFound
	}
	detail, err := scanRequestAuditValueDetail(r.db.QueryRowContext(ctx,
		`SELECT `+requestAuditValueDetailColumns+` FROM request_audit_value_details WHERE usage_log_id = $1`, usageLogID))
	if errors.Is(err, sql.ErrNoRows) {
		return service.RequestAuditValueDetail{}, service.ErrRequestAuditValueDetailNotFound
	}
	if err != nil {
		return service.RequestAuditValueDetail{}, fmt.Errorf("get request audit value detail: %w", err)
	}
	return detail, nil
}

// ReadRequestAuditValueDetailValues 只在行仍持有**未到期**密文且本层持有密钥时才解密。
//
// 未留存、已物理清除、已到期、密钥缺失、认证失败或**解密结果不合格**统一返回
// ErrRequestAuditValueDetailGone，使读取结果不能作为「这条使用记录留过什么」的探针。
// 解密结果还要过一遍白名单与有界校验（DecodeRequestAuditValueDetailValues）：
// 解密成功不等于内容可信。
func (r *requestAuditValueDetailRepository) ReadRequestAuditValueDetailValues(ctx context.Context, usageLogID int64, now time.Time) (service.RequestAuditValueDetailValues, error) {
	if r == nil || r.db == nil {
		return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailUnavailable
	}
	if usageLogID <= 0 {
		return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailNotFound
	}
	var ciphertext []byte
	var expiresAt sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT ciphertext, expires_at FROM request_audit_value_details WHERE usage_log_id = $1
	`, usageLogID).Scan(&ciphertext, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailNotFound
		}
		return service.RequestAuditValueDetailValues{}, fmt.Errorf("read request audit value detail: %w", err)
	}
	if !expiresAt.Valid || !expiresAt.Time.After(now.UTC()) || len(ciphertext) == 0 {
		return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailGone
	}
	if r.cipher == nil {
		// 有密文却没有密钥（配置被移除）：这是可重试的部署故障，不是「值已消失」。
		return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailUnavailable
	}
	plaintext, err := r.cipher.Decrypt(ciphertext)
	if err != nil || len(plaintext) == 0 {
		return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailGone
	}
	values, err := service.DecodeRequestAuditValueDetailValues(plaintext)
	if err != nil {
		return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailGone
	}
	return values, nil
}

// ClearExpiredRequestAuditValueDetails 在在线主库物理清除已到第 7 天的值密文。
//
// 只置空密文列与密钥代并记 purged，保留整行信封与 expires_at，
// 使「曾留存、现已清除」与「从未留存」在状态上可区分。
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
