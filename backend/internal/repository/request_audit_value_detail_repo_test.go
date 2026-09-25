//go:build unit

package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newValueDetailSQLMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock, service.RequestAuditValueDetailCipher) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	valueCipher, err := NewRequestAuditValueDetailCipher(key, 3)
	require.NoError(t, err)
	return db, mock, valueCipher
}

func valueDetailStoredWrite(t *testing.T, payload []byte) service.RequestAuditValueDetailWrite {
	t.Helper()
	now := time.Now().UTC()
	return service.RequestAuditValueDetailWrite{
		UsageLogID: 77,
		State:      service.RequestAuditValueDetailStateStored,
		Reason:     service.RequestAuditValueDetailRetained,
		Fields: service.RequestAuditValueDetailFields{
			Route:        service.RequestAuditValueDetailRouteMessages,
			Protocol:     service.RequestAuditProtocolAnthropic,
			ClientStatus: 200,
			StartedAt:    now,
		},
		Payload:      payload,
		AttemptCount: 2,
		EntryCount:   5,
		ExpiresAt:    now.Add(service.RequestAuditValueDetailRetention),
	}
}

// 写入必须是同一条语句里的 EXISTS 守卫：审计行不存在时不写任何行。
func TestCreateRequestAuditValueDetailGuardsOnExistingAuditRow(t *testing.T) {
	db, mock, valueCipher := newValueDetailSQLMock(t)
	repo := NewRequestAuditValueDetailRepository(db, valueCipher)
	payload, err := service.EncodeRequestAuditValueDetailValues(service.RequestAuditValueDetailValues{Model: "m"})
	require.NoError(t, err)

	mock.ExpectQuery(`INSERT INTO request_audit_value_details`).
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(time.Now().UTC()))

	detail, err := repo.CreateRequestAuditValueDetail(context.Background(), valueDetailStoredWrite(t, payload))
	require.NoError(t, err)
	require.True(t, detail.Stored)
	require.Equal(t, service.RequestAuditValueDetailRetained, detail.Reason)
	require.Equal(t, 5, detail.EntryCount)
	require.Equal(t, len(payload), detail.PayloadBytes)
	require.Equal(t, 3, detail.KeyVersion)
	require.NoError(t, mock.ExpectationsWereMet())

	// 断言语句里确实带了 EXISTS 守卫，而不是先查后写（先查后写会留下竞态窗口）。
	require.Contains(t, requestAuditValueDetailInsertStatement,
		"WHERE EXISTS (SELECT 1 FROM request_audits WHERE usage_log_id = $1)")
	require.Contains(t, requestAuditValueDetailInsertStatement, "ON CONFLICT (usage_log_id) DO NOTHING")
}

// 审计行不存在（或已经写过）时没有行返回：这是幂等空操作，不是写入成功，也不是失败。
func TestCreateRequestAuditValueDetailWithoutAuditRowIsNoOp(t *testing.T) {
	db, mock, valueCipher := newValueDetailSQLMock(t)
	repo := NewRequestAuditValueDetailRepository(db, valueCipher)
	payload, err := service.EncodeRequestAuditValueDetailValues(service.RequestAuditValueDetailValues{Model: "m"})
	require.NoError(t, err)

	mock.ExpectQuery(`INSERT INTO request_audit_value_details`).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}))
	detail, err := repo.CreateRequestAuditValueDetail(context.Background(), valueDetailStoredWrite(t, payload))
	require.NoError(t, err)
	require.Zero(t, detail.UsageLogID)
	require.False(t, detail.Stored)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 没有密钥时限明文也不落库：写入必须退化成 skipped_encryption_unavailable。
func TestCreateRequestAuditValueDetailWithoutCipherDegradesToSkipped(t *testing.T) {
	db, mock, _ := newValueDetailSQLMock(t)
	repo := NewRequestAuditValueDetailRepository(db, nil)
	payload, err := service.EncodeRequestAuditValueDetailValues(service.RequestAuditValueDetailValues{Model: "m"})
	require.NoError(t, err)

	mock.ExpectQuery(`INSERT INTO request_audit_value_details`).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(time.Now().UTC()))
	detail, err := repo.CreateRequestAuditValueDetail(context.Background(), valueDetailStoredWrite(t, payload))
	require.NoError(t, err)
	require.False(t, detail.Stored)
	require.Equal(t, service.RequestAuditValueDetailStateSkipped, detail.State)
	require.Equal(t, service.RequestAuditValueDetailSkippedEncryptionUnavailable, detail.Reason)
	require.Zero(t, detail.PayloadBytes)
	require.Zero(t, detail.KeyVersion)
	require.Zero(t, detail.EntryCount)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 空载荷也不能落成「已留存」。
func TestCreateRequestAuditValueDetailRejectsEmptyPayload(t *testing.T) {
	db, mock, valueCipher := newValueDetailSQLMock(t)
	repo := NewRequestAuditValueDetailRepository(db, valueCipher)

	mock.ExpectQuery(`INSERT INTO request_audit_value_details`).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(time.Now().UTC()))
	detail, err := repo.CreateRequestAuditValueDetail(context.Background(), valueDetailStoredWrite(t, nil))
	require.NoError(t, err)
	require.False(t, detail.Stored)
	require.Equal(t, service.RequestAuditValueDetailSkippedEncryptionUnavailable, detail.Reason)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetRequestAuditValueDetail(t *testing.T) {
	db, mock, valueCipher := newValueDetailSQLMock(t)
	repo := NewRequestAuditValueDetailRepository(db, valueCipher)
	now := time.Now().UTC()

	mock.ExpectQuery(`FROM request_audit_value_details WHERE usage_log_id`).
		WithArgs(int64(77)).
		WillReturnRows(sqlmock.NewRows([]string{
			"usage_log_id", "state", "reason", "route", "protocol", "client_status",
			"attempt_count", "entry_count", "payload_bytes", "key_version",
			"stored", "started_at", "completed_at", "expires_at", "created_at",
		}).AddRow(int64(77), "stored", "retained", "/v1/messages", "anthropic.messages", 200,
			2, 5, 128, 3, true, now, nil, now.Add(service.RequestAuditValueDetailRetention), now))

	detail, err := repo.GetRequestAuditValueDetail(context.Background(), 77)
	require.NoError(t, err)
	require.Equal(t, int64(77), detail.UsageLogID)
	require.True(t, detail.Stored)
	require.Equal(t, service.RequestAuditValueDetailRouteMessages, detail.Fields.Route)
	require.True(t, detail.Readable(now))
	require.False(t, detail.Expired(now))
	require.True(t, detail.Readable(now.Add(-time.Hour)))
	require.False(t, detail.Readable(now.Add(service.RequestAuditValueDetailRetention+time.Hour)))
	require.Equal(t, now.Unix(), detail.Fields.StartedAt.Unix())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetRequestAuditValueDetailNotFound(t *testing.T) {
	db, mock, valueCipher := newValueDetailSQLMock(t)
	repo := NewRequestAuditValueDetailRepository(db, valueCipher)

	mock.ExpectQuery(`FROM request_audit_value_details`).
		WillReturnRows(sqlmock.NewRows([]string{"usage_log_id"}))
	_, err := repo.GetRequestAuditValueDetail(context.Background(), 77)
	require.ErrorIs(t, err, service.ErrRequestAuditValueDetailNotFound)
	require.NoError(t, mock.ExpectationsWereMet())

	_, err = repo.GetRequestAuditValueDetail(context.Background(), 0)
	require.ErrorIs(t, err, service.ErrRequestAuditValueDetailNotFound)
}

// 解密成功不等于内容可信，且到期、清除与缺密钥必须分别映射到稳定错误。
func TestReadRequestAuditValueDetailValues(t *testing.T) {
	db, mock, valueCipher := newValueDetailSQLMock(t)
	repo := NewRequestAuditValueDetailRepository(db, valueCipher)
	now := time.Now().UTC()

	payload, err := service.EncodeRequestAuditValueDetailValues(service.RequestAuditValueDetailValues{Model: "claude-sonnet-4-5"})
	require.NoError(t, err)
	ciphertext, err := valueCipher.Encrypt(payload)
	require.NoError(t, err)

	mock.ExpectQuery(`SELECT ciphertext, expires_at FROM request_audit_value_details`).
		WillReturnRows(sqlmock.NewRows([]string{"ciphertext", "expires_at"}).
			AddRow(ciphertext, now.Add(time.Hour)))
	values, err := repo.ReadRequestAuditValueDetailValues(context.Background(), 77, now)
	require.NoError(t, err)
	require.Equal(t, "claude-sonnet-4-5", values.Model)

	// 已到期：即使密文还在，读取也必须拒绝。
	mock.ExpectQuery(`SELECT ciphertext, expires_at FROM request_audit_value_details`).
		WillReturnRows(sqlmock.NewRows([]string{"ciphertext", "expires_at"}).
			AddRow(ciphertext, now.Add(-time.Second)))
	_, err = repo.ReadRequestAuditValueDetailValues(context.Background(), 77, now)
	require.ErrorIs(t, err, service.ErrRequestAuditValueDetailGone)

	// 已被清理（密文为 NULL）：同样是不可揭示，不是「从未留存」。
	mock.ExpectQuery(`SELECT ciphertext, expires_at FROM request_audit_value_details`).
		WillReturnRows(sqlmock.NewRows([]string{"ciphertext", "expires_at"}).
			AddRow(nil, now.Add(time.Hour)))
	_, err = repo.ReadRequestAuditValueDetailValues(context.Background(), 77, now)
	require.ErrorIs(t, err, service.ErrRequestAuditValueDetailGone)

	// 密文被篡改：认证失败即不可用，不返回部分明文。
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0xff
	mock.ExpectQuery(`SELECT ciphertext, expires_at FROM request_audit_value_details`).
		WillReturnRows(sqlmock.NewRows([]string{"ciphertext", "expires_at"}).
			AddRow(tampered, now.Add(time.Hour)))
	_, err = repo.ReadRequestAuditValueDetailValues(context.Background(), 77, now)
	require.ErrorIs(t, err, service.ErrRequestAuditValueDetailGone)

	// 有密文却没有密钥（配置被移除）：可重试的部署故障，不是「值已消失」。
	noCipherRepo := NewRequestAuditValueDetailRepository(db, nil)
	mock.ExpectQuery(`SELECT ciphertext, expires_at FROM request_audit_value_details`).
		WillReturnRows(sqlmock.NewRows([]string{"ciphertext", "expires_at"}).
			AddRow(ciphertext, now.Add(time.Hour)))
	_, err = noCipherRepo.ReadRequestAuditValueDetailValues(context.Background(), 77, now)
	require.ErrorIs(t, err, service.ErrRequestAuditValueDetailUnavailable)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 清理只置空密文列并记 purged，保留整行信封与到期时刻。
func TestClearExpiredRequestAuditValueDetails(t *testing.T) {
	db, mock, valueCipher := newValueDetailSQLMock(t)
	repo := NewRequestAuditValueDetailRepository(db, valueCipher)
	now := time.Now().UTC()

	mock.ExpectExec(`UPDATE request_audit_value_details`).
		WithArgs(sqlmock.AnyArg(), 500).
		WillReturnResult(sqlmock.NewResult(0, 7))
	cleared, err := repo.ClearExpiredRequestAuditValueDetails(context.Background(), now, 500)
	require.NoError(t, err)
	require.Equal(t, int64(7), cleared)

	// 非正批量上限是空操作，不是错误。
	cleared, err = repo.ClearExpiredRequestAuditValueDetails(context.Background(), now, 0)
	require.NoError(t, err)
	require.Zero(t, cleared)
	require.NoError(t, mock.ExpectationsWereMet())

	require.Contains(t, requestAuditValueDetailClearStatement, "SET ciphertext = NULL, key_version = 0, state = 'purged'")
	require.Contains(t, requestAuditValueDetailClearStatement, "ciphertext IS NOT NULL")
	require.Contains(t, requestAuditValueDetailClearStatement, "expires_at <= $1")
	require.NotContains(t, requestAuditValueDetailClearStatement, "DELETE")
}

// 存储不可用时必须报错，绝不能静默当成空结果。
func TestRequestAuditValueDetailRepositoryUnavailable(t *testing.T) {
	repo := NewRequestAuditValueDetailRepository(nil, nil)
	_, err := repo.GetRequestAuditValueDetail(context.Background(), 77)
	require.ErrorIs(t, err, service.ErrRequestAuditValueDetailUnavailable)
	_, err = repo.ReadRequestAuditValueDetailValues(context.Background(), 77, time.Now())
	require.ErrorIs(t, err, service.ErrRequestAuditValueDetailUnavailable)
	_, err = repo.ClearExpiredRequestAuditValueDetails(context.Background(), time.Now(), 10)
	require.ErrorIs(t, err, service.ErrRequestAuditValueDetailUnavailable)
	_, err = repo.CreateRequestAuditValueDetail(context.Background(), service.RequestAuditValueDetailWrite{UsageLogID: 1})
	require.ErrorIs(t, err, service.ErrRequestAuditValueDetailUnavailable)

	var nilRepo *requestAuditValueDetailRepository
	_, err = nilRepo.GetRequestAuditValueDetail(context.Background(), 1)
	require.ErrorIs(t, err, service.ErrRequestAuditValueDetailUnavailable)
}

// 加密器：专用子密钥派生必须稳定，且缺主密钥／格式不对时返回 nil（绝不明文回退）。
func TestProvideRequestAuditValueDetailCipher(t *testing.T) {
	require.Nil(t, ProvideRequestAuditValueDetailCipher(nil), "缺配置时不得回退为明文")

	// 自动生成的进程级密钥（EncryptionKeyConfigured=false）不可用：换进程即变。
	autoGenerated := &configStubConfig{key: "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20", configured: false}
	require.Nil(t, ProvideRequestAuditValueDetailCipher(autoGenerated.build()))

	// 手动配置的稳定密钥可用，且派生是确定性的（同配置两次结果一致）。
	stable := &configStubConfig{key: "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20", configured: true}
	first := ProvideRequestAuditValueDetailCipher(stable.build())
	second := ProvideRequestAuditValueDetailCipher(stable.build())
	require.NotNil(t, first)
	require.NotNil(t, second)
	require.Equal(t, first.KeyVersion(), second.KeyVersion())

	ciphertext, err := first.Encrypt([]byte("payload"))
	require.NoError(t, err)
	plaintext, err := second.Decrypt(ciphertext)
	require.NoError(t, err)
	require.Equal(t, "payload", string(plaintext))

	// 用途隔离：与错误诊断正文加密器不共用密钥材料（同一主密钥、不同 info 标签）。
	diagnosticCipher, err := NewErrorDiagnosticBodyCipherFromConfig(stable.build())
	require.NoError(t, err)
	_, err = diagnosticCipher.(*errorDiagnosticAESCipher).Decrypt(ciphertext)
	require.Error(t, err, "值明细密文不得能被诊断正文密钥解开")
}

// configStubConfig 只构造本测试需要的配置形状。
type configStubConfig struct {
	key        string
	configured bool
}

func (c *configStubConfig) build() *config.Config {
	return &config.Config{Totp: config.TotpConfig{EncryptionKey: c.key, EncryptionKeyConfigured: c.configured}}
}

// 无密钥加密器一律报错，绝不明文返回。
func TestRequestAuditValueDetailCipherWithoutKey(t *testing.T) {
	var cipher *requestAuditValueDetailAESCipher
	_, err := cipher.Encrypt([]byte("x"))
	require.Error(t, err)
	_, err = cipher.Decrypt([]byte("x"))
	require.Error(t, err)
	require.Zero(t, cipher.KeyVersion())

	_, err = NewRequestAuditValueDetailCipher(make([]byte, 16), 1)
	require.Error(t, err)
}
