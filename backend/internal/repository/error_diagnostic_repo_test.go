//go:build unit

package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newErrorDiagnosticSQLMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock, service.ErrorDiagnosticBodyCipher) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cipher, err := NewErrorDiagnosticBodyCipher(key, 2)
	require.NoError(t, err)
	return db, mock, cipher
}

func validErrorDiagnosticID(t *testing.T) string {
	t.Helper()
	id, err := service.NewErrorDiagnosticID()
	require.NoError(t, err)
	return id
}

func errorDiagnosticWriteFixture(t *testing.T) service.ErrorDiagnosticWrite {
	t.Helper()
	return service.ErrorDiagnosticWrite{
		ID: validErrorDiagnosticID(t),
		Attempt: service.ErrorDiagnosticAttempt{
			Protocol:           service.ErrorDiagnosticProtocolMessages,
			Stage:              service.ErrorDiagnosticStageWire,
			AttemptIndex:       1,
			UpstreamStatusCode: 503,
		},
		BodyState:  service.ErrorDiagnosticBodyStateNotObserved,
		BodyReason: service.ErrorDiagnosticBodyNotObserved,
	}
}

func errorDiagnosticSelectColumns() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"diagnostic_id", "usage_log_id", "protocol", "attempt_index", "stage", "upstream_status",
		"body_state", "body_reason", "body_stored", "body_bytes", "body_key_version",
		"created_at", "metadata_expires_at", "body_expires_at",
		"header_state", "header_reason", "header_stored", "header_bytes", "header_key_version",
		"header_entry_count", "header_expires_at",
	})
}

// TestErrorDiagnosticRepository_NilDependenciesFailClosed 覆盖缺依赖时不得 panic、
// 也不得把零值当成成功结果返回。
func TestErrorDiagnosticRepository_NilDependenciesFailClosed(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	id := strings.Repeat("a", service.ErrorDiagnosticIDLength)

	var nilRepo *errorDiagnosticRepository

	for name, repo := range map[string]service.ErrorDiagnosticRepository{
		"nil receiver": nilRepo,
		"nil db":       NewErrorDiagnosticRepository(nil, nil),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := repo.CreateErrorDiagnostic(ctx, errorDiagnosticWriteFixture(t), now)
			require.ErrorIs(t, err, service.ErrErrorDiagnosticUnavailable)

			_, err = repo.GetErrorDiagnostic(ctx, id)
			require.ErrorIs(t, err, service.ErrErrorDiagnosticUnavailable)

			_, err = repo.ListRecentErrorDiagnostics(ctx, "", 10)
			require.ErrorIs(t, err, service.ErrErrorDiagnosticUnavailable)

			_, err = repo.ListErrorDiagnosticsByUsageLog(ctx, 1, 10)
			require.ErrorIs(t, err, service.ErrErrorDiagnosticUnavailable)

			_, err = repo.ReadErrorDiagnosticBody(ctx, id, now)
			require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone)

			_, err = repo.ClearExpiredErrorDiagnosticBodies(ctx, now, 10)
			require.ErrorIs(t, err, service.ErrErrorDiagnosticUnavailable)

			_, err = repo.DeleteExpiredErrorDiagnostics(ctx, now, 10)
			require.ErrorIs(t, err, service.ErrErrorDiagnosticUnavailable)
		})
	}
}

// TestErrorDiagnosticRepository_RejectsMalformedIDsBeforeSQL 覆盖
// 形状不合法的标识不得进入 SQL 语句。
func TestErrorDiagnosticRepository_RejectsMalformedIDsBeforeSQL(t *testing.T) {
	ctx := context.Background()
	db, mock, cipher := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, cipher)

	_, err := repo.GetErrorDiagnostic(ctx, "'; DROP TABLE error_diagnostic_records; --")
	require.ErrorIs(t, err, service.ErrErrorDiagnosticNotFound)

	_, err = repo.ReadErrorDiagnosticBody(ctx, "not-an-id", time.Now())
	require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone)

	write := errorDiagnosticWriteFixture(t)
	write.ID = "not-an-id"
	_, err = repo.CreateErrorDiagnostic(ctx, write, time.Now())
	require.ErrorIs(t, err, service.ErrErrorDiagnosticInvalidAttempt)

	require.NoError(t, mock.ExpectationsWereMet(), "不合法标识不得触发任何 SQL")
}

// TestErrorDiagnosticRepository_CreateMetadataOnlyBindsNoBody 覆盖票 01：
// 元数据写入不绑定任何密文，到期时刻由存储层按保留期计算。
func TestErrorDiagnosticRepository_CreateMetadataOnlyBindsNoBody(t *testing.T) {
	ctx := context.Background()
	db, mock, cipher := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, cipher)

	write := errorDiagnosticWriteFixture(t)
	write.Attempt.UsageLogID = 0
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`(?s)INSERT INTO error_diagnostic_records.*RETURNING created_at`).
		WithArgs(
			write.ID, nil, service.ErrorDiagnosticProtocolMessages, 1, service.ErrorDiagnosticStageWire, 503,
			service.ErrorDiagnosticBodyStateNotObserved, service.ErrorDiagnosticBodyNotObserved,
			nil, 0, 0,
			now, now.Add(service.ErrorDiagnosticMetadataRetention), nil,
			service.ErrorDiagnosticHeaderStateNotObserved, service.ErrorDiagnosticHeaderNotObserved,
			nil, 0, 0, 0, nil,
		).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))

	record, err := repo.CreateErrorDiagnostic(ctx, write, now)
	require.NoError(t, err)
	require.False(t, record.HasUsage)
	require.Zero(t, record.UsageLogID)
	require.False(t, record.BodyStored)
	require.Zero(t, record.BodyBytes)
	require.Zero(t, record.BodyKeyVersion)
	require.True(t, record.BodyExpiresAt.IsZero())
	require.Equal(t, now.Add(service.ErrorDiagnosticMetadataRetention), record.MetadataExpiresAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestErrorDiagnosticRepository_CreateRetainedBodyBindsCiphertext 覆盖票 02：
// 只有密文落库，正文长度按明文计，正文到期为创建后 7 天。
func TestErrorDiagnosticRepository_CreateRetainedBodyBindsCiphertext(t *testing.T) {
	ctx := context.Background()
	db, mock, cipher := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, cipher)

	plaintext := []byte(`{"model":"claude","messages":[{"role":"user","content":"sentinel"}]}`)
	ciphertext, err := cipher.Encrypt(plaintext)
	require.NoError(t, err)

	write := errorDiagnosticWriteFixture(t)
	write.Attempt.UsageLogID = 77
	write.Attempt.Body = plaintext
	write.BodyState = service.ErrorDiagnosticBodyStateStored
	write.BodyReason = service.ErrorDiagnosticBodyRetained
	write.BodyCiphertext = ciphertext
	write.BodyKeyVersion = 2

	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`(?s)INSERT INTO error_diagnostic_records.*RETURNING created_at`).
		WithArgs(
			write.ID, int64(77), service.ErrorDiagnosticProtocolMessages, 1, service.ErrorDiagnosticStageWire, 503,
			service.ErrorDiagnosticBodyStateStored, service.ErrorDiagnosticBodyRetained,
			ciphertext, 2, len(plaintext),
			now, now.Add(service.ErrorDiagnosticMetadataRetention), now.Add(service.ErrorDiagnosticBodyRetention),
			service.ErrorDiagnosticHeaderStateNotObserved, service.ErrorDiagnosticHeaderNotObserved,
			nil, 0, 0, 0, nil,
		).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))

	record, err := repo.CreateErrorDiagnostic(ctx, write, now)
	require.NoError(t, err)
	require.True(t, record.HasUsage)
	require.Equal(t, int64(77), record.UsageLogID)
	require.True(t, record.BodyStored)
	require.Equal(t, len(plaintext), record.BodyBytes)
	require.Equal(t, 2, record.BodyKeyVersion)
	require.Equal(t, now.Add(service.ErrorDiagnosticBodyRetention), record.BodyExpiresAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestErrorDiagnosticRepository_ClaimedStoredWithoutCiphertextIsDowngraded 覆盖
// 「自称已留存却没有密文」的写入必须收敛成未留存，绝不落库成明文或矛盾行。
func TestErrorDiagnosticRepository_ClaimedStoredWithoutCiphertextIsDowngraded(t *testing.T) {
	ctx := context.Background()
	db, mock, cipher := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, cipher)

	write := errorDiagnosticWriteFixture(t)
	write.BodyState = service.ErrorDiagnosticBodyStateStored
	write.BodyReason = service.ErrorDiagnosticBodyRetained
	// BodyCiphertext 故意留空。
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`(?s)INSERT INTO error_diagnostic_records.*RETURNING created_at`).
		WithArgs(
			write.ID, nil, service.ErrorDiagnosticProtocolMessages, 1, service.ErrorDiagnosticStageWire, 503,
			service.ErrorDiagnosticBodyStateSkipped, service.ErrorDiagnosticBodySkippedEncryptionUnavailable,
			nil, 0, 0,
			now, now.Add(service.ErrorDiagnosticMetadataRetention), nil,
			service.ErrorDiagnosticHeaderStateNotObserved, service.ErrorDiagnosticHeaderNotObserved,
			nil, 0, 0, 0, nil,
		).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))

	record, err := repo.CreateErrorDiagnostic(ctx, write, now)
	require.NoError(t, err)
	require.Equal(t, service.ErrorDiagnosticBodyStateSkipped, record.BodyState)
	require.Equal(t, service.ErrorDiagnosticBodySkippedEncryptionUnavailable, record.BodyReason)
	require.False(t, record.BodyStored)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestErrorDiagnosticRepository_ReadMapsNullableColumns 覆盖可空列的映射：
// 无 usage 与无正文都必须映射成零值而不是伪造值。
func TestErrorDiagnosticRepository_ReadMapsNullableColumns(t *testing.T) {
	ctx := context.Background()
	db, mock, cipher := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, cipher)

	id := validErrorDiagnosticID(t)
	created := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`(?s)SELECT.*FROM error_diagnostic_records WHERE diagnostic_id = \$1`).
		WithArgs(id).
		WillReturnRows(errorDiagnosticSelectColumns().AddRow(
			id, nil, service.ErrorDiagnosticProtocolMessages, 3, service.ErrorDiagnosticStageWire, 429,
			service.ErrorDiagnosticBodyStateNotObserved, service.ErrorDiagnosticBodyNotObserved,
			false, 0, 0,
			created, created.Add(service.ErrorDiagnosticMetadataRetention), nil,
			service.ErrorDiagnosticHeaderStateNotObserved, service.ErrorDiagnosticHeaderNotObserved,
			false, 0, 0, 0, nil,
		))

	record, err := repo.GetErrorDiagnostic(ctx, id)
	require.NoError(t, err)
	require.False(t, record.HasUsage)
	require.Zero(t, record.UsageLogID)
	require.False(t, record.BodyStored)
	require.True(t, record.BodyExpiresAt.IsZero())
	require.Equal(t, 429, record.UpstreamStatusCode)
	require.Equal(t, 3, record.AttemptIndex)

	// 无行即不存在：清理延迟或已被物理删除都走同一条路径。
	mock.ExpectQuery(`(?s)SELECT.*FROM error_diagnostic_records WHERE diagnostic_id = \$1`).
		WithArgs(id).
		WillReturnError(sql.ErrNoRows)
	_, err = repo.GetErrorDiagnostic(ctx, id)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticNotFound)

	// 数据库错误必须冒泡，不得伪装成「不存在」。
	dbErr := errors.New("connection reset")
	mock.ExpectQuery(`(?s)SELECT.*FROM error_diagnostic_records WHERE diagnostic_id = \$1`).
		WithArgs(id).
		WillReturnError(dbErr)
	_, err = repo.GetErrorDiagnostic(ctx, id)
	require.Error(t, err)
	require.NotErrorIs(t, err, service.ErrErrorDiagnosticNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestErrorDiagnosticRepository_ReadBodyRefusesExpiredWithoutDecrypting 覆盖
// 过期正文在 SQL 之后、解密之前就被拒绝。
func TestErrorDiagnosticRepository_ReadBodyRefusesExpiredWithoutDecrypting(t *testing.T) {
	ctx := context.Background()
	db, mock, cipher := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, cipher)

	plaintext := []byte(`{"a":1}`)
	ciphertext, err := cipher.Encrypt(plaintext)
	require.NoError(t, err)

	id := validErrorDiagnosticID(t)
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	// 正文已过第 7 天：不得解密。
	mock.ExpectQuery(`(?s)SELECT body_ciphertext, body_expires_at, metadata_expires_at`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"body_ciphertext", "body_expires_at", "metadata_expires_at"}).
			AddRow(ciphertext, now.Add(-time.Minute), now.Add(20*24*time.Hour)))
	_, err = repo.ReadErrorDiagnosticBody(ctx, id, now)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone)

	// 元数据已过第 30 天：同样拒绝，即使正文期限看起来还在。
	mock.ExpectQuery(`(?s)SELECT body_ciphertext, body_expires_at, metadata_expires_at`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"body_ciphertext", "body_expires_at", "metadata_expires_at"}).
			AddRow(ciphertext, now.Add(time.Hour), now.Add(-time.Minute)))
	_, err = repo.ReadErrorDiagnosticBody(ctx, id, now)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone)

	// 密文已被第 7 天清理置空：列为 NULL。
	mock.ExpectQuery(`(?s)SELECT body_ciphertext, body_expires_at, metadata_expires_at`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"body_ciphertext", "body_expires_at", "metadata_expires_at"}).
			AddRow(nil, now.Add(time.Hour), now.Add(20*24*time.Hour)))
	_, err = repo.ReadErrorDiagnosticBody(ctx, id, now)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone)

	// 行已删除。
	mock.ExpectQuery(`(?s)SELECT body_ciphertext, body_expires_at, metadata_expires_at`).
		WithArgs(id).
		WillReturnError(sql.ErrNoRows)
	_, err = repo.ReadErrorDiagnosticBody(ctx, id, now)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone)

	// 未到期：解密成逐字节一致的明文。
	mock.ExpectQuery(`(?s)SELECT body_ciphertext, body_expires_at, metadata_expires_at`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"body_ciphertext", "body_expires_at", "metadata_expires_at"}).
			AddRow(ciphertext, now.Add(time.Hour), now.Add(20*24*time.Hour)))
	body, err := repo.ReadErrorDiagnosticBody(ctx, id, now)
	require.NoError(t, err)
	require.Equal(t, plaintext, body)

	// 密文被篡改：认证失败也必须收敛成「不可用」，不返回任何内容。
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0xFF
	mock.ExpectQuery(`(?s)SELECT body_ciphertext, body_expires_at, metadata_expires_at`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"body_ciphertext", "body_expires_at", "metadata_expires_at"}).
			AddRow(tampered, now.Add(time.Hour), now.Add(20*24*time.Hour)))
	_, err = repo.ReadErrorDiagnosticBody(ctx, id, now)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone)

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestErrorDiagnosticRepository_ReadBodyWithoutCipherIsUnavailable 覆盖缺密钥：
// 连查询都不发起，正文一律不可读。
func TestErrorDiagnosticRepository_ReadBodyWithoutCipherIsUnavailable(t *testing.T) {
	db, mock, _ := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, nil)

	_, err := repo.ReadErrorDiagnosticBody(context.Background(), validErrorDiagnosticID(t), time.Now())
	require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestErrorDiagnosticRepository_ListsAreBoundedAndOrdered 覆盖列表语义：
// 结果条数受调用方 limit 约束，且 usage 关联查询不触碰空关联。
func TestErrorDiagnosticRepository_ListsAreBoundedAndOrdered(t *testing.T) {
	ctx := context.Background()
	db, mock, cipher := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, cipher)

	created := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	firstID := validErrorDiagnosticID(t)
	secondID := validErrorDiagnosticID(t)

	mock.ExpectQuery(`(?s)SELECT.*FROM error_diagnostic_records.*WHERE \(\$1 = '' OR protocol = \$1\).*ORDER BY created_at DESC, diagnostic_id DESC.*LIMIT \$2`).
		WithArgs(service.ErrorDiagnosticProtocolMessages, 25).
		WillReturnRows(errorDiagnosticSelectColumns().
			AddRow(firstID, nil, service.ErrorDiagnosticProtocolMessages, 0, service.ErrorDiagnosticStageWire, 500,
				service.ErrorDiagnosticBodyStateNotObserved, service.ErrorDiagnosticBodyNotObserved,
				false, 0, 0, created, created.Add(service.ErrorDiagnosticMetadataRetention), nil,
				service.ErrorDiagnosticHeaderStateNotObserved, service.ErrorDiagnosticHeaderNotObserved,
				false, 0, 0, 0, nil).
			AddRow(secondID, int64(9), service.ErrorDiagnosticProtocolMessages, 1, service.ErrorDiagnosticStageWire, 502,
				service.ErrorDiagnosticBodyStateNotObserved, service.ErrorDiagnosticBodyNotObserved,
				false, 0, 0, created, created.Add(service.ErrorDiagnosticMetadataRetention), nil,
				service.ErrorDiagnosticHeaderStateNotObserved, service.ErrorDiagnosticHeaderNotObserved,
				false, 0, 0, 0, nil))

	records, err := repo.ListRecentErrorDiagnostics(ctx, service.ErrorDiagnosticProtocolMessages, 25)
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, firstID, records[0].ID)
	require.False(t, records[0].HasUsage)
	require.Equal(t, secondID, records[1].ID)
	require.True(t, records[1].HasUsage)
	require.Equal(t, int64(9), records[1].UsageLogID)

	mock.ExpectQuery(`(?s)SELECT.*FROM error_diagnostic_records.*WHERE usage_log_id = \$1.*ORDER BY created_at ASC, attempt_index ASC, diagnostic_id ASC.*LIMIT \$2`).
		WithArgs(int64(9), 10).
		WillReturnRows(errorDiagnosticSelectColumns().
			AddRow(secondID, int64(9), service.ErrorDiagnosticProtocolMessages, 1, service.ErrorDiagnosticStageWire, 502,
				service.ErrorDiagnosticBodyStateNotObserved, service.ErrorDiagnosticBodyNotObserved,
				false, 0, 0, created, created.Add(service.ErrorDiagnosticMetadataRetention), nil,
				service.ErrorDiagnosticHeaderStateNotObserved, service.ErrorDiagnosticHeaderNotObserved,
				false, 0, 0, 0, nil))

	linked, err := repo.ListErrorDiagnosticsByUsageLog(ctx, 9, 10)
	require.NoError(t, err)
	require.Len(t, linked, 1)

	// 无效的 usage 关联不得查询存储层。
	none, err := repo.ListErrorDiagnosticsByUsageLog(ctx, 0, 10)
	require.NoError(t, err)
	require.Empty(t, none)

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestErrorDiagnosticRepository_CleanupTargetsOnlyExpiredRows 覆盖
// 第 7 天只清密文、第 30 天才删整行，且都只在线主库执行。
func TestErrorDiagnosticRepository_CleanupTargetsOnlyExpiredRows(t *testing.T) {
	ctx := context.Background()
	db, mock, cipher := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, cipher)
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	mock.ExpectExec(`(?s)UPDATE error_diagnostic_records\s+SET body_ciphertext = NULL, body_key_version = 0, body_state = 'purged'.*body_expires_at <= \$1`).
		WithArgs(now, 100).
		WillReturnResult(sqlmock.NewResult(0, 3))
	cleared, err := repo.ClearExpiredErrorDiagnosticBodies(ctx, now, 100)
	require.NoError(t, err)
	require.EqualValues(t, 3, cleared)

	mock.ExpectExec(`(?s)DELETE FROM error_diagnostic_records.*metadata_expires_at <= \$1`).
		WithArgs(now, 100).
		WillReturnResult(sqlmock.NewResult(0, 7))
	deleted, err := repo.DeleteExpiredErrorDiagnostics(ctx, now, 100)
	require.NoError(t, err)
	require.EqualValues(t, 7, deleted)

	// 清理失败必须冒泡，不能伪称已清理。
	cleanupErr := errors.New("deadlock detected")
	mock.ExpectExec(`(?s)UPDATE error_diagnostic_records`).WithArgs(now, 100).WillReturnError(cleanupErr)
	_, err = repo.ClearExpiredErrorDiagnosticBodies(ctx, now, 100)
	require.ErrorIs(t, err, cleanupErr)

	mock.ExpectExec(`(?s)DELETE FROM error_diagnostic_records`).WithArgs(now, 100).WillReturnError(cleanupErr)
	_, err = repo.DeleteExpiredErrorDiagnostics(ctx, now, 100)
	require.ErrorIs(t, err, cleanupErr)

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestErrorDiagnosticRepository_HeaderValuesAreStoredIndependentlyOfBody 覆盖二者的正交性：
// 正文不留存而 429 头值留存必须同时成立——这正是「头值与正文互不影响」在存储层的体现。
func TestErrorDiagnosticRepository_HeaderValuesAreStoredIndependentlyOfBody(t *testing.T) {
	ctx := context.Background()
	db, mock, cipher := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, cipher)
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	payload, err := service.EncodeErrorDiagnosticHeaderValues(service.ErrorDiagnosticHeaderValues{
		Response: map[string]string{"Retry-After": "42"},
	})
	require.NoError(t, err)
	ciphertext, err := cipher.Encrypt(payload)
	require.NoError(t, err)

	write := errorDiagnosticWriteFixture(t)
	write.Attempt.Protocol = service.ErrorDiagnosticProtocolMessages
	write.Attempt.UpstreamStatusCode = 429
	write.Attempt.HeaderValues = service.ErrorDiagnosticHeaderValues{
		Response: map[string]string{"Retry-After": "42"},
	}
	write.BodyState = service.ErrorDiagnosticBodyStateSkipped
	write.BodyReason = service.ErrorDiagnosticBodySkippedRetentionDisabled
	write.HeaderState = service.ErrorDiagnosticHeaderStateStored
	write.HeaderReason = service.ErrorDiagnosticHeaderRetained
	write.HeaderCiphertext = ciphertext
	write.HeaderKeyVersion = 2
	write.HeaderEntryCount = 1
	write.HeaderPayloadBytes = len(payload)

	mock.ExpectQuery(`(?s)INSERT INTO error_diagnostic_records.*RETURNING created_at`).
		WithArgs(
			write.ID, nil, service.ErrorDiagnosticProtocolMessages, 1, service.ErrorDiagnosticStageWire, 429,
			service.ErrorDiagnosticBodyStateSkipped, service.ErrorDiagnosticBodySkippedRetentionDisabled,
			nil, 0, 0,
			now, now.Add(service.ErrorDiagnosticMetadataRetention), nil,
			service.ErrorDiagnosticHeaderStateStored, service.ErrorDiagnosticHeaderRetained,
			ciphertext, 2, len(payload), 1, now.Add(service.ErrorDiagnosticHeaderRetention),
		).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))

	record, err := repo.CreateErrorDiagnostic(ctx, write, now)
	require.NoError(t, err)
	require.False(t, record.BodyStored, "正文未留存")
	require.True(t, record.BodyExpiresAt.IsZero())
	require.True(t, record.HeaderStored, "头值必须留存")
	require.Equal(t, now.Add(service.ErrorDiagnosticHeaderRetention), record.HeaderExpiresAt)
	require.Equal(t, len(payload), record.HeaderBytes)
	require.Equal(t, 1, record.HeaderEntryCount)
	require.Equal(t, 2, record.HeaderKeyVersion)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestErrorDiagnosticRepository_ClaimedStoredHeaderValuesWithoutCiphertextAreDowngraded 覆盖
// 「自称已留存头值却没有密文」的写入被降级为未留存，绝不在库里留下自相矛盾的行。
func TestErrorDiagnosticRepository_ClaimedStoredHeaderValuesWithoutCiphertextAreDowngraded(t *testing.T) {
	ctx := context.Background()
	db, mock, cipher := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, cipher)
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	write := errorDiagnosticWriteFixture(t)
	write.Attempt.Protocol = service.ErrorDiagnosticProtocolMessages
	write.Attempt.UpstreamStatusCode = 429
	write.HeaderState = service.ErrorDiagnosticHeaderStateStored
	write.HeaderReason = service.ErrorDiagnosticHeaderRetained
	write.HeaderEntryCount = 3

	mock.ExpectQuery(`(?s)INSERT INTO error_diagnostic_records.*RETURNING created_at`).
		WithArgs(
			write.ID, nil, service.ErrorDiagnosticProtocolMessages, 1, service.ErrorDiagnosticStageWire, 429,
			service.ErrorDiagnosticBodyStateNotObserved, service.ErrorDiagnosticBodyNotObserved,
			nil, 0, 0,
			now, now.Add(service.ErrorDiagnosticMetadataRetention), nil,
			service.ErrorDiagnosticHeaderStateSkipped, service.ErrorDiagnosticHeaderSkippedEncryptionUnavailable,
			nil, 0, 0, 0, nil,
		).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))

	record, err := repo.CreateErrorDiagnostic(ctx, write, now)
	require.NoError(t, err)
	require.False(t, record.HeaderStored)
	require.Equal(t, service.ErrorDiagnosticHeaderStateSkipped, record.HeaderState)
	require.Equal(t, service.ErrorDiagnosticHeaderSkippedEncryptionUnavailable, record.HeaderReason)
	require.Zero(t, record.HeaderEntryCount)
	require.True(t, record.HeaderExpiresAt.IsZero())
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestErrorDiagnosticRepository_ReadHeaderValuesRefusesGoneWithoutLeaking 覆盖读取路径：
// 未到期才解密，且解密结果必须重新通过白名单校验；其余情形一律收敛成同一个「不可用」。
func TestErrorDiagnosticRepository_ReadHeaderValuesRefusesGoneWithoutLeaking(t *testing.T) {
	ctx := context.Background()
	db, mock, cipher := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, cipher)
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	id := validErrorDiagnosticID(t)

	payload, err := service.EncodeErrorDiagnosticHeaderValues(service.ErrorDiagnosticHeaderValues{
		Request: map[string]string{"Anthropic-Version": "2023-06-01"},
	})
	require.NoError(t, err)
	ciphertext, err := cipher.Encrypt(payload)
	require.NoError(t, err)

	expect := func() *sqlmock.ExpectedQuery {
		return mock.ExpectQuery(`(?s)SELECT header_ciphertext, header_expires_at, metadata_expires_at`).WithArgs(id)
	}

	// 头值已过第 7 天：即使密文还在也拒绝。
	expect().WillReturnRows(sqlmock.NewRows([]string{"header_ciphertext", "header_expires_at", "metadata_expires_at"}).
		AddRow(ciphertext, now.Add(-time.Minute), now.Add(20*24*time.Hour)))
	_, err = repo.ReadErrorDiagnosticHeaderValues(ctx, id, now)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticHeaderValuesGone)

	// 元数据已过第 30 天。
	expect().WillReturnRows(sqlmock.NewRows([]string{"header_ciphertext", "header_expires_at", "metadata_expires_at"}).
		AddRow(ciphertext, now.Add(time.Hour), now.Add(-time.Minute)))
	_, err = repo.ReadErrorDiagnosticHeaderValues(ctx, id, now)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticHeaderValuesGone)

	// 密文已被第 7 天清理置空。
	expect().WillReturnRows(sqlmock.NewRows([]string{"header_ciphertext", "header_expires_at", "metadata_expires_at"}).
		AddRow(nil, now.Add(time.Hour), now.Add(20*24*time.Hour)))
	_, err = repo.ReadErrorDiagnosticHeaderValues(ctx, id, now)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticHeaderValuesGone)

	// 行已删除。
	expect().WillReturnError(sql.ErrNoRows)
	_, err = repo.ReadErrorDiagnosticHeaderValues(ctx, id, now)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticHeaderValuesGone)

	// 未到期：解密后逐条一致。
	expect().WillReturnRows(sqlmock.NewRows([]string{"header_ciphertext", "header_expires_at", "metadata_expires_at"}).
		AddRow(ciphertext, now.Add(time.Hour), now.Add(20*24*time.Hour)))
	values, err := repo.ReadErrorDiagnosticHeaderValues(ctx, id, now)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Anthropic-Version": "2023-06-01"}, values.Request)

	// 密文被篡改：认证失败必须收敛成不可用，不返回任何内容。
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0xFF
	expect().WillReturnRows(sqlmock.NewRows([]string{"header_ciphertext", "header_expires_at", "metadata_expires_at"}).
		AddRow(tampered, now.Add(time.Hour), now.Add(20*24*time.Hour)))
	_, err = repo.ReadErrorDiagnosticHeaderValues(ctx, id, now)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticHeaderValuesGone)

	// 解密成功但内容不合格（未知头名）：同样不可用，绝不把越界内容当结果返回。
	invalid, err := cipher.Encrypt([]byte(`{"request":{"x-custom-prompt":"private"}}`))
	require.NoError(t, err)
	expect().WillReturnRows(sqlmock.NewRows([]string{"header_ciphertext", "header_expires_at", "metadata_expires_at"}).
		AddRow(invalid, now.Add(time.Hour), now.Add(20*24*time.Hour)))
	_, err = repo.ReadErrorDiagnosticHeaderValues(ctx, id, now)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticHeaderValuesGone)

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestErrorDiagnosticRepository_HeaderValuesNeedCipherAndValidID 覆盖缺密钥与非法 ID：
// 连查询都不发起，头值一律不可读。
func TestErrorDiagnosticRepository_HeaderValuesNeedCipherAndValidID(t *testing.T) {
	ctx := context.Background()
	// 固定 UTC 时刻：仓储会把入参规范化成 UTC，带单调时钟的 time.Now() 与之不可比。
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	// 缺密钥：读取不查询（无密钥就解密不了），但**清理必须照常执行**——
	// 否则一旦稳定密钥丢失，已到期的头值密文就再也清不掉了。
	db, mock, _ := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, nil)
	_, err := repo.ReadErrorDiagnosticHeaderValues(ctx, validErrorDiagnosticID(t), now)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticHeaderValuesGone)

	mock.ExpectExec(`(?s)UPDATE error_diagnostic_records`).WithArgs(now, 10).
		WillReturnResult(sqlmock.NewResult(0, 2))
	cleared, err := repo.ClearExpiredErrorDiagnosticHeaderValues(ctx, now, 10)
	require.NoError(t, err)
	require.EqualValues(t, 2, cleared)
	require.NoError(t, mock.ExpectationsWereMet())

	// 非法 ID：不查询。
	db2, mock2, cipher2 := newErrorDiagnosticSQLMock(t)
	repo2 := NewErrorDiagnosticRepository(db2, cipher2)
	_, err = repo2.ReadErrorDiagnosticHeaderValues(ctx, "'; DROP TABLE error_diagnostic_records; --", now)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticHeaderValuesGone)
	require.NoError(t, mock2.ExpectationsWereMet())
}

// TestErrorDiagnosticRepository_ClearExpiredHeaderValuesOnlyTouchesCiphertext 覆盖头值清理：
// 只置空密文列与密钥代并记 purged，保留整行元数据与到期时刻。
func TestErrorDiagnosticRepository_ClearExpiredHeaderValuesOnlyTouchesCiphertext(t *testing.T) {
	ctx := context.Background()
	db, mock, cipher := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, cipher)
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	mock.ExpectExec(`(?s)UPDATE error_diagnostic_records\s+SET header_ciphertext = NULL, header_key_version = 0, header_state = 'purged'.*header_expires_at <= \$1`).
		WithArgs(now, 100).
		WillReturnResult(sqlmock.NewResult(0, 5))
	cleared, err := repo.ClearExpiredErrorDiagnosticHeaderValues(ctx, now, 100)
	require.NoError(t, err)
	require.EqualValues(t, 5, cleared)

	// 清理失败必须冒泡，不能伪称已清理。
	cleanupErr := errors.New("deadlock detected")
	mock.ExpectExec(`(?s)UPDATE error_diagnostic_records`).WithArgs(now, 100).WillReturnError(cleanupErr)
	_, err = repo.ClearExpiredErrorDiagnosticHeaderValues(ctx, now, 100)
	require.ErrorIs(t, err, cleanupErr)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestErrorDiagnosticRepository_BacklogReportsHeaderValues 覆盖积压观测扩展到头值列：
// 「这一层是否落后」必须可见，否则 7 天头值的物理残留无从发现。
func TestErrorDiagnosticRepository_BacklogReportsHeaderValues(t *testing.T) {
	ctx := context.Background()
	db, mock, cipher := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, cipher)
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	oldest := now.Add(-90 * time.Minute)

	mock.ExpectQuery(`(?s)SELECT COUNT\(\*\), MIN\(body_expires_at\).*body_ciphertext IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"count", "min"}).AddRow(int64(2), now.Add(-time.Hour)))
	mock.ExpectQuery(`(?s)SELECT COUNT\(\*\), MIN\(metadata_expires_at\).*metadata_expires_at <= \$1`).
		WithArgs(now).
		WillReturnRows(sqlmock.NewRows([]string{"count", "min"}).AddRow(int64(1), now.Add(-30*time.Minute)))
	mock.ExpectQuery(`(?s)SELECT COUNT\(\*\), MIN\(header_expires_at\).*header_ciphertext IS NOT NULL`).
		WithArgs(now).
		WillReturnRows(sqlmock.NewRows([]string{"count", "min"}).AddRow(int64(4), oldest))

	backlogReader, ok := repo.(service.ErrorDiagnosticCleanupBacklogReader)
	require.True(t, ok, "真实仓储必须提供积压观测能力")
	backlog, err := backlogReader.ReadErrorDiagnosticCleanupBacklog(ctx, now)
	require.NoError(t, err)
	require.EqualValues(t, 2, backlog.BodiesOverdue)
	require.EqualValues(t, 1, backlog.RecordsOverdue)
	require.EqualValues(t, 4, backlog.HeaderValuesOverdue)
	require.Equal(t, oldest, backlog.OldestHeaderOverdueAt)
	// 最老超期时长必须把三段一起看，否则头值卡住时监控仍显示「没有落后」。
	require.EqualValues(t, 5400, backlog.OldestOverdueSeconds(now))
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestErrorDiagnosticBodyCipher_DerivesPurposeBoundKeyFromConfig 覆盖
// 诊断正文密钥是用途隔离的派生密钥，且主密钥缺失／格式错误时不产生加密器。
func TestErrorDiagnosticBodyCipher_DerivesPurposeBoundKeyFromConfig(t *testing.T) {
	master := strings.Repeat("ab", 32) // 64 hex 字符 = 32 字节

	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = master
	cipher, err := NewErrorDiagnosticBodyCipherFromConfig(cfg)
	require.NoError(t, err)
	require.NotNil(t, cipher)

	// 同一主密钥派生稳定，且不同用途标签派生不同密钥。
	same, err := NewErrorDiagnosticBodyCipherFromConfig(cfg)
	require.NoError(t, err)
	plaintext := []byte(`{"a":1}`)
	sealed, err := cipher.Encrypt(plaintext)
	require.NoError(t, err)
	opened, err := same.Decrypt(sealed)
	require.NoError(t, err)
	require.Equal(t, plaintext, opened)

	// 派生密钥不得等于主密钥本身（不做直接复用）。
	masterBytes := make([]byte, 32)
	for i := range masterBytes {
		masterBytes[i] = 0xab
	}
	direct, err := NewErrorDiagnosticBodyCipher(masterBytes, 1)
	require.NoError(t, err)
	_, err = direct.Decrypt(sealed)
	require.Error(t, err, "派生密钥必须与直接复用主密钥不同")

	// 主密钥缺失或不是 32 字节都不得产生加密器。
	_, err = NewErrorDiagnosticBodyCipherFromConfig(nil)
	require.Error(t, err)

	empty := &config.Config{}
	_, err = NewErrorDiagnosticBodyCipherFromConfig(empty)
	require.Error(t, err)

	short := &config.Config{}
	short.Totp.EncryptionKey = "abcd"
	_, err = NewErrorDiagnosticBodyCipherFromConfig(short)
	require.Error(t, err)

	notHex := &config.Config{}
	notHex.Totp.EncryptionKey = strings.Repeat("zz", 32)
	_, err = NewErrorDiagnosticBodyCipherFromConfig(notHex)
	require.Error(t, err)
}

// TestErrorDiagnosticBodyCipher_NilReceiverIsUnavailable 覆盖空加密器不得 panic。
func TestErrorDiagnosticBodyCipher_NilReceiverIsUnavailable(t *testing.T) {
	var cipher *errorDiagnosticAESCipher
	require.Zero(t, cipher.KeyVersion())
	_, err := cipher.Encrypt([]byte("x"))
	require.Error(t, err)
	_, err = cipher.Decrypt([]byte("x"))
	require.Error(t, err)
}

// TestErrorDiagnosticBodyCipher_RejectsShortCiphertext 覆盖长度不足不得进入 GCM。
func TestErrorDiagnosticBodyCipher_RejectsShortCiphertext(t *testing.T) {
	key := make([]byte, 32)
	cipher, err := NewErrorDiagnosticBodyCipher(key, 1)
	require.NoError(t, err)
	_, err = cipher.Decrypt([]byte{1, 2, 3})
	require.Error(t, err)
	_, err = cipher.Decrypt(nil)
	require.Error(t, err)
}

// TestErrorDiagnosticRepository_CountHonorsMetadataExpiry 覆盖 total 必须与列表同源：
// 只统计未过第 30 天的行，且协议过滤与列表完全一致。
func TestErrorDiagnosticRepository_CountHonorsMetadataExpiry(t *testing.T) {
	ctx := context.Background()
	db, mock, cipher := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, cipher)
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`(?s)SELECT COUNT\(\*\)\s+FROM error_diagnostic_records\s+WHERE metadata_expires_at > \$1\s+AND \(\$2 = '' OR protocol = \$2\)`).
		WithArgs(now, service.ErrorDiagnosticProtocolMessages).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(4)))

	total, err := repo.CountRecentErrorDiagnostics(ctx, service.ErrorDiagnosticProtocolMessages, now)
	require.NoError(t, err)
	require.EqualValues(t, 4, total)

	// 不限协议：协议参数为空字符串，SQL 仍只受元数据到期约束。
	mock.ExpectQuery(`(?s)SELECT COUNT\(\*\)\s+FROM error_diagnostic_records`).
		WithArgs(now, "").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(9)))
	total, err = repo.CountRecentErrorDiagnostics(ctx, "", now)
	require.NoError(t, err)
	require.EqualValues(t, 9, total)

	// 计数失败必须冒泡，不得伪装成 0：管理端会把 0 当成「没有记录」。
	countErr := errors.New("count failed")
	mock.ExpectQuery(`(?s)SELECT COUNT\(\*\)`).WithArgs(now, "").WillReturnError(countErr)
	_, err = repo.CountRecentErrorDiagnostics(ctx, "", now)
	require.ErrorIs(t, err, countErr)

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestErrorDiagnosticRepository_PageUsesBoundedWindow 覆盖分页在 SQL 层
// 就排除已到期行，避免翻页时出现错位或重复。
func TestErrorDiagnosticRepository_PageUsesBoundedWindow(t *testing.T) {
	ctx := context.Background()
	db, mock, cipher := newErrorDiagnosticSQLMock(t)
	repo := NewErrorDiagnosticRepository(db, cipher)
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	created := now.Add(-time.Hour)
	id := validErrorDiagnosticID(t)

	mock.ExpectQuery(`(?s)SELECT.*FROM error_diagnostic_records\s+WHERE metadata_expires_at > \$1\s+AND \(\$2 = '' OR protocol = \$2\)\s+ORDER BY created_at DESC, diagnostic_id DESC\s+OFFSET \$3 LIMIT \$4`).
		WithArgs(now, service.ErrorDiagnosticProtocolMessages, 40, 20).
		WillReturnRows(errorDiagnosticSelectColumns().
			AddRow(id, nil, service.ErrorDiagnosticProtocolMessages, 0, service.ErrorDiagnosticStageWire, 500,
				service.ErrorDiagnosticBodyStateNotObserved, service.ErrorDiagnosticBodyNotObserved,
				false, 0, 0, created, created.Add(service.ErrorDiagnosticMetadataRetention), nil,
				service.ErrorDiagnosticHeaderStateNotObserved, service.ErrorDiagnosticHeaderNotObserved,
				false, 0, 0, 0, nil))

	page, err := repo.ListRecentErrorDiagnosticPage(ctx, service.ErrorDiagnosticProtocolMessages, now, 40, 20)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, id, page[0].ID)

	// 偏移必须原样传给 SQL：一旦被夹到较小值，管理端会把上一页的行当成下一页返回。
	mock.ExpectQuery(`(?s)SELECT.*FROM error_diagnostic_records\s+WHERE metadata_expires_at > \$1\s+AND \(\$2 = '' OR protocol = \$2\)\s+ORDER BY created_at DESC, diagnostic_id DESC\s+OFFSET \$3 LIMIT \$4`).
		WithArgs(now, "", 800, 100).
		WillReturnRows(errorDiagnosticSelectColumns())
	deep, err := repo.ListRecentErrorDiagnosticPage(ctx, "", now, 800, 100)
	require.NoError(t, err)
	require.Empty(t, deep)

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestProvideErrorDiagnosticBodyCipher_FailsClosedWithoutAKey 覆盖依赖注入路径的安全要求：
// 缺密钥时提供 nil 加密器（正文一律不留存），绝不合成明文、绝不用不安全默认密钥，
// 也绝不让整个服务因为一个默认关闭的功能而起不来。
func TestProvideErrorDiagnosticBodyCipher_FailsClosedWithoutAKey(t *testing.T) {
	// 没有配置：返回 nil，而不是 panic 或默认密钥。
	require.Nil(t, ProvideErrorDiagnosticBodyCipher(nil))

	empty := &config.Config{}
	require.Nil(t, ProvideErrorDiagnosticBodyCipher(empty))

	short := &config.Config{}
	short.Totp.EncryptionKey = "abcd"
	require.Nil(t, ProvideErrorDiagnosticBodyCipher(short))

	notHex := &config.Config{}
	notHex.Totp.EncryptionKey = strings.Repeat("zz", 32)
	require.Nil(t, ProvideErrorDiagnosticBodyCipher(notHex))

	// 自动生成的密钥（未手动配置）：不可用于留存。
	// 它换个进程就变，用它留存正文会在重启后留下永远解不开的密文，
	// 而接口仍然承诺「7 天内可查看」——那是静默的数据损失，必须 fail closed。
	autoGenerated := &config.Config{}
	autoGenerated.Totp.EncryptionKey = strings.Repeat("ab", 32)
	autoGenerated.Totp.EncryptionKeyConfigured = false
	require.Nil(t, ProvideErrorDiagnosticBodyCipher(autoGenerated),
		"未手动配置的主密钥不得用于留存正文")

	// 显式配置的密钥：返回可用的加密器，且能往返。
	valid := &config.Config{}
	valid.Totp.EncryptionKey = strings.Repeat("ab", 32)
	valid.Totp.EncryptionKeyConfigured = true
	cipher := ProvideErrorDiagnosticBodyCipher(valid)
	require.NotNil(t, cipher)

	plaintext := []byte(`{"model":"claude"}`)
	sealed, err := cipher.Encrypt(plaintext)
	require.NoError(t, err)
	require.NotEqual(t, plaintext, sealed, "落库的必须是密文")
	opened, err := cipher.Decrypt(sealed)
	require.NoError(t, err)
	require.Equal(t, plaintext, opened)

	// 关键：缺密钥时诊断服务必须仍然以「元数据可写、正文不留存」工作，而不是整体失败。
	repo := newErrorDiagnosticBlockingCreateRepo()
	svc := service.NewErrorDiagnosticService(repo, errorDiagnosticSettingsAllowing{}, nil)
	attempt := service.ErrorDiagnosticAttempt{
		Protocol:           service.ErrorDiagnosticProtocolMessages,
		Stage:              service.ErrorDiagnosticStageWire,
		UpstreamStatusCode: 500,
		Body:               []byte(`{"model":"claude"}`),
		BodyReadComplete:   true,
		BodyVerdict:        service.ErrorDiagnosticBodyVerdictComplete,
	}
	record, err := svc.RecordErrorDiagnostic(context.Background(), attempt)
	require.NoError(t, err, "缺密钥不得让诊断写入整体失败")
	require.Equal(t, service.ErrorDiagnosticBodyStateSkipped, record.BodyState)
	require.Equal(t, service.ErrorDiagnosticBodySkippedEncryptionUnavailable, record.BodyReason)
}

type errorDiagnosticSettingsAllowing struct{}

func (errorDiagnosticSettingsAllowing) GetErrorDiagnosticSettings(context.Context) (service.ErrorDiagnosticSettings, error) {
	return service.ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true}, nil
}

// errorDiagnosticBlockingCreateRepo 只实现写入所需的最小行为，用于断言落库决定。
type errorDiagnosticBlockingCreateRepo struct {
	service.ErrorDiagnosticRepository
	created []service.ErrorDiagnosticWrite
}

func (r *errorDiagnosticBlockingCreateRepo) CreateErrorDiagnostic(_ context.Context, write service.ErrorDiagnosticWrite, now time.Time) (service.ErrorDiagnosticRecord, error) {
	r.created = append(r.created, write)
	return service.ErrorDiagnosticRecord{
		ID:                 write.ID,
		Protocol:           write.Attempt.Protocol,
		AttemptIndex:       write.Attempt.AttemptIndex,
		Stage:              write.Attempt.Stage,
		UpstreamStatusCode: write.Attempt.UpstreamStatusCode,
		BodyState:          write.BodyState,
		BodyReason:         write.BodyReason,
		CreatedAt:          now,
		MetadataExpiresAt:  now.Add(service.ErrorDiagnosticMetadataRetention),
	}, nil
}

func newErrorDiagnosticBlockingCreateRepo() *errorDiagnosticBlockingCreateRepo {
	return &errorDiagnosticBlockingCreateRepo{}
}

// TestErrorDiagnosticRepository_BodyExpiryIsBoundOnlyWithCiphertext 覆盖
// 「密文与正文到期时刻成对」这一不变量，并锁定到期时刻的取值来源：
// 该值过去通过无检查的类型断言取回，现在改为类型化的 sql.NullTime，
// 因此既不可能 panic，也不可能出现「有密文却没有到期时刻」的行。
func TestErrorDiagnosticRepository_BodyExpiryIsBoundOnlyWithCiphertext(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	wantExpiry := now.Add(service.ErrorDiagnosticBodyRetention)

	// 情形一：留存正文 —— 绑定密文，并绑定恰好 7 天后的到期时刻。
	t.Run("retained body binds a matching expiry", func(t *testing.T) {
		db, mock, cipher := newErrorDiagnosticSQLMock(t)
		repo := NewErrorDiagnosticRepository(db, cipher)

		plaintext := []byte(`{"model":"claude"}`)
		ciphertext, err := cipher.Encrypt(plaintext)
		require.NoError(t, err)

		write := errorDiagnosticWriteFixture(t)
		write.Attempt.Body = plaintext
		write.BodyState = service.ErrorDiagnosticBodyStateStored
		write.BodyReason = service.ErrorDiagnosticBodyRetained
		write.BodyCiphertext = ciphertext
		write.BodyKeyVersion = 2

		mock.ExpectQuery(`(?s)INSERT INTO error_diagnostic_records.*RETURNING created_at`).
			WithArgs(
				write.ID, nil, service.ErrorDiagnosticProtocolMessages, 1, service.ErrorDiagnosticStageWire, 503,
				service.ErrorDiagnosticBodyStateStored, service.ErrorDiagnosticBodyRetained,
				ciphertext, 2, len(plaintext),
				now, now.Add(service.ErrorDiagnosticMetadataRetention), sql.NullTime{Time: wantExpiry, Valid: true},
				service.ErrorDiagnosticHeaderStateNotObserved, service.ErrorDiagnosticHeaderNotObserved,
				nil, 0, 0, 0, nil,
			).
			WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))

		record, err := repo.CreateErrorDiagnostic(ctx, write, now)
		require.NoError(t, err)
		require.True(t, record.BodyStored)
		require.Equal(t, wantExpiry, record.BodyExpiresAt, "到期时刻必须与绑定的值一致")
		require.NoError(t, mock.ExpectationsWereMet())
	})

	// 情形二：不留存正文 —— 不绑定密文，到期时刻绑定 SQL NULL，记录保持零值。
	t.Run("skipped body binds null and a zero expiry", func(t *testing.T) {
		db, mock, cipher := newErrorDiagnosticSQLMock(t)
		repo := NewErrorDiagnosticRepository(db, cipher)

		write := errorDiagnosticWriteFixture(t)
		write.BodyState = service.ErrorDiagnosticBodyStateSkipped
		write.BodyReason = service.ErrorDiagnosticBodySkippedTooLarge

		mock.ExpectQuery(`(?s)INSERT INTO error_diagnostic_records.*RETURNING created_at`).
			WithArgs(
				write.ID, nil, service.ErrorDiagnosticProtocolMessages, 1, service.ErrorDiagnosticStageWire, 503,
				service.ErrorDiagnosticBodyStateSkipped, service.ErrorDiagnosticBodySkippedTooLarge,
				nil, 0, 0,
				now, now.Add(service.ErrorDiagnosticMetadataRetention), sql.NullTime{},
				service.ErrorDiagnosticHeaderStateNotObserved, service.ErrorDiagnosticHeaderNotObserved,
				nil, 0, 0, 0, nil,
			).
			WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))

		record, err := repo.CreateErrorDiagnostic(ctx, write, now)
		require.NoError(t, err)
		require.False(t, record.BodyStored)
		require.True(t, record.BodyExpiresAt.IsZero(), "未留存正文时到期时刻必须为零值，与绑定的 NULL 一致")
		require.NoError(t, mock.ExpectationsWereMet())
	})

	// 情形三：自称已留存但没有密文 —— 被降级为未留存，因此也不绑定到期时刻。
	t.Run("claimed stored without ciphertext binds null", func(t *testing.T) {
		db, mock, cipher := newErrorDiagnosticSQLMock(t)
		repo := NewErrorDiagnosticRepository(db, cipher)

		write := errorDiagnosticWriteFixture(t)
		write.BodyState = service.ErrorDiagnosticBodyStateStored
		write.BodyReason = service.ErrorDiagnosticBodyRetained

		mock.ExpectQuery(`(?s)INSERT INTO error_diagnostic_records.*RETURNING created_at`).
			WithArgs(
				write.ID, nil, service.ErrorDiagnosticProtocolMessages, 1, service.ErrorDiagnosticStageWire, 503,
				service.ErrorDiagnosticBodyStateSkipped, service.ErrorDiagnosticBodySkippedEncryptionUnavailable,
				nil, 0, 0,
				now, now.Add(service.ErrorDiagnosticMetadataRetention), sql.NullTime{},
				service.ErrorDiagnosticHeaderStateNotObserved, service.ErrorDiagnosticHeaderNotObserved,
				nil, 0, 0, 0, nil,
			).
			WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))

		record, err := repo.CreateErrorDiagnostic(ctx, write, now)
		require.NoError(t, err)
		require.True(t, record.BodyExpiresAt.IsZero())
		require.False(t, record.BodyStored)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
