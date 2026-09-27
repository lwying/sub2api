//go:build integration

package repository

// 迁移 254「新值明细明文、usage-owned」在真实 PostgreSQL 上的验收（ADR 0007）。
//
// 253 是冻结的旧格式：密文载荷 + `expires_at TIMESTAMPTZ NOT NULL DEFAULT (NOW() + 7 days)`，
// 并有 stored_requires_ciphertext／expires_after_creation／reason_allowed（7 值）三条 CHECK。
// 254 只追加新格式，把这三条换成覆盖两种格式的 storage_pairing／storage_format_allowed／
// reason_allowed（8 值），并按 `storage_format` 分流。
//
// 这里要证明四件事，它们都是「约束行为」而不是「文件里出现了某段文本」：
//
//	1. 旧行（253 形状）在新 CHECK 下仍然合法。NOT VALID 只豁免「加锁时全表扫描」，不是
//	   「旧行可以违规」：交给 PostgreSQL 自己跑 VALIDATE CONSTRAINT，任何一条违规旧行都会被拒。
//	2. 新写入被强制执行。被替换掉的旧约束（自称 stored 必须有密文、到期必须晚于创建、
//	   原因码闭集）必须在新谓词里继续生效，明文格式自己的字段配对也必须生效。
//	3. 历史清理路径不被新约束打断：253 的清理 UPDATE（置空密文、记 purged、保留 expires_at）
//	   对旧行必须仍然可执行，否则升级后旧密文永远清不掉。
//	4. 迁移可重放。运行器按 checksum 跳过已应用的迁移，但灾备重放／手工重放会重新执行同一份
//	   SQL；重放不得因对象已存在而失败（migrations/README.md 的幂等要求，253／255／256 都遵守）。
//
// 隔离方式：每条测试在自己的临时 schema 内重放 253／254 的真实嵌入 SQL 文件，search_path 只指向
// 该 schema，既不触碰 public 的在线表，也不修改任何既有数据；整个 schema 随测试事务一起回滚。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const (
	valueDetailLegacyMigration    = "253_request_audit_value_details.sql"
	valueDetailPlaintextMigration = "254_request_audit_value_details_plaintext.sql"

	valueDetailTable = "request_audit_value_details"

	valueDetailPairingConstraint    = "request_audit_value_details_storage_pairing"
	valueDetailFormatConstraint     = "request_audit_value_details_storage_format_allowed"
	valueDetailReasonConstraint     = "request_audit_value_details_reason_allowed"
	valueDetailCiphertextConstraint = "request_audit_value_details_stored_requires_ciphertext"
	valueDetailExpiryConstraint     = "request_audit_value_details_expires_after_creation"
	legacyValueDetailReasonSet      = "('not_observed','retained','skipped_out_of_scope','skipped_value_retention_disabled','skipped_encryption_unavailable','skipped_invalid_values','skipped_too_many_attempts')"
	legacyStoredRequiresCiphertext  = "state <> 'stored' OR ciphertext IS NOT NULL"
	legacyExpiresAfterCreation      = "expires_at > created_at"
	valueDetailCiphertextSample     = "legacy-aes-gcm-ciphertext"
	// 新格式的载荷是明文 JSON（有界、经服务层复核），这里用真实字节而不是 SQL 字面量。
	valueDetailPlaintextPayload  = `{"model":"claude-sonnet-4-5"}`
	expectedPostgresCheckFailure = pq.ErrorCode("23514")
)

// --- 测试用例 --------------------------------------------------------------

// TestPlaintextValueDetailMigration254KeepsLegacyRowsReadableAndEnforcesNewWrites 是 254 的核心验收：
// 旧行原样保留、旧清理路径仍可执行、新格式可写、新写入被强制执行。
func TestPlaintextValueDetailMigration254KeepsLegacyRowsReadableAndEnforcesNewWrites(t *testing.T) {
	ctx := context.Background()
	tx, schema := newIsolatedValueDetailSchema(t)

	seedLegacyValueDetailSchema(t, tx, schema)

	// 253 形状的旧行：stored／expired 持密文，purged／skipped 无密文；expires_at 走 253 的
	// NOT NULL DEFAULT，因此这里刻意不显式赋值，让旧默认值成为被观察的事实。
	storedID := newLegacyValueDetailRow(t, tx, "stored", "retained", []byte(valueDetailCiphertextSample))
	expiredID := newLegacyValueDetailRow(t, tx, "expired", "retained", []byte(valueDetailCiphertextSample))
	purgedID := newLegacyValueDetailRow(t, tx, "purged", "retained", nil)
	skippedID := newLegacyValueDetailRow(t, tx, "skipped", "skipped_value_retention_disabled", nil)

	before := map[int64]legacyValueDetailSnapshot{
		storedID:  readLegacyValueDetailSnapshot(t, tx, storedID),
		expiredID: readLegacyValueDetailSnapshot(t, tx, expiredID),
		purgedID:  readLegacyValueDetailSnapshot(t, tx, purgedID),
		skippedID: readLegacyValueDetailSnapshot(t, tx, skippedID),
	}
	for id, snapshot := range before {
		require.True(t, snapshot.expiresAt.Valid, "旧行 %d 必须带 253 的 7 天到期时刻", id)
	}

	applyValueDetailPlaintextMigration(t, tx)

	// 1. 旧行逐列原样：格式被回填成 encrypted_v1，而明文列必须为 NULL，到期／密文／状态／原因不变。
	for id, snapshot := range before {
		after := readValueDetailSnapshotAfterMigration(t, tx, id)
		require.Equal(t, "encrypted_v1", after.storageFormat, "旧行 %d 必须被判定为旧格式", id)
		require.False(t, after.plaintextPresent, "旧行 %d 不得凭空获得明文载荷", id)
		require.Equal(t, snapshot, after.withoutStorageFormat(), "旧行 %d 的既有列不得被改写", id)
	}

	// 2. NOT VALID 是加锁手段，不是「旧行可以违规」：让数据库自己校验全部旧行。
	for _, constraint := range []string{
		valueDetailPairingConstraint,
		valueDetailFormatConstraint,
		valueDetailReasonConstraint,
	} {
		requireConstraintNotValidated(t, tx, schema, valueDetailTable, constraint)
		validateValueDetailConstraint(t, tx, schema, valueDetailTable, constraint)
	}

	// 3. 被替换掉的旧约束确实不存在了（它们的语义由 storage_pairing 承担）。
	for _, dropped := range []string{valueDetailCiphertextConstraint, valueDetailExpiryConstraint} {
		requireConstraintAbsent(t, tx, schema, valueDetailTable, dropped)
	}

	// 4. ADD COLUMN ... NOT NULL DEFAULT 必须走 fast default（无表重写），否则加锁期间会重建整表。
	requireColumnAddedAsFastDefault(t, tx, schema, valueDetailTable, "storage_format")

	// 5. 新格式可写：明文载荷 + 无密文 + key_version 0 + expires_at NULL（不再有独立截止时刻）。
	plaintextID := newUsageLogForValueDetail(t, tx)
	_, err := tx.ExecContext(ctx, `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, storage_format, route, protocol, client_status,
     attempt_count, entry_count, payload_bytes, key_version, ciphertext, plaintext_payload,
     started_at, completed_at, expires_at, created_at)
VALUES ($1, 'stored', 'retained', 'plaintext_usage_bound', '/v1/messages', 'anthropic.messages', 200,
        1, 1, $3, 0, NULL, $2, NOW(), NOW(), NULL, NOW())
`, plaintextID, []byte(valueDetailPlaintextPayload), len(valueDetailPlaintextPayload))
	require.NoError(t, err, "新明文值明细必须可写")

	var (
		insertedFormat    string
		insertedPayload   []byte
		insertedHasExpiry bool
	)
	require.NoError(t, scanSingleRow(ctx, tx, `
SELECT storage_format, plaintext_payload, (expires_at IS NOT NULL)
FROM request_audit_value_details WHERE usage_log_id = $1
`, []any{plaintextID}, &insertedFormat, &insertedPayload, &insertedHasExpiry))
	require.Equal(t, "plaintext_usage_bound", insertedFormat)
	require.JSONEq(t, valueDetailPlaintextPayload, string(insertedPayload))
	require.False(t, insertedHasExpiry, "新明文行不得带独立到期时刻")

	// 明文格式的「未留存」行同样可写：只有状态与原因，没有载荷。
	skippedPlaintextID := newUsageLogForValueDetail(t, tx)
	_, err = tx.ExecContext(ctx, `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, storage_format, route, protocol, attempt_count,
     entry_count, payload_bytes, key_version, expires_at)
VALUES ($1, 'skipped', 'skipped_value_retention_disabled', 'plaintext_usage_bound', '/v1/messages', '', 0,
        0, 0, 0, NULL)
`, skippedPlaintextID)
	require.NoError(t, err, "明文格式的 skipped 行必须可写")

	// 6. 新写入被强制执行：每一条都应被 CHECK 拒绝（SQLSTATE 23514）。
	for _, tc := range valueDetailEnforcementCases() {
		t.Run(tc.name, func(t *testing.T) {
			usageLogID := newUsageLogForValueDetail(t, tx)
			expectPostgresError(t, tx, expectedPostgresCheckFailure, tc.statement,
				append([]any{usageLogID}, tc.args...)...)
		})
	}

	// 7. 历史清理路径：253 的清理 UPDATE（置空密文、key_version 归零、记 purged、保留 expires_at）
	//    必须仍然可执行，否则旧密文在升级后既清不掉也读不了。
	for _, id := range []int64{storedID, expiredID} {
		_, err := tx.ExecContext(ctx, `
UPDATE request_audit_value_details
SET ciphertext = NULL, key_version = 0, state = 'purged'
WHERE id = $1 AND ciphertext IS NOT NULL
`, id)
		require.NoErrorf(t, err, "旧行 %d 的 7 天清理必须仍然可执行", id)

		after := readValueDetailSnapshotAfterMigration(t, tx, id)
		require.Equal(t, "purged", after.state)
		require.False(t, after.ciphertextPresent, "清理后密文必须被置空")
		require.True(t, after.expiresAt.Valid, "清理只置空密文，到期时刻作为「曾留存」的证据保留")
		require.False(t, after.plaintextPresent, "旧行清理不得写出明文载荷")
	}
}

// TestPlaintextValueDetailMigration254IsReplaySafe 证明同一份 254 SQL 可以被重放：
// 灾备重放不会因「约束已存在」失败，且重放后约束语义保持一致。
func TestPlaintextValueDetailMigration254IsReplaySafe(t *testing.T) {
	ctx := context.Background()
	tx, schema := newIsolatedValueDetailSchema(t)

	seedLegacyValueDetailSchema(t, tx, schema)
	legacyID := newLegacyValueDetailRow(t, tx, "stored", "retained", []byte(valueDetailCiphertextSample))

	applyValueDetailPlaintextMigration(t, tx)

	// 重放：把同一份迁移文件再执行一次，不得报「constraint ... already exists」。
	_, err := tx.ExecContext(ctx, valueDetailMigrationText(t, valueDetailPlaintextMigration))
	require.NoError(t, err, "迁移 254 必须可重放（幂等），否则灾备重放会失败")

	// 重放后旧行仍在，且三条新约束仍然生效。
	after := readValueDetailSnapshotAfterMigration(t, tx, legacyID)
	require.Equal(t, "encrypted_v1", after.storageFormat)
	require.True(t, after.expiresAt.Valid)

	for _, constraint := range []string{
		valueDetailPairingConstraint,
		valueDetailFormatConstraint,
		valueDetailReasonConstraint,
	} {
		requireConstraintExists(t, tx, schema, valueDetailTable, constraint)
		requireConstraintNotValidated(t, tx, schema, valueDetailTable, constraint)
		validateValueDetailConstraint(t, tx, schema, valueDetailTable, constraint)
	}

	unknownFormatID := newUsageLogForValueDetail(t, tx)
	expectPostgresError(t, tx, expectedPostgresCheckFailure, `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, storage_format, expires_at)
VALUES ($1, 'not_observed', 'not_observed', 'plaintext_v2', NULL)
`, unknownFormatID)

	// 重放不得把明文格式的原因码集合收窄回 7 值集合。
	unsupportedReasonID := newUsageLogForValueDetail(t, tx)
	_, err = tx.ExecContext(ctx, `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, storage_format, route, protocol, attempt_count,
     entry_count, payload_bytes, key_version, expires_at)
VALUES ($1, 'skipped', 'skipped_unsupported_protocol', 'plaintext_usage_bound', '/v1/messages', '', 0,
        0, 0, 0, NULL)
`, unsupportedReasonID)
	require.NoError(t, err, "重放后 skipped_unsupported_protocol 必须仍在原因码闭集内")
}

// --- 写入强制执行的用例 ------------------------------------------------------

type valueDetailEnforcementCase struct {
	name      string
	statement string
	args      []any
}

// valueDetailEnforcementCases 覆盖「被替换掉的旧约束仍生效」与「明文格式字段配对生效」两类。
// 每条语句固定 $1 = usage_log_id，其余参数由 args 提供。
func valueDetailEnforcementCases() []valueDetailEnforcementCase {
	return []valueDetailEnforcementCase{
		{
			name: "encrypted without expires_at is rejected",
			statement: `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, route, protocol, expires_at)
VALUES ($1, 'not_observed', 'not_observed', '/v1/messages', '', NULL)`,
		},
		{
			name: "encrypted stored without ciphertext is rejected",
			statement: `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, route, protocol, expires_at)
VALUES ($1, 'stored', 'retained', '/v1/messages', '', NOW() + INTERVAL '7 days')`,
		},
		{
			name: "encrypted expires_at not after created_at is rejected",
			statement: `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, route, protocol, ciphertext, expires_at, created_at)
VALUES ($1, 'stored', 'retained', '/v1/messages', '', 'x'::bytea, NOW(), NOW() + INTERVAL '1 day')`,
		},
		{
			name: "encrypted must not carry a plaintext payload",
			statement: `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, storage_format, route, protocol, ciphertext, plaintext_payload, expires_at)
VALUES ($1, 'stored', 'retained', 'encrypted_v1', '/v1/messages', '', 'x'::bytea, 'y'::bytea, NOW() + INTERVAL '7 days')`,
		},
		{
			name: "plaintext must not carry ciphertext",
			statement: `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, storage_format, route, protocol, key_version, ciphertext, plaintext_payload, expires_at)
VALUES ($1, 'stored', 'retained', 'plaintext_usage_bound', '/v1/messages', '', 0, 'x'::bytea, 'y'::bytea, NULL)`,
		},
		{
			name: "plaintext must not carry a key version",
			statement: `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, storage_format, route, protocol, key_version, plaintext_payload, expires_at)
VALUES ($1, 'stored', 'retained', 'plaintext_usage_bound', '/v1/messages', '', 1, 'y'::bytea, NULL)`,
		},
		{
			name: "plaintext must not carry an independent expiry",
			statement: `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, storage_format, route, protocol, key_version, plaintext_payload, expires_at)
VALUES ($1, 'stored', 'retained', 'plaintext_usage_bound', '/v1/messages', '', 0, 'y'::bytea, NOW() + INTERVAL '7 days')`,
		},
		{
			name: "plaintext stored without payload is rejected",
			statement: `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, storage_format, route, protocol, key_version, expires_at)
VALUES ($1, 'stored', 'retained', 'plaintext_usage_bound', '/v1/messages', '', 0, NULL)`,
		},
		{
			name: "plaintext payload outside stored state is rejected",
			statement: `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, storage_format, route, protocol, key_version, plaintext_payload, expires_at)
VALUES ($1, 'skipped', 'skipped_value_retention_disabled', 'plaintext_usage_bound', '/v1/messages', '', 0, 'y'::bytea, NULL)`,
		},
		{
			name: "unknown storage format is rejected",
			statement: `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, storage_format, route, protocol, expires_at)
VALUES ($1, 'not_observed', 'not_observed', 'plaintext_v2', '/v1/messages', '', NULL)`,
		},
		{
			name: "reason outside the closed set is rejected",
			statement: `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, storage_format, route, protocol, expires_at)
VALUES ($1, 'skipped', 'skipped_because_it_looked_odd', 'plaintext_usage_bound', '/v1/messages', '', NULL)`,
		},
	}
}

// --- fixture 与断言 helpers ------------------------------------------------

type legacyValueDetailSnapshot struct {
	state             string
	reason            string
	storageFormat     string
	ciphertextPresent bool
	plaintextPresent  bool
	expiresAt         sql.NullTime
}

// withoutStorageFormat 用于比较 254 之前的快照：storage_format 与明文列在 254 之前还不存在。
func (s legacyValueDetailSnapshot) withoutStorageFormat() legacyValueDetailSnapshot {
	s.storageFormat = ""
	return s
}

// readLegacyValueDetailSnapshot 在 254 之前读取：此时还没有 storage_format／plaintext_payload。
func readLegacyValueDetailSnapshot(t *testing.T, tx *sql.Tx, usageLogID int64) legacyValueDetailSnapshot {
	t.Helper()

	var snapshot legacyValueDetailSnapshot
	require.NoError(t, scanSingleRow(context.Background(), tx, `
SELECT state, reason, (ciphertext IS NOT NULL), expires_at
FROM request_audit_value_details WHERE usage_log_id = $1
`, []any{usageLogID},
		&snapshot.state, &snapshot.reason,
		&snapshot.ciphertextPresent, &snapshot.expiresAt))
	return snapshot
}

// readValueDetailSnapshotAfterMigration 在 254 之后读取：额外观察格式判别与明文列。
func readValueDetailSnapshotAfterMigration(t *testing.T, tx *sql.Tx, usageLogID int64) legacyValueDetailSnapshot {
	t.Helper()

	var snapshot legacyValueDetailSnapshot
	require.NoError(t, scanSingleRow(context.Background(), tx, `
SELECT state, reason, storage_format,
       (ciphertext IS NOT NULL), (plaintext_payload IS NOT NULL), expires_at
FROM request_audit_value_details WHERE usage_log_id = $1
`, []any{usageLogID},
		&snapshot.state, &snapshot.reason, &snapshot.storageFormat,
		&snapshot.ciphertextPresent, &snapshot.plaintextPresent, &snapshot.expiresAt))
	return snapshot
}

// newIsolatedValueDetailSchema 创建只属于本测试的 schema，并把 search_path 固定到它，
// 使 253／254 里所有未限定的表名都落在隔离 schema 内，绝不触碰 public 的在线表。
func newIsolatedValueDetailSchema(t *testing.T) (*sql.Tx, string) {
	t.Helper()

	ctx := context.Background()
	tx := testTx(t)
	schema := fmt.Sprintf("mig254_it_%d", time.Now().UnixNano())

	_, err := tx.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err, "create isolated schema")
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+schema)
	require.NoError(t, err, "pin search_path to the isolated schema")

	// 253 的外键指向 usage_logs：只需主键形状，用于放置旧行。
	_, err = tx.ExecContext(ctx, "CREATE TABLE usage_logs (id BIGSERIAL PRIMARY KEY)")
	require.NoError(t, err, "create isolated usage_logs")

	return tx, schema
}

// seedLegacyValueDetailSchema 在隔离 schema 内重放 253 的真实 SQL，并补回 253 的 DO 块在
// 共享测试库中会跳过的三条旧约束（253 的守卫按 conname 全局查询，public 上已有同名约束，
// 因此在这里会被跳过）。补齐后该表的形状与约束就是 253 在生产上的真实结果。
func seedLegacyValueDetailSchema(t *testing.T, tx *sql.Tx, schema string) {
	t.Helper()

	_, err := tx.ExecContext(context.Background(), valueDetailMigrationText(t, valueDetailLegacyMigration))
	require.NoError(t, err, "apply migration 253")

	ensureLocalCheckConstraint(t, tx, schema, valueDetailTable, valueDetailCiphertextConstraint, legacyStoredRequiresCiphertext)
	ensureLocalCheckConstraint(t, tx, schema, valueDetailTable, valueDetailExpiryConstraint, legacyExpiresAfterCreation)
	ensureLocalCheckConstraint(t, tx, schema, valueDetailTable, valueDetailReasonConstraint,
		"reason IN "+legacyValueDetailReasonSet)
}

func ensureLocalCheckConstraint(t *testing.T, tx *sql.Tx, schema, table, name, predicate string) {
	t.Helper()

	if constraintExists(t, tx, schema, table, name) {
		return
	}

	_, err := tx.ExecContext(context.Background(),
		fmt.Sprintf("ALTER TABLE %s.%s ADD CONSTRAINT %s CHECK (%s)", schema, table, name, predicate))
	require.NoErrorf(t, err, "add legacy constraint %s", name)
}

func applyValueDetailPlaintextMigration(t *testing.T, tx *sql.Tx) {
	t.Helper()

	_, err := tx.ExecContext(context.Background(), valueDetailMigrationText(t, valueDetailPlaintextMigration))
	require.NoError(t, err, "apply migration 254")
}

func valueDetailMigrationText(t *testing.T, name string) string {
	t.Helper()

	content, err := migrations.FS.ReadFile(name)
	require.NoErrorf(t, err, "read migration %s", name)
	return string(content)
}

func newUsageLogForValueDetail(t *testing.T, tx *sql.Tx) int64 {
	t.Helper()

	var usageLogID int64
	require.NoError(t, scanSingleRow(context.Background(), tx,
		"INSERT INTO usage_logs DEFAULT VALUES RETURNING id", nil, &usageLogID))
	return usageLogID
}

// newLegacyValueDetailRow 插入一条 253 形状的值明细行：expires_at 刻意走 253 的
// NOT NULL DEFAULT（NOW() + 7 days），用来观察 254 是否改动旧行的既有列。
func newLegacyValueDetailRow(t *testing.T, tx *sql.Tx, state, reason string, ciphertext any) int64 {
	t.Helper()

	usageLogID := newUsageLogForValueDetail(t, tx)
	_, err := tx.ExecContext(context.Background(), `
INSERT INTO request_audit_value_details
    (usage_log_id, state, reason, route, protocol, client_status, attempt_count,
     entry_count, payload_bytes, key_version, ciphertext)
VALUES ($1, $2, $3, '/v1/messages', 'anthropic.messages', 200, 1, 2, 16, 1, $4)
`, usageLogID, state, reason, ciphertext)
	require.NoError(t, err, "insert legacy value detail row")
	return usageLogID
}

func constraintExists(t *testing.T, tx *sql.Tx, schema, table, name string) bool {
	t.Helper()

	var exists bool
	require.NoError(t, scanSingleRow(context.Background(), tx, `
SELECT EXISTS (
    SELECT 1
    FROM pg_constraint c
    JOIN pg_class rel ON rel.oid = c.conrelid
    JOIN pg_namespace ns ON ns.oid = rel.relnamespace
    WHERE ns.nspname = $1 AND rel.relname = $2 AND c.conname = $3
)
`, []any{schema, table, name}, &exists))
	return exists
}

func requireConstraintExists(t *testing.T, tx *sql.Tx, schema, table, name string) {
	t.Helper()
	require.Truef(t, constraintExists(t, tx, schema, table, name), "expected constraint %s", name)
}

func requireConstraintAbsent(t *testing.T, tx *sql.Tx, schema, table, name string) {
	t.Helper()
	require.Falsef(t, constraintExists(t, tx, schema, table, name),
		"constraint %s should have been replaced by the 254 predicates", name)
}

// requireConstraintNotValidated 断言约束是 NOT VALID：ADD CONSTRAINT 没有扫描整表，
// 因此 ACCESS EXCLUSIVE 只用于改目录，不随行数增长。
func requireConstraintNotValidated(t *testing.T, tx *sql.Tx, schema, table, name string) {
	t.Helper()

	var validated bool
	require.NoError(t, scanSingleRow(context.Background(), tx, `
SELECT c.convalidated
FROM pg_constraint c
JOIN pg_class rel ON rel.oid = c.conrelid
JOIN pg_namespace ns ON ns.oid = rel.relnamespace
WHERE ns.nspname = $1 AND rel.relname = $2 AND c.conname = $3
`, []any{schema, table, name}, &validated))
	require.Falsef(t, validated,
		"constraint %s must be NOT VALID so ADD CONSTRAINT stays O(1) instead of scanning the history", name)
}

// validateValueDetailConstraint 让 PostgreSQL 检查全部已存行。任何一条旧行不满足新谓词，
// 这里就会失败——这是「NOT VALID 没有掩盖违规旧行」的直接证据。
func validateValueDetailConstraint(t *testing.T, tx *sql.Tx, schema, table, name string) {
	t.Helper()

	_, err := tx.ExecContext(context.Background(),
		fmt.Sprintf("ALTER TABLE %s.%s VALIDATE CONSTRAINT %s", schema, table, name))
	require.NoErrorf(t, err, "existing rows must satisfy %s", name)
}

// requireColumnAddedAsFastDefault 断言列是以 fast default（attmissingval）追加的：
// 该路径不重写堆表，加锁窗口与既有行数无关。
func requireColumnAddedAsFastDefault(t *testing.T, tx *sql.Tx, schema, table, column string) {
	t.Helper()

	var hasMissingDefault bool
	require.NoError(t, scanSingleRow(context.Background(), tx, `
SELECT attr.atthasmissing
FROM pg_attribute attr
JOIN pg_class rel ON rel.oid = attr.attrelid
JOIN pg_namespace ns ON ns.oid = rel.relnamespace
WHERE ns.nspname = $1 AND rel.relname = $2 AND attr.attname = $3 AND attr.attnum > 0
`, []any{schema, table, column}, &hasMissingDefault))
	require.Truef(t, hasMissingDefault,
		"column %s must be added as a fast default (no table rewrite) to keep the ACCESS EXCLUSIVE window bounded", column)
}

// expectPostgresError 在 SAVEPOINT 内执行语句并要求它以指定 SQLSTATE 失败；
// 回滚到该 SAVEPOINT 后事务仍然可用（PostgreSQL 会把失败语句之后的事务置为 aborted）。
func expectPostgresError(t *testing.T, tx *sql.Tx, code pq.ErrorCode, statement string, args ...any) {
	t.Helper()

	ctx := context.Background()
	_, err := tx.ExecContext(ctx, "SAVEPOINT expected_violation")
	require.NoError(t, err)
	defer func() {
		_, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT expected_violation")
		require.NoError(t, rollbackErr)
	}()

	_, err = tx.ExecContext(ctx, statement, args...)
	require.Errorf(t, err, "statement must be rejected: %s", statement)

	var pgErr *pq.Error
	require.Truef(t, errors.As(err, &pgErr), "expected a PostgreSQL error, got %v", err)
	require.Equalf(t, code, pgErr.Code, "unexpected SQLSTATE %s (%s)", pgErr.Code, pgErr.Message)
}
