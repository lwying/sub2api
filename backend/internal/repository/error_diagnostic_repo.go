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
	header_entry_count, header_expires_at,
	plain_record, plain_owner_usage_log_id,
	(plain_body_payload IS NOT NULL), plain_body_state, plain_body_reason, plain_body_bytes,
	(plain_header_payload IS NOT NULL), plain_header_state, plain_header_reason,
	plain_header_bytes, plain_header_entry_count
`

type errorDiagnosticRowScanner interface {
	Scan(dest ...any) error
}

func scanErrorDiagnosticRecord(row errorDiagnosticRowScanner) (service.ErrorDiagnosticRecord, error) {
	var record service.ErrorDiagnosticRecord
	var usageLogID sql.NullInt64
	var bodyExpiresAt sql.NullTime
	var headerExpiresAt sql.NullTime
	var plainOwner sql.NullInt64
	err := row.Scan(
		&record.ID, &usageLogID, &record.Protocol, &record.AttemptIndex, &record.Stage,
		&record.UpstreamStatusCode, &record.BodyState, &record.BodyReason, &record.BodyStored,
		&record.BodyBytes, &record.BodyKeyVersion, &record.CreatedAt, &record.MetadataExpiresAt,
		&bodyExpiresAt,
		&record.HeaderState, &record.HeaderReason, &record.HeaderStored, &record.HeaderBytes,
		&record.HeaderKeyVersion, &record.HeaderEntryCount, &headerExpiresAt,
		&record.PlainRecord, &plainOwner,
		&record.PlainBodyStored, &record.PlainBodyState, &record.PlainBodyReason, &record.PlainBodyBytes,
		&record.PlainHeaderStored, &record.PlainHeaderState, &record.PlainHeaderReason,
		&record.PlainHeaderBytes, &record.PlainHeaderEntryCount,
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
	// 新明文行的「有关联使用记录」由**所有者**列回答，而不是旧的可空 usage_log_id：
	// 旧列的 SET NULL 语义与新层的级联所有权是两件事，混用会把「曾经关联过、现已被置空」
	// 显示成仍然有人拥有它。有一个就披露一个，两个都有时以新层的所有者为准。
	if plainOwner.Valid {
		record.PlainLinked = true
		record.PlainOwnerUsageLogID = plainOwner.Int64
		record.UsageLogID = plainOwner.Int64
		record.HasUsage = true
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
	if write.Attempt.UsageLogID > 0 && !write.PlainRecord {
		// 新明文行的所有权只由 plain_owner_usage_log_id（可验证关联后写入、级联删除）表达：
		// 存储层的互斥约束不允许一行同时带旧的可空关联与新层的所有者，而且旧列是 SET NULL
		// 语义，用它当所有人的话「用了这条 usage 的明文」会活得比 usage 还久。
		// 写入时所有者必为空（此刻还没有验证过任何关联），因此明文行一律不带旧关联。
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

	plain := newPlainDiagnosticColumns(write)
	record.PlainRecord = plain.record
	record.PlainBodyState = plain.bodyState
	record.PlainBodyReason = plain.bodyReason
	record.PlainBodyBytes = plain.bodyBytes
	record.PlainBodyStored = plain.bodyPayload != nil
	record.PlainHeaderState = plain.headerState
	record.PlainHeaderReason = plain.headerReason
	record.PlainHeaderBytes = plain.headerBytes
	record.PlainHeaderEntryCount = plain.headerEntryCount
	record.PlainHeaderStored = plain.headerPayload != nil

	err := r.db.QueryRowContext(ctx, `
		INSERT INTO error_diagnostic_records (
			diagnostic_id, usage_log_id, protocol, attempt_index, stage, upstream_status,
			body_state, body_reason, body_ciphertext, body_key_version, body_bytes,
			created_at, metadata_expires_at, body_expires_at,
			header_state, header_reason, header_ciphertext, header_key_version, header_bytes,
			header_entry_count, header_expires_at,
			plain_record, plain_owner_usage_log_id, plain_link_digest,
			plain_link_attempt_index, plain_link_wire_status,
			plain_body_state, plain_body_reason, plain_body_payload, plain_body_bytes,
			plain_header_state, plain_header_reason, plain_header_payload,
			plain_header_bytes, plain_header_entry_count
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21,
			$22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35)
		RETURNING created_at
	`,
		record.ID, usageLogID, record.Protocol, record.AttemptIndex, record.Stage, record.UpstreamStatusCode,
		record.BodyState, record.BodyReason, ciphertext, bodyKeyVersion, bodyBytes,
		now, metadataExpiresAt, bodyExpiresAt,
		headerState, headerReason, headerCiphertextParam, headerKeyVersion, headerPayloadBytes,
		headerEntryCount, headerExpiresAt,
		plain.record, plain.ownerUsageLogID, plain.linkDigest,
		plain.linkAttemptIndex, plain.linkWireStatus,
		plain.bodyState, plain.bodyReason, plain.bodyPayload, plain.bodyBytes,
		plain.headerState, plain.headerReason, plain.headerPayload,
		plain.headerBytes, plain.headerEntryCount,
	).Scan(&record.CreatedAt)
	if err != nil {
		return service.ErrorDiagnosticRecord{}, fmt.Errorf("create error diagnostic: %w", err)
	}
	return record, nil
}

// plainDiagnosticMaxHeaderEntryCount 是存储层对明文头值条数的硬上限
// （见迁移 255 的 error_diagnostic_plain_header_pair）。
const plainDiagnosticMaxHeaderEntryCount = 64

// plainDiagnosticColumns 是一次插入要绑定到新明文列上的值。
//
// 每个字段都必须是**已收敛**的：落库值不满足存储层封闭集合时，宁可把该层记成未采集，
// 也不能让整次写入失败——诊断写入是 fail-open 的旁路，一条约束冲突会把整条观察丢掉，
// 而「丢整行」比「少一层」严重得多。
type plainDiagnosticColumns struct {
	record           bool
	ownerUsageLogID  any
	linkDigest       any
	linkAttemptIndex any
	linkWireStatus   any
	bodyState        string
	bodyReason       string
	bodyPayload      any
	bodyBytes        int
	headerState      string
	headerReason     string
	headerPayload    any
	headerBytes      int
	headerEntryCount int
}

// newPlainDiagnosticColumns 把服务侧的写入决定收敛成可落库的明文列。
//
// 旧格式的行（PlainRecord=false）在这里落成「明文列全部未采集」：它们不是新格式，
// 因此新列一个事实都不冒充，读取侧据 plain_record 走旧路径。
func newPlainDiagnosticColumns(write service.ErrorDiagnosticWrite) plainDiagnosticColumns {
	columns := plainDiagnosticColumns{
		record:       write.PlainRecord,
		bodyState:    service.ErrorDiagnosticBodyStateNotObserved,
		bodyReason:   service.ErrorDiagnosticBodyNotObserved,
		headerState:  service.ErrorDiagnosticHeaderStateNotObserved,
		headerReason: service.ErrorDiagnosticHeaderNotObserved,
	}
	if !write.PlainRecord {
		return columns
	}
	columns.ownerUsageLogID = nil
	// 关联三元组只在「可验证的关联摘要 + 在界内的真实序号与状态」同时成立时写入。
	// 存储层要求三者同进同出，缺一个就整组留空：那一行因此永远不可关联，只能按三十天截止，
	// 而不是拿一个兜底推断的序号去猜一条关联。
	if len(write.PlainLinkDigest) == 64 &&
		write.PlainLinkAttemptIndex >= 1 && write.PlainLinkAttemptIndex <= service.ErrorDiagnosticMaxAttemptIndex &&
		write.PlainLinkWireStatus >= 400 && write.PlainLinkWireStatus <= 599 {
		columns.linkDigest = write.PlainLinkDigest
		columns.linkAttemptIndex = write.PlainLinkAttemptIndex
		columns.linkWireStatus = write.PlainLinkWireStatus
	}

	columns.bodyState, columns.bodyReason, columns.bodyPayload, columns.bodyBytes = normalizePlainBodyColumns(
		write.PlainBodyState, write.PlainBodyReason, write.PlainBodyPayload)
	columns.headerState, columns.headerReason, columns.headerPayload, columns.headerBytes, columns.headerEntryCount =
		normalizePlainHeaderColumns(write.PlainHeaderState, write.PlainHeaderReason, write.PlainHeaderPayload, write.PlainHeaderEntryCount)
	return columns
}

// normalizePlainBodyColumns 收敛新明文正文列的落库值。
func normalizePlainBodyColumns(state, reason string, payload []byte) (string, string, any, int) {
	if state != service.ErrorDiagnosticBodyStateStored {
		switch state {
		case service.ErrorDiagnosticBodyStateNotObserved, service.ErrorDiagnosticBodyStateSkipped:
			return state, plainDiagnosticAllowedBodyReason(reason), nil, 0
		default:
			// 未知状态：按未采集落库，绝不自称已留存。
			return service.ErrorDiagnosticBodyStateNotObserved, service.ErrorDiagnosticBodyNotObserved, nil, 0
		}
	}
	if len(payload) == 0 || len(payload) > service.ErrorDiagnosticMaxBodyBytes {
		return service.ErrorDiagnosticBodyStateSkipped, service.ErrorDiagnosticBodySkippedTooLarge, nil, 0
	}
	if reason != service.ErrorDiagnosticPlainBodyRetained {
		// 自称已留存却带着另一个原因码：这不是可解释的组合，按未采集处理。
		return service.ErrorDiagnosticBodyStateNotObserved, service.ErrorDiagnosticBodyNotObserved, nil, 0
	}
	return service.ErrorDiagnosticBodyStateStored, service.ErrorDiagnosticPlainBodyRetained, payload, len(payload)
}

// normalizePlainHeaderColumns 收敛新明文 429 头值列的落库值。
func normalizePlainHeaderColumns(state, reason string, payload []byte, entryCount int) (string, string, any, int, int) {
	if state != service.ErrorDiagnosticHeaderStateStored {
		switch state {
		case service.ErrorDiagnosticHeaderStateNotObserved, service.ErrorDiagnosticHeaderStateSkipped:
			return state, plainDiagnosticAllowedHeaderReason(reason), nil, 0, 0
		default:
			return service.ErrorDiagnosticHeaderStateNotObserved, service.ErrorDiagnosticHeaderNotObserved, nil, 0, 0
		}
	}
	// 条数上限与存储层的互斥约束一致：超出即整层判为不合格（绝不部分写入）。
	if len(payload) == 0 || len(payload) > service.ErrorDiagnosticMaxHeaderPayloadBytes ||
		entryCount <= 0 || entryCount > plainDiagnosticMaxHeaderEntryCount {
		return service.ErrorDiagnosticHeaderStateSkipped, service.ErrorDiagnosticHeaderSkippedInvalidValues, nil, 0, 0
	}
	if reason != service.ErrorDiagnosticPlainHeaderRetained {
		return service.ErrorDiagnosticHeaderStateNotObserved, service.ErrorDiagnosticHeaderNotObserved, nil, 0, 0
	}
	return service.ErrorDiagnosticHeaderStateStored, service.ErrorDiagnosticPlainHeaderRetained, payload, len(payload), entryCount
}

// plainDiagnosticAllowedBodyReason 只回声新明文正文列的封闭原因码集合；
// 未知值按未采集处理，不回显任意字符串（否则约束会拒绝整次插入）。
func plainDiagnosticAllowedBodyReason(reason string) string {
	switch reason {
	case service.ErrorDiagnosticBodySkippedNotTextJSON,
		service.ErrorDiagnosticBodySkippedTooLarge,
		service.ErrorDiagnosticBodySkippedAttachment,
		service.ErrorDiagnosticBodySkippedKnownCredential,
		service.ErrorDiagnosticBodySkippedIncompleteRead,
		service.ErrorDiagnosticBodySkippedRetentionDisabled:
		return reason
	default:
		return service.ErrorDiagnosticBodyNotObserved
	}
}

// plainDiagnosticAllowedHeaderReason 只回声新明文头值列的封闭原因码集合。
func plainDiagnosticAllowedHeaderReason(reason string) string {
	switch reason {
	case service.ErrorDiagnosticHeaderSkippedRetentionDisabled, service.ErrorDiagnosticHeaderSkippedInvalidValues:
		return reason
	default:
		return service.ErrorDiagnosticHeaderNotObserved
	}
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
	// 两条所有权列都要匹配：旧列是可空的 SET NULL 关联，新明文层用 plain_owner_usage_log_id
	// （级联所有权）。只查旧列会让「这次失败尝试属于这条使用记录」的明文行彻底看不见。
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+errorDiagnosticRecordColumns+`
		FROM error_diagnostic_records
		WHERE usage_log_id = $1 OR plain_owner_usage_log_id = $1
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
//
// 已关联 usage 的新明文行不受三十天约束（它们随 usage 存在），因此这个谓词必须显式放行它们：
// 否则一条仍然有人拥有、仍然可读的明文诊断会在第三十天从管理端消失。
func (r *errorDiagnosticRepository) ListRecentErrorDiagnosticPage(ctx context.Context, protocol string, now time.Time, offset, limit int) ([]service.ErrorDiagnosticRecord, error) {
	if r == nil || r.db == nil {
		return nil, service.ErrErrorDiagnosticUnavailable
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+errorDiagnosticRecordColumns+`
		FROM error_diagnostic_records
		WHERE (metadata_expires_at > $1 OR (plain_record AND plain_owner_usage_log_id IS NOT NULL))
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
//
// 谓词必须与分页查询逐字一致（含「已关联明文行不受三十天约束」这一条），
// 否则 total 与 items 会各说各话。
func (r *errorDiagnosticRepository) CountRecentErrorDiagnostics(ctx context.Context, protocol string, now time.Time) (int64, error) {
	if r == nil || r.db == nil {
		return 0, service.ErrErrorDiagnosticUnavailable
	}
	var count int64
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM error_diagnostic_records
		WHERE (metadata_expires_at > $1 OR (plain_record AND plain_owner_usage_log_id IS NOT NULL))
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

	// 新明文层的积压单独观测：它们是**明文**残留，与旧密文残留的处置不同，
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
// **已关联的新明文行不在范围内**：它们的所有者是使用记录，三十天只是「未关联时」的读取
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
