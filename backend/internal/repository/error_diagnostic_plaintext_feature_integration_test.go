//go:build integration

package repository

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 票据 08／09 的端到端存储事实（本地 PostgreSQL，仅合成数据）：
//   - 新明文行写进明文列，旧密文列保持空；读取不需要任何密钥；
//   - 已关联 usage 的行随 usage 删除；未关联的行在三十天整点不可读、随后被清行；
//   - 三十天清理**不**碰已关联的行；
//   - 明文清理只置空载荷并记 purged，保留原因码，使「曾留存」与「从未留存」可区分；
//   - 旧格式行（密文）不受任何新逻辑影响。

func plainDiagnosticWriteFixture(t *testing.T, digest string, attemptIndex, status int) service.ErrorDiagnosticWrite {
	t.Helper()
	id, err := service.NewErrorDiagnosticID()
	require.NoError(t, err)
	return service.ErrorDiagnosticWrite{
		ID: id,
		Attempt: service.ErrorDiagnosticAttempt{
			Protocol:           service.ErrorDiagnosticProtocolMessages,
			Stage:              service.ErrorDiagnosticStageWire,
			AttemptIndex:       attemptIndex,
			UpstreamStatusCode: status,
			Body:               []byte(`{"model":"claude"}`),
			BodyReadComplete:   true,
		},
		PlainRecord:           true,
		PlainLinkDigest:       digest,
		PlainLinkAttemptIndex: attemptIndex,
		PlainLinkWireStatus:   status,
		PlainBodyState:        service.ErrorDiagnosticBodyStateStored,
		PlainBodyReason:       service.ErrorDiagnosticPlainBodyRetained,
		PlainBodyPayload:      []byte(`{"model":"claude"}`),
		PlainHeaderState:      service.ErrorDiagnosticHeaderStateStored,
		PlainHeaderReason:     service.ErrorDiagnosticPlainHeaderRetained,
		// 方向必须与白名单一致：Retry-After 是**响应侧**头名，放进请求侧会让整份快照不合格。
		PlainHeaderPayload:    []byte(`{"request":{"Content-Type":"application/json"},"response":{"Retry-After":"30"}}`),
		PlainHeaderEntryCount: 1,
	}
}

// cleanupUsageLogFixture 删除本测试创建的使用记录。
//
// 这些行是**全局可见**的：同一轮里的使用记录统计类测试会按 user_id 去重计数，
// 漏删一行就会把别的套件读成「多了一个活跃用户」。
func cleanupUsageLogFixture(t *testing.T, usageID int64) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM usage_logs WHERE id = $1`, usageID)
	})
}

// TestPlainDiagnosticStoresPlaintextWithoutAnyCipher 覆盖写读闭环：没有密钥也能写、能读，
// 而且一次都不碰密文列。
func TestPlainDiagnosticStoresPlaintextWithoutAnyCipher(t *testing.T) {
	ctx := context.Background()
	repo := NewErrorDiagnosticRepository(integrationDB, nil)
	writer, ok := repo.(interface {
		service.ErrorDiagnosticRepository
		service.ErrorDiagnosticPlaintextReader
	})
	require.True(t, ok, "真实仓储必须同时提供明文读取与清理能力")

	write := plainDiagnosticWriteFixture(t, "", 1, 500)
	now := time.Now().UTC()
	record, err := writer.CreateErrorDiagnostic(ctx, write, now)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM error_diagnostic_records WHERE diagnostic_id = $1`, write.ID)
	})
	require.True(t, record.PlainRecord)
	require.False(t, record.BodyStored, "明文行不得同时带密文")

	var ciphertext sql.NullString
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT body_ciphertext::text, header_ciphertext::text FROM error_diagnostic_records WHERE diagnostic_id = $1`,
		write.ID).Scan(&ciphertext, new(sql.NullString)))
	require.False(t, ciphertext.Valid, "旧密文列必须保持空")

	body, err := writer.ReadErrorDiagnosticPlainBody(ctx, write.ID, now)
	require.NoError(t, err)
	require.Equal(t, `{"model":"claude"}`, string(body))

	headers, err := writer.ReadErrorDiagnosticPlainHeaderValues(ctx, write.ID, now)
	require.NoError(t, err)
	require.Equal(t, "30", headers.Response["Retry-After"])
	require.Equal(t, "application/json", headers.Request["Content-Type"])
}

// TestPlainDiagnosticLinkedRowFollowsUsage 覆盖所有权：可靠关联后整行随 usage 删除，
// 而且三十天的整行清理不会碰它。
func TestPlainDiagnosticLinkedRowFollowsUsage(t *testing.T) {
	ctx := context.Background()
	_, _, _, usageID := createErrorDiagnosticUsageFixture(t, ctx)
	cleanupUsageLogFixture(t, usageID)
	repo := NewErrorDiagnosticRepository(integrationDB, nil)
	attacher, ok := repo.(service.ErrorDiagnosticUsageAttacher)
	require.True(t, ok)

	digest := strings.Repeat("c", 64)
	_, err := integrationDB.ExecContext(ctx, `INSERT INTO request_audits (usage_log_id, metadata, attempts)
		VALUES ($1, jsonb_build_object('ids', jsonb_build_object('local_request_fingerprint', $2::text)),
		jsonb_build_array(jsonb_build_object('stage', 'wire', 'upstream_status', 429)))`, usageID, digest)
	require.NoError(t, err)

	write := plainDiagnosticWriteFixture(t, digest, 1, 429)
	// 关联必须发生在三十天窗口之内（过期行绝不复活，见 07 的接缝）。
	created := time.Now().UTC().Add(-24 * time.Hour)
	_, err = repo.CreateErrorDiagnostic(ctx, write, created)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM error_diagnostic_records WHERE diagnostic_id = $1`, write.ID)
	})

	linked, err := attacher.LinkPlainErrorDiagnostics(ctx, usageID, digest, time.Now().UTC())
	require.NoError(t, err)
	require.EqualValues(t, 1, linked)

	// 把时钟推到三十天之后：已关联的行不得被整行清理带走，也不得被清载荷。
	afterWindow := created.Add(40 * 24 * time.Hour)
	_, err = repo.DeleteExpiredErrorDiagnostics(ctx, afterWindow, 100)
	require.NoError(t, err)
	var remaining int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM error_diagnostic_records WHERE diagnostic_id = $1`, write.ID).Scan(&remaining))
	require.Equal(t, 1, remaining, "已关联 usage 的明文行不受三十天清理影响")

	// 明文清理也不得碰已关联的行：它随 usage，没有自有到期时刻。
	_, err = repo.(service.ErrorDiagnosticPlaintextReader).ClearExpiredErrorDiagnosticPlainBodies(ctx, afterWindow, 100)
	require.NoError(t, err)
	body, err := repo.(service.ErrorDiagnosticPlaintextReader).ReadErrorDiagnosticPlainBody(ctx, write.ID, afterWindow)
	require.NoError(t, err, "已关联的明文在三十天之后仍然可读")
	require.Equal(t, `{"model":"claude"}`, string(body))

	_, err = integrationDB.ExecContext(ctx, `DELETE FROM usage_logs WHERE id = $1`, usageID)
	require.NoError(t, err)
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM error_diagnostic_records WHERE diagnostic_id = $1`, write.ID).Scan(&remaining))
	require.Zero(t, remaining, "使用记录被删除时明文行整行消失")
}

// TestPlainDiagnosticUnlinkedRowIsRefusedAtTheCutoffAndThenCleared 覆盖未关联行：
// 三十天整点 API 拒绝读取，随后清理只置空载荷并保留「曾留存」的事实。
func TestPlainDiagnosticUnlinkedRowIsRefusedAtTheCutoffAndThenCleared(t *testing.T) {
	ctx := context.Background()
	repo := NewErrorDiagnosticRepository(integrationDB, nil)
	reader := repo.(service.ErrorDiagnosticPlaintextReader)

	write := plainDiagnosticWriteFixture(t, "", 1, 500)
	created := time.Now().UTC().Add(-31 * 24 * time.Hour)
	_, err := repo.CreateErrorDiagnostic(ctx, write, created)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM error_diagnostic_records WHERE diagnostic_id = $1`, write.ID)
	})

	_, err = reader.ReadErrorDiagnosticPlainBody(ctx, write.ID, time.Now().UTC())
	require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone, "未关联行过了三十天必须立刻拒绝读取")

	cleared, err := reader.ClearExpiredErrorDiagnosticPlainBodies(ctx, time.Now().UTC(), 100)
	require.NoError(t, err)
	require.GreaterOrEqual(t, cleared, int64(1))

	var state, reason string
	var payload sql.NullString
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT plain_body_state, plain_body_reason, plain_body_payload::text FROM error_diagnostic_records WHERE diagnostic_id = $1`,
		write.ID).Scan(&state, &reason, &payload))
	require.Equal(t, service.ErrorDiagnosticBodyStatePurged, state)
	require.Equal(t, service.ErrorDiagnosticPlainBodyRetained, reason, "清理必须保留「曾留存」的原因码")
	require.False(t, payload.Valid)

	// 清理之后整行也会被三十天删除带走。
	_, err = repo.DeleteExpiredErrorDiagnostics(ctx, time.Now().UTC(), 100)
	require.NoError(t, err)
	var remaining int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM error_diagnostic_records WHERE diagnostic_id = $1`, write.ID).Scan(&remaining))
	require.Zero(t, remaining)
}

// TestLegacyDiagnosticRowsAreUntouchedByThePlaintextLayers 是回归：旧格式写入仍然只写密文列，
// 新列一个事实都不冒充。
func TestLegacyDiagnosticRowsAreUntouchedByThePlaintextLayers(t *testing.T) {
	ctx := context.Background()
	cipher, err := NewErrorDiagnosticBodyCipher([]byte(strings.Repeat("k", 32)), 1)
	require.NoError(t, err)
	repo := NewErrorDiagnosticRepository(integrationDB, cipher)

	body := []byte(`{"model":"claude"}`)
	ciphertext, err := cipher.Encrypt(body)
	require.NoError(t, err)
	id, err := service.NewErrorDiagnosticID()
	require.NoError(t, err)
	write := service.ErrorDiagnosticWrite{
		ID: id,
		Attempt: service.ErrorDiagnosticAttempt{
			Protocol:           service.ErrorDiagnosticProtocolMessages,
			Stage:              service.ErrorDiagnosticStageWire,
			AttemptIndex:       1,
			UpstreamStatusCode: 500,
			Body:               body,
		},
		BodyState:      service.ErrorDiagnosticBodyStateStored,
		BodyReason:     service.ErrorDiagnosticBodyRetained,
		BodyCiphertext: ciphertext,
		BodyKeyVersion: 1,
	}
	record, err := repo.CreateErrorDiagnostic(ctx, write, time.Now().UTC())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM error_diagnostic_records WHERE diagnostic_id = $1`, write.ID)
	})
	require.False(t, record.PlainRecord, "旧格式写入不得变成明文行")

	var plainRecord bool
	var plainBody sql.NullString
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT plain_record, plain_body_payload::text FROM error_diagnostic_records WHERE diagnostic_id = $1`,
		write.ID).Scan(&plainRecord, &plainBody))
	require.False(t, plainRecord)
	require.False(t, plainBody.Valid)

	// 旧密文读取路径仍然工作，明文读取路径对它一无所获。
	legacyBody, err := repo.ReadErrorDiagnosticBody(ctx, write.ID, time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, body, legacyBody)
	_, err = repo.(service.ErrorDiagnosticPlaintextReader).ReadErrorDiagnosticPlainBody(ctx, write.ID, time.Now().UTC())
	require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone)
}

// TestPlainDiagnosticReconcileLinksLateUsage 覆盖异步反序：诊断先落库、usage 后到，
// 由有界补偿把两边按「同一逻辑请求 + 真实序号 + 状态」绑定；不吻合则永不绑定。
func TestPlainDiagnosticReconcileLinksLateUsage(t *testing.T) {
	ctx := context.Background()
	// 一条使用记录只对应一条审计行（request_audits 对 usage_log_id 唯一），
	// 因此两种情形各用一条自己的使用记录，不能共用。
	_, _, _, matchingUsageID := createErrorDiagnosticUsageFixture(t, ctx)
	cleanupUsageLogFixture(t, matchingUsageID)
	_, _, _, mismatchUsageID := createErrorDiagnosticUsageFixture(t, ctx)
	cleanupUsageLogFixture(t, mismatchUsageID)
	repo := NewErrorDiagnosticRepository(integrationDB, nil)
	reconciler, ok := repo.(service.ErrorDiagnosticLinkReconciler)
	require.True(t, ok, "真实仓储必须提供补关联能力")

	insertAudit := func(usageID int64, digest string, secondStatus int) {
		_, err := integrationDB.ExecContext(ctx, `INSERT INTO request_audits (usage_log_id, metadata, attempts)
			VALUES ($1, jsonb_build_object('ids', jsonb_build_object('local_request_fingerprint', $2::text)),
			jsonb_build_array(
				jsonb_build_object('stage', 'wire', 'upstream_status', 500),
				jsonb_build_object('stage', 'wire', 'upstream_status', $3::int)))`, usageID, digest, secondStatus)
		require.NoError(t, err)
	}
	ownerOf := func(id string) sql.NullInt64 {
		var owner sql.NullInt64
		require.NoError(t, integrationDB.QueryRowContext(ctx,
			`SELECT plain_owner_usage_log_id FROM error_diagnostic_records WHERE diagnostic_id = $1`, id).Scan(&owner))
		return owner
	}

	// 情形一：审计里的第 2 次真实尝试状态与诊断吻合 → 绑定。
	matchingDigest := strings.Repeat("d", 64)
	matching := plainDiagnosticWriteFixture(t, matchingDigest, 2, 502)
	_, err := repo.CreateErrorDiagnostic(ctx, matching, time.Now().UTC())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM error_diagnostic_records WHERE diagnostic_id = $1`, matching.ID)
	})
	insertAudit(matchingUsageID, matchingDigest, 502)

	// 情形二：状态不吻合（审计说 503，诊断说 502）→ 绝不绑定。
	mismatchDigest := strings.Repeat("e", 64)
	mismatch := plainDiagnosticWriteFixture(t, mismatchDigest, 2, 502)
	_, err = repo.CreateErrorDiagnostic(ctx, mismatch, time.Now().UTC())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM error_diagnostic_records WHERE diagnostic_id = $1`, mismatch.ID)
	})
	insertAudit(mismatchUsageID, mismatchDigest, 503)

	_, err = reconciler.ReconcilePlainErrorDiagnosticLinks(ctx, time.Now().UTC(), 10)
	require.NoError(t, err)

	require.True(t, ownerOf(matching.ID).Valid, "序号与状态都吻合的诊断必须被补关联")
	require.Equal(t, matchingUsageID, ownerOf(matching.ID).Int64)
	require.False(t, ownerOf(mismatch.ID).Valid, "状态不吻合时绝不猜一个关联")
}
