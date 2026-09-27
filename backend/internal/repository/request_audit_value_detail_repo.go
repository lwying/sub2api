package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Values are stored only after an audit row exists. New payloads are plaintext
// and usage-owned; legacy ciphertext keeps its original independent expiry.
type requestAuditValueDetailRepository struct {
	db     *sql.DB
	cipher service.RequestAuditValueDetailCipher
}

func NewRequestAuditValueDetailRepository(db *sql.DB, valueCipher service.RequestAuditValueDetailCipher) service.RequestAuditValueDetailRepository {
	return &requestAuditValueDetailRepository{db: db, cipher: valueCipher}
}

const requestAuditValueDetailColumns = `
	usage_log_id, state, reason, storage_format, route, protocol, client_status,
	attempt_count, entry_count, payload_bytes, key_version,
	(ciphertext IS NOT NULL OR plaintext_payload IS NOT NULL), started_at, completed_at, expires_at, created_at
`

const requestAuditValueDetailInsertStatement = `
	INSERT INTO request_audit_value_details (
		usage_log_id, state, reason, storage_format, route, protocol, client_status,
		attempt_count, entry_count, payload_bytes, key_version, ciphertext, plaintext_payload,
		started_at, completed_at, expires_at, created_at
	)
	SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
	WHERE EXISTS (SELECT 1 FROM request_audits WHERE usage_log_id = $1)
	ON CONFLICT (usage_log_id) DO NOTHING
	RETURNING created_at
`

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
		&detail.UsageLogID, &detail.State, &detail.Reason, &detail.StorageFormat, &detail.Fields.Route,
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

func (r *requestAuditValueDetailRepository) CreateRequestAuditValueDetail(ctx context.Context, write service.RequestAuditValueDetailWrite) (service.RequestAuditValueDetail, error) {
	if r == nil || r.db == nil {
		return service.RequestAuditValueDetail{}, service.ErrRequestAuditValueDetailUnavailable
	}
	if write.UsageLogID <= 0 {
		return service.RequestAuditValueDetail{}, service.ErrRequestAuditValueDetailNotFound
	}

	state, reason := write.State, write.Reason
	if state == "" {
		state = service.RequestAuditValueDetailStateNotObserved
	}
	if reason == "" {
		reason = service.RequestAuditValueDetailNotObserved
	}
	format := write.StorageFormat
	if format == "" {
		format = service.RequestAuditValueDetailStorageEncryptedV1
	}
	if format != service.RequestAuditValueDetailStoragePlaintextUsageBound && format != service.RequestAuditValueDetailStorageEncryptedV1 {
		return service.RequestAuditValueDetail{}, service.ErrRequestAuditValueDetailUnavailable
	}
	createdAt := time.Now().UTC()
	expiresAt := write.ExpiresAt.UTC()
	if format == service.RequestAuditValueDetailStorageEncryptedV1 {
		if !expiresAt.After(createdAt) {
			expiresAt = createdAt.Add(service.RequestAuditValueDetailRetention)
		}
	} else {
		expiresAt = time.Time{}
	}

	entryCount := write.EntryCount
	payloadBytes := len(write.Payload)
	keyVersion := 0
	var ciphertext, plaintext any
	if state == service.RequestAuditValueDetailStateStored {
		switch format {
		case service.RequestAuditValueDetailStoragePlaintextUsageBound:
			// 新明文的入站头值按入站路由推导出的协议复核；出站侧仍按逐次真实 wire 协议。
			// 路由不在闭集里时整份拒留（与 Build 的同源 fail closed 一致）。
			inboundProtocol, inboundSupported := service.RequestAuditValueDetailInboundProtocolForRoute(write.Fields.Route)
			if !inboundSupported || len(write.Payload) == 0 || len(write.Payload) > service.RequestAuditValueDetailMaxPayloadBytes {
				state, reason = service.RequestAuditValueDetailStateSkipped, service.RequestAuditValueDetailSkippedInvalidValues
			} else if _, err := service.DecodeRequestAuditValueDetailValuesForProtocols(write.Payload, inboundProtocol, write.Fields.Protocol); err != nil {
				state, reason = service.RequestAuditValueDetailStateSkipped, service.RequestAuditValueDetailSkippedInvalidValues
			} else {
				plaintext = append([]byte(nil), write.Payload...)
			}
		case service.RequestAuditValueDetailStorageEncryptedV1:
			if r.cipher == nil || len(write.Payload) == 0 {
				state, reason = service.RequestAuditValueDetailStateSkipped, service.RequestAuditValueDetailSkippedEncryptionUnavailable
			} else if encrypted, err := r.cipher.Encrypt(write.Payload); err != nil || len(encrypted) == 0 {
				state, reason = service.RequestAuditValueDetailStateSkipped, service.RequestAuditValueDetailSkippedEncryptionUnavailable
			} else {
				ciphertext = encrypted
				keyVersion = r.cipher.KeyVersion()
			}
		}
	}
	if state != service.RequestAuditValueDetailStateStored {
		entryCount, payloadBytes, keyVersion = 0, 0, 0
	}
	var startedAt, completedAt sql.NullTime
	if !write.Fields.StartedAt.IsZero() {
		startedAt = sql.NullTime{Time: write.Fields.StartedAt.UTC(), Valid: true}
	}
	if !write.Fields.CompletedAt.IsZero() {
		completedAt = sql.NullTime{Time: write.Fields.CompletedAt.UTC(), Valid: true}
	}
	var expiry any
	if format == service.RequestAuditValueDetailStorageEncryptedV1 {
		expiry = expiresAt
	}
	detail := service.RequestAuditValueDetail{
		UsageLogID: write.UsageLogID, State: state, Reason: reason, StorageFormat: format,
		Fields: write.Fields, Stored: ciphertext != nil || plaintext != nil, KeyVersion: keyVersion,
		AttemptCount: write.AttemptCount, EntryCount: entryCount, PayloadBytes: payloadBytes,
		ExpiresAt: expiresAt,
	}
	err := r.db.QueryRowContext(ctx, requestAuditValueDetailInsertStatement,
		detail.UsageLogID, detail.State, detail.Reason, detail.StorageFormat, detail.Fields.Route,
		detail.Fields.Protocol, detail.Fields.ClientStatus, detail.AttemptCount, detail.EntryCount,
		detail.PayloadBytes, detail.KeyVersion, ciphertext, plaintext, startedAt, completedAt,
		expiry, createdAt,
	).Scan(&detail.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return service.RequestAuditValueDetail{}, nil
	}
	if err != nil {
		return service.RequestAuditValueDetail{}, fmt.Errorf("create request audit value detail: %w", err)
	}
	return detail, nil
}

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

func (r *requestAuditValueDetailRepository) ReadRequestAuditValueDetailValues(ctx context.Context, usageLogID int64, now time.Time) (service.RequestAuditValueDetailValues, error) {
	if r == nil || r.db == nil {
		return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailUnavailable
	}
	if usageLogID <= 0 {
		return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailNotFound
	}
	var format, route, protocol string
	var ciphertext, plaintext []byte
	var expiresAt sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT storage_format, route, protocol, ciphertext, plaintext_payload, expires_at
		FROM request_audit_value_details WHERE usage_log_id = $1
	`, usageLogID).Scan(&format, &route, &protocol, &ciphertext, &plaintext, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailNotFound
		}
		return service.RequestAuditValueDetailValues{}, fmt.Errorf("read request audit value detail: %w", err)
	}
	var payload []byte
	switch format {
	case service.RequestAuditValueDetailStoragePlaintextUsageBound:
		if len(plaintext) == 0 || len(ciphertext) != 0 {
			return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailGone
		}
		payload = plaintext
	case service.RequestAuditValueDetailStorageEncryptedV1:
		if !expiresAt.Valid || !expiresAt.Time.After(now.UTC()) || len(ciphertext) == 0 {
			return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailGone
		}
		if r.cipher == nil {
			return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailUnavailable
		}
		payload, err = r.cipher.Decrypt(ciphertext)
		if err != nil || len(payload) == 0 {
			return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailGone
		}
	default:
		return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailUnavailable
	}
	// 入站头值按入站路由的协议复核（Chat Completions 入站 → Anthropic 出站这类转换里，
	// 入站 wire 与出站 wire 是两个闭集）。旧密文行在写下的那一刻还没有这个事实，
	// 因此保持原有「单协议」语义，不按路由重新解释历史行。
	inboundProtocol := protocol
	if format == service.RequestAuditValueDetailStoragePlaintextUsageBound {
		if derived, ok := service.RequestAuditValueDetailInboundProtocolForRoute(route); ok {
			inboundProtocol = derived
		}
	}
	values, err := service.DecodeRequestAuditValueDetailValuesForProtocols(payload, inboundProtocol, protocol)
	if err != nil {
		return service.RequestAuditValueDetailValues{}, service.ErrRequestAuditValueDetailGone
	}
	return values, nil
}

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
