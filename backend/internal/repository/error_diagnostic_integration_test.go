//go:build integration

package repository

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// 这些用例对真实 PostgreSQL 运行错误诊断的存储、到期与清理接缝。
//
// 夹具一律直接写 integrationDB / integrationEntClient（已提交），
// 因为被测仓储持有自己的连接，看不到未提交的 ent 事务；清理在 t.Cleanup 中按 id 删除。
// 与 usage-owned 请求审计不同，诊断仓储不使用 ent，也不进入 ent 事务。

// errorDiagnosticTestEncryptionKey 是集成夹具的稳定密钥材料（AES-256 的 32 字节）。
//
// 正文加密与门控读取用的是同一把材料：门控读取器靠配置里的稳定密钥判定「正文留存
// 此刻是否真的可用」，配不上就会把有效结论里的正文留存关掉。
func errorDiagnosticTestEncryptionKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i*7 + 1)
	}
	return key
}

func newErrorDiagnosticTestCipher(t *testing.T) service.ErrorDiagnosticBodyCipher {
	t.Helper()
	cipher, err := NewErrorDiagnosticBodyCipher(errorDiagnosticTestEncryptionKey(), 4)
	require.NoError(t, err)
	require.Equal(t, 4, cipher.KeyVersion())
	return cipher
}

func newErrorDiagnosticTestService(t *testing.T, settings service.ErrorDiagnosticSettings) (*service.ErrorDiagnosticService, *service.ErrorDiagnosticCleanupService) {
	t.Helper()
	cipher := newErrorDiagnosticTestCipher(t)
	repo := NewErrorDiagnosticRepository(integrationDB, cipher)
	settingsRepo := NewSettingRepository(integrationEntClient)

	require.NoError(t, setErrorDiagnosticTestSettings(t, settingsRepo, settings))
	if settings.CaptureAllowed() {
		// 门控打开还需要书面确认：采集侧判定点是「存量布尔值 且 当前版本的有效确认」，
		// 只写门控键会让所有采集用例停在 ErrErrorDiagnosticDisabled（见
		// ApplyErrorDiagnosticRiskAcknowledgement）。夹具据此补上确认记录。
		require.NoError(t, setErrorDiagnosticTestRiskAcknowledgement(t, settingsRepo))
	}

	// 门控由真实设置服务读取，走与生产一致的 GetErrorDiagnosticSettings 路径；
	// 配置里带上与测试 cipher 同一把稳定密钥，正文留存才会被判定为真的可用
	// （缺密钥时有效结论会关掉正文留存，见 applyErrorDiagnosticBodyRetentionKeyAvailability）。
	settingsSvc := service.NewSettingService(settingsRepo, &config.Config{Totp: config.TotpConfig{
		EncryptionKey:           hex.EncodeToString(errorDiagnosticTestEncryptionKey()),
		EncryptionKeyConfigured: true,
	}})
	svc := service.NewErrorDiagnosticService(repo, settingsSvc, cipher)
	cleanup := service.NewErrorDiagnosticCleanupService(repo, svc.Metrics())
	t.Cleanup(func() {
		_ = settingsRepo.Delete(context.Background(), service.SettingKeyErrorDiagnostic)
		_ = settingsRepo.Delete(context.Background(), service.SettingKeyErrorDiagnosticRiskAcknowledgement)
	})
	return svc, cleanup
}

// setErrorDiagnosticTestRiskAcknowledgement 写入一条覆盖当前语句版本的有效书面确认。
//
// 语句原文必须与当前版本语句**逐字**相同（见 CoversCurrentStatement），因此这里直接取
// 生产常量，而不是自造一句话：自造原文会被判为不覆盖当前语句，采集仍然关闭。
func setErrorDiagnosticTestRiskAcknowledgement(t *testing.T, repo service.SettingRepository) error {
	t.Helper()
	payload, err := json.Marshal(service.ErrorDiagnosticRiskAcknowledgement{
		Version:     service.ErrorDiagnosticRiskAcknowledgementVersion,
		Phrase:      service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
		AdminUserID: 7,
		AcceptedAt:  time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		return err
	}
	return repo.Set(context.Background(), service.SettingKeyErrorDiagnosticRiskAcknowledgement, string(payload))
}

func setErrorDiagnosticTestSettings(t *testing.T, repo service.SettingRepository, settings service.ErrorDiagnosticSettings) error {
	t.Helper()
	if !settings.Enabled && !settings.RiskAcknowledged && !settings.BodyRetentionEnabled {
		// 显式写入「全关」，避免受共享测试库里其他设置的影响。
		return repo.Set(context.Background(), service.SettingKeyErrorDiagnostic, `{"enabled":false}`)
	}
	raw := `{"enabled":true,"risk_acknowledged":true`
	if settings.BodyRetentionEnabled {
		raw += `,"body_retention_enabled":true`
	}
	raw += `}`
	return repo.Set(context.Background(), service.SettingKeyErrorDiagnostic, raw)
}

func errorDiagnosticMessagesAttempt() service.ErrorDiagnosticAttempt {
	return service.ErrorDiagnosticAttempt{
		Protocol:           service.ErrorDiagnosticProtocolMessages,
		Stage:              service.ErrorDiagnosticStageWire,
		UpstreamStatusCode: 400,
	}
}

func countErrorDiagnosticRows(t *testing.T, id string) int {
	t.Helper()
	var count int
	require.NoError(t, integrationDB.QueryRow(
		`SELECT COUNT(*) FROM error_diagnostic_records WHERE diagnostic_id = $1`, id).Scan(&count))
	return count
}

func errorDiagnosticRowFacts(t *testing.T, id string) (bodyCiphertext []byte, bodyBytes int, usageLogID sql.NullInt64, metadataExpiresAt, bodyExpiresAt sql.NullTime) {
	t.Helper()
	require.NoError(t, integrationDB.QueryRow(`
		SELECT body_ciphertext, body_bytes, usage_log_id, metadata_expires_at, body_expires_at
		FROM error_diagnostic_records WHERE diagnostic_id = $1`, id).
		Scan(&bodyCiphertext, &bodyBytes, &usageLogID, &metadataExpiresAt, &bodyExpiresAt))
	return
}

func backdateErrorDiagnostic(t *testing.T, id string, age time.Duration) {
	t.Helper()
	// 显式按秒做减法，避免 pq 把未标注的参数推断成 interval。
	_, err := integrationDB.Exec(`
		UPDATE error_diagnostic_records
		SET created_at = created_at - ($2::double precision * INTERVAL '1 second'),
		    metadata_expires_at = metadata_expires_at - ($2::double precision * INTERVAL '1 second'),
		    body_expires_at = body_expires_at - ($2::double precision * INTERVAL '1 second')
		WHERE diagnostic_id = $1`, id, age.Seconds())
	require.NoError(t, err, "模拟时间推进")
}

func deleteErrorDiagnosticRows(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if id == "" {
			continue
		}
		_, err := integrationDB.Exec(`DELETE FROM error_diagnostic_records WHERE diagnostic_id = $1`, id)
		require.NoError(t, err)
	}
}

// TestErrorDiagnostic_BodyCipherRejectsTamperedAndForeignCiphertext 覆盖加密必须认证：
// 被改写的密文或换错密钥都不得返回明文。
func TestErrorDiagnostic_BodyCipherRejectsTamperedAndForeignCiphertext(t *testing.T) {
	cipher := newErrorDiagnosticTestCipher(t)
	plaintext := []byte(`{"model":"claude","messages":[{"role":"user","content":"sentinel-body-42"}]}`)

	ciphertext, err := cipher.Encrypt(plaintext)
	require.NoError(t, err)
	require.NotEmpty(t, ciphertext)
	require.NotEqual(t, plaintext, ciphertext, "密文不得等于明文")
	require.False(t, bytes.Contains(ciphertext, []byte("sentinel-body-42")), "密文不得包含明文片段")

	// 同一密钥可以还原出逐字节一致的内容。
	decrypted, err := cipher.Decrypt(ciphertext)
	require.NoError(t, err)
	require.Equal(t, plaintext, decrypted)

	// 每个密文使用独立 nonce，同一明文的两次加密结果不同。
	second, err := cipher.Encrypt(plaintext)
	require.NoError(t, err)
	require.NotEqual(t, ciphertext, second)

	// 篡改任何字节都必须解密失败，而不是返回被改坏的明文。
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0x01
	_, err = cipher.Decrypt(tampered)
	require.Error(t, err)

	require.Error(t, func() error {
		_, decryptErr := cipher.Decrypt(ciphertext[:4])
		return decryptErr
	}())

	// 换错密钥不得解出明文。
	foreignKey := make([]byte, 32)
	_, err = rand.Read(foreignKey)
	require.NoError(t, err)
	foreign, err := NewErrorDiagnosticBodyCipher(foreignKey, 4)
	require.NoError(t, err)
	_, err = foreign.Decrypt(ciphertext)
	require.Error(t, err)

	// 密钥长度必须是 32 字节。
	_, err = NewErrorDiagnosticBodyCipher([]byte("short"), 1)
	require.Error(t, err)
}

// TestErrorDiagnostic_MetadataWriteLandsWithoutBodyOnPrimary 覆盖票 01：
// 安全元数据落库，且主库行上没有任何正文列。
func TestErrorDiagnostic_MetadataWriteLandsWithoutBodyOnPrimary(t *testing.T) {
	ctx := context.Background()
	svc, _ := newErrorDiagnosticTestService(t, service.ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true})

	attempt := errorDiagnosticMessagesAttempt()
	attempt.UpstreamStatusCode = 500

	record, err := svc.RecordErrorDiagnostic(ctx, attempt)
	require.NoError(t, err)
	t.Cleanup(func() { deleteErrorDiagnosticRows(t, record.ID) })

	require.True(t, service.ValidErrorDiagnosticID(record.ID))
	require.False(t, record.HasUsage)
	require.Equal(t, service.ErrorDiagnosticBodyStateNotObserved, record.BodyState)
	require.Equal(t, service.ErrorDiagnosticBodyNotObserved, record.BodyReason)
	require.True(t, record.BodyExpiresAt.IsZero())

	ciphertext, bodyBytes, usageLogID, metadataExpiresAt, bodyExpiresAt := errorDiagnosticRowFacts(t, record.ID)
	require.Empty(t, ciphertext, "票 01 不得在主库写入正文")
	require.Zero(t, bodyBytes)
	require.False(t, usageLogID.Valid, "无 usage 的失败必须保持关联为空")
	require.False(t, bodyExpiresAt.Valid)

	// 元数据到期严格等于创建后 30 天。
	require.True(t, metadataExpiresAt.Valid)
	require.WithinDuration(t, record.CreatedAt.Add(service.ErrorDiagnosticMetadataRetention), metadataExpiresAt.Time, 2*time.Second)

	// 独立入口可查，且不是伪造用量。
	found, err := svc.ListRecentErrorDiagnostics(ctx, service.ErrorDiagnosticProtocolMessages, 50)
	require.NoError(t, err)
	require.True(t, containsErrorDiagnosticID(found, record.ID))

	// 该诊断不得凭空产生使用记录。
	require.Zero(t, countUsageLogsForDiagnostic(t, record))
}

func countUsageLogsForDiagnostic(t *testing.T, record service.ErrorDiagnosticRecord) int {
	t.Helper()
	var count int
	require.NoError(t, integrationDB.QueryRow(`SELECT COUNT(*) FROM usage_logs WHERE request_id = $1`, record.ID).Scan(&count))
	return count
}

func containsErrorDiagnosticID(records []service.ErrorDiagnosticRecord, id string) bool {
	for _, record := range records {
		if record.ID == id {
			return true
		}
	}
	return false
}

// TestErrorDiagnostic_EachFailedAttemptIsRecordedIndependently 覆盖
// 「先 400 后重试成功」时每次尝试各有一行，且指纹／尝试序号可区分。
func TestErrorDiagnostic_EachFailedAttemptIsRecordedIndependently(t *testing.T) {
	ctx := context.Background()
	svc, _ := newErrorDiagnosticTestService(t, service.ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true})

	first := errorDiagnosticMessagesAttempt()
	first.UpstreamStatusCode = 400
	first.AttemptIndex = 0

	second := errorDiagnosticMessagesAttempt()
	second.UpstreamStatusCode = 429
	second.AttemptIndex = 1

	firstRecord, err := svc.RecordErrorDiagnostic(ctx, first)
	require.NoError(t, err)
	secondRecord, err := svc.RecordErrorDiagnostic(ctx, second)
	require.NoError(t, err)
	t.Cleanup(func() { deleteErrorDiagnosticRows(t, firstRecord.ID, secondRecord.ID) })

	require.NotEqual(t, firstRecord.ID, secondRecord.ID, "重复尝试必须各有独立标识")

	stored, err := svc.GetErrorDiagnostic(ctx, firstRecord.ID)
	require.NoError(t, err)
	require.Equal(t, 400, stored.UpstreamStatusCode)
	require.Equal(t, 0, stored.AttemptIndex)

	storedSecond, err := svc.GetErrorDiagnostic(ctx, secondRecord.ID)
	require.NoError(t, err)
	require.Equal(t, 429, storedSecond.UpstreamStatusCode)
	require.Equal(t, 1, storedSecond.AttemptIndex)
}

// TestErrorDiagnostic_UncoveredOrLocalFailuresWriteNothing 覆盖
// 非覆盖分支、状态码越界、缺少上游状态都不得生成假 4xx/5xx 诊断。
func TestErrorDiagnostic_UncoveredOrLocalFailuresWriteNothing(t *testing.T) {
	ctx := context.Background()
	svc, _ := newErrorDiagnosticTestService(t, service.ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true})

	before := countAllErrorDiagnosticRows(t)

	uncovered := errorDiagnosticMessagesAttempt()
	uncovered.Protocol = "antigravity.generate"
	_, err := svc.RecordErrorDiagnostic(ctx, uncovered)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticInvalidAttempt)

	noStatus := errorDiagnosticMessagesAttempt()
	noStatus.UpstreamStatusCode = 0 // 连接故障／本地拒绝没有上游状态
	_, err = svc.RecordErrorDiagnostic(ctx, noStatus)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticInvalidAttempt)

	localRejection := errorDiagnosticMessagesAttempt()
	localRejection.UpstreamStatusCode = 200 // 上游成功不是失败尝试
	_, err = svc.RecordErrorDiagnostic(ctx, localRejection)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticInvalidAttempt)

	require.Equal(t, before, countAllErrorDiagnosticRows(t), "不合格输入不得产生任何行")
}

func countAllErrorDiagnosticRows(t *testing.T) int {
	t.Helper()
	var count int
	require.NoError(t, integrationDB.QueryRow(`SELECT COUNT(*) FROM error_diagnostic_records`).Scan(&count))
	return count
}

// TestErrorDiagnostic_BodyRetentionAndPrimaryPurge 覆盖票 02：正文加密留存、
// 逐字节一致、第 7 天 API 拒绝读取并在主库物理清除密文，而元数据仍保留到第 30 天。
func TestErrorDiagnostic_BodyRetentionAndPrimaryPurge(t *testing.T) {
	ctx := context.Background()
	svc, cleanup := newErrorDiagnosticTestService(t, service.ErrorDiagnosticSettings{
		Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true,
	})

	plaintext := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"diagnostic-sentinel-7f3a"}]}`)
	attempt := errorDiagnosticMessagesAttempt()
	attempt.UpstreamStatusCode = 503
	attempt.Body = plaintext
	attempt.BodyReadComplete = true

	record, err := svc.RecordErrorDiagnostic(ctx, attempt)
	require.NoError(t, err)
	t.Cleanup(func() { deleteErrorDiagnosticRows(t, record.ID) })

	require.Equal(t, service.ErrorDiagnosticBodyStateStored, record.BodyState)
	require.Equal(t, service.ErrorDiagnosticBodyRetained, record.BodyReason)
	require.Equal(t, len(plaintext), record.BodyBytes)

	// 主库里必须是密文，且不含明文 sentinel。
	ciphertext, bodyBytes, _, _, bodyExpiresAt := errorDiagnosticRowFacts(t, record.ID)
	require.NotEmpty(t, ciphertext)
	require.False(t, bytes.Contains(ciphertext, []byte("diagnostic-sentinel-7f3a")), "主库不得出现明文 sentinel")
	require.Equal(t, len(plaintext), bodyBytes)
	require.True(t, bodyExpiresAt.Valid)

	// 正文到期严格等于创建后 7 天，元数据到期严格晚于正文到期。
	require.WithinDuration(t, record.CreatedAt.Add(service.ErrorDiagnosticBodyRetention), bodyExpiresAt.Time, 2*time.Second)

	// 显式读取必须逐字节等于实际出站字节。
	body, err := svc.ReadErrorDiagnosticBody(ctx, record.ID)
	require.NoError(t, err)
	require.Equal(t, plaintext, body)

	// 第 7 天之后：正文不可读，元数据仍可读。
	backdateErrorDiagnostic(t, record.ID, service.ErrorDiagnosticBodyRetention+time.Minute)

	_, err = svc.ReadErrorDiagnosticBody(ctx, record.ID)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone, "第 7 天后 API 必须拒绝读取正文")

	metadata, err := svc.GetErrorDiagnostic(ctx, record.ID)
	require.NoError(t, err, "第 30 天之前元数据仍可读")
	require.Equal(t, service.ErrorDiagnosticBodyStateExpired, metadata.BodyState)

	// 清理：在线主库物理清除密文列，整行元数据保留。
	cleared, deleted, err := cleanup.RunOnce(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, cleared, int64(1))

	var afterPurgeBody []byte
	var afterPurgeBytes int
	require.NoError(t, integrationDB.QueryRow(
		`SELECT body_ciphertext, body_bytes FROM error_diagnostic_records WHERE diagnostic_id = $1`, record.ID).
		Scan(&afterPurgeBody, &afterPurgeBytes))
	require.Empty(t, afterPurgeBody, "第 7 天必须物理清除主库上的密文列")
	require.Equal(t, len(plaintext), afterPurgeBytes, "正文长度作为历史事实保留")

	require.Equal(t, 1, countErrorDiagnosticRows(t, record.ID), "第 7 天清理不得删除整行元数据")

	purged, err := svc.GetErrorDiagnostic(ctx, record.ID)
	require.NoError(t, err)
	require.Equal(t, service.ErrorDiagnosticBodyStatePurged, purged.BodyState)
	require.Equal(t, service.ErrorDiagnosticBodyRetained, purged.BodyReason)
	_ = deleted

	// 清理之后仍然不可读。
	_, err = svc.ReadErrorDiagnosticBody(ctx, record.ID)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone)
}

// TestErrorDiagnostic_MetadataDay30IsPhysicallyDeletedFromPrimary 覆盖票 01／02：
// 第 30 天在在线主库物理删除整行，且清理延迟期间 API 已经拒绝读取。
func TestErrorDiagnostic_MetadataDay30IsPhysicallyDeletedFromPrimary(t *testing.T) {
	ctx := context.Background()
	svc, cleanup := newErrorDiagnosticTestService(t, service.ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true})

	attempt := errorDiagnosticMessagesAttempt()
	attempt.UpstreamStatusCode = 502
	record, err := svc.RecordErrorDiagnostic(ctx, attempt)
	require.NoError(t, err)

	// 时间推进到第 30 天之后：即使清理还没跑，读取路径也必须拒绝。
	backdateErrorDiagnostic(t, record.ID, service.ErrorDiagnosticMetadataRetention+time.Minute)

	_, err = svc.GetErrorDiagnostic(ctx, record.ID)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticNotFound, "第 30 天后元数据 API 不可读")

	recent, err := svc.ListRecentErrorDiagnostics(ctx, "", 200)
	require.NoError(t, err)
	require.False(t, containsErrorDiagnosticID(recent, record.ID), "已到期的元数据不得出现在列表中")

	require.Equal(t, 1, countErrorDiagnosticRows(t, record.ID), "清理前主库行仍在（API 拒绝不等于已删除）")

	_, deleted, err := cleanup.RunOnce(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, deleted, int64(1))

	require.Zero(t, countErrorDiagnosticRows(t, record.ID), "第 30 天必须在主库物理删除整行")
}

// TestErrorDiagnostic_BodyExclusionsKeepMetadataOnly 覆盖正文边界：
// 越界、非文本 JSON、附件、已知结构化凭据、读取不完整都只留元数据与稳定原因码，不留正文。
func TestErrorDiagnostic_BodyExclusionsKeepMetadataOnly(t *testing.T) {
	ctx := context.Background()
	svc, _ := newErrorDiagnosticTestService(t, service.ErrorDiagnosticSettings{
		Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true,
	})

	base := errorDiagnosticMessagesAttempt()
	// 独立的 sentinel 正文；期望值写死，不由被测函数反推。
	overSized := append(exactlyOneMiBErrorDiagnosticJSON(), ' ')

	cases := []struct {
		name       string
		body       []byte
		complete   bool
		wantState  string
		wantReason string
	}{
		{
			name:       "非文本 JSON",
			body:       []byte{0x00, 0x01, 0xff},
			complete:   true,
			wantState:  service.ErrorDiagnosticBodyStateSkipped,
			wantReason: service.ErrorDiagnosticBodySkippedNotTextJSON,
		},
		{
			name:       "超过 1 MiB 不截断冒充完整",
			body:       overSized,
			complete:   true,
			wantState:  service.ErrorDiagnosticBodyStateSkipped,
			wantReason: service.ErrorDiagnosticBodySkippedTooLarge,
		},
		{
			name:       "图片或文件附件",
			body:       []byte(`{"content":[{"type":"image","image_url":{"url":"https://sentinel/a.png"}}]}`),
			complete:   true,
			wantState:  service.ErrorDiagnosticBodyStateSkipped,
			wantReason: service.ErrorDiagnosticBodySkippedAttachment,
		},
		{
			// P4：编码媒体部件（音频 base64）在体量合格时必须同样只留元数据。
			name:       "编码音频部件",
			body:       []byte(`{"content":[{"type":"input_audio","input_audio":{"data":"AA==","format":"wav"}}]}`),
			complete:   true,
			wantState:  service.ErrorDiagnosticBodyStateSkipped,
			wantReason: service.ErrorDiagnosticBodySkippedAttachment,
		},
		{
			name:       "已知结构化凭据",
			body:       []byte(`{"fallback_credit_token":"sentinel-credential"}`),
			complete:   true,
			wantState:  service.ErrorDiagnosticBodyStateSkipped,
			wantReason: service.ErrorDiagnosticBodySkippedKnownCredential,
		},
		{
			name:       "正文读取不完整",
			body:       []byte(`{"model":"claude"}`),
			complete:   false,
			wantState:  service.ErrorDiagnosticBodyStateSkipped,
			wantReason: service.ErrorDiagnosticBodySkippedIncompleteRead,
		},
	}

	var ids []string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attempt := base
			attempt.UpstreamStatusCode = 500
			attempt.Body = tc.body
			attempt.BodyReadComplete = tc.complete

			record, err := svc.RecordErrorDiagnostic(ctx, attempt)
			require.NoError(t, err, "正文不合格仍要保留安全元数据")
			ids = append(ids, record.ID)

			require.Equal(t, tc.wantState, record.BodyState)
			require.Equal(t, tc.wantReason, record.BodyReason)
			require.Zero(t, record.BodyBytes)

			ciphertext, bodyBytes, _, _, bodyExpiresAt := errorDiagnosticRowFacts(t, record.ID)
			require.Empty(t, ciphertext, "不合格正文不得落库")
			require.Zero(t, bodyBytes)
			require.False(t, bodyExpiresAt.Valid)

			// 未留存时显式读取必须拒绝，且不区分原因。
			_, err = svc.ReadErrorDiagnosticBody(ctx, record.ID)
			require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone)

			// 元数据仍然逐次可查，并带上未留存原因。
			stored, err := svc.GetErrorDiagnostic(ctx, record.ID)
			require.NoError(t, err)
			require.Equal(t, tc.wantReason, stored.BodyReason)
		})
	}
	t.Cleanup(func() { deleteErrorDiagnosticRows(t, ids...) })
}

// TestErrorDiagnostic_ExactlyOneMiBIsAccepted 覆盖 1 MiB 边界本身是可留存的。
func TestErrorDiagnostic_ExactlyOneMiBIsAccepted(t *testing.T) {
	ctx := context.Background()
	svc, _ := newErrorDiagnosticTestService(t, service.ErrorDiagnosticSettings{
		Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true,
	})

	// 恰好 1 MiB 的合法文本 JSON。
	payload := exactlyOneMiBErrorDiagnosticJSON()
	require.True(t, len(payload) == service.ErrorDiagnosticMaxBodyBytes)

	attempt := errorDiagnosticMessagesAttempt()
	attempt.UpstreamStatusCode = 500
	attempt.Body = payload
	attempt.BodyReadComplete = true

	record, err := svc.RecordErrorDiagnostic(ctx, attempt)
	require.NoError(t, err)
	t.Cleanup(func() { deleteErrorDiagnosticRows(t, record.ID) })

	require.Equal(t, service.ErrorDiagnosticBodyStateStored, record.BodyState)
	require.Equal(t, service.ErrorDiagnosticMaxBodyBytes, record.BodyBytes)

	body, err := svc.ReadErrorDiagnosticBody(ctx, record.ID)
	require.NoError(t, err)
	require.Equal(t, payload, body)
}

// TestErrorDiagnostic_BodyRetentionDisabledBySettings 覆盖票 02 的分阶段 opt-in：
// 未打开正文开关时，正文一律不留存，只留稳定原因码。
func TestErrorDiagnostic_BodyRetentionDisabledBySettings(t *testing.T) {
	ctx := context.Background()
	svc, _ := newErrorDiagnosticTestService(t, service.ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true})

	attempt := errorDiagnosticMessagesAttempt()
	attempt.UpstreamStatusCode = 400
	attempt.Body = []byte(`{"model":"claude"}`)
	attempt.BodyReadComplete = true

	record, err := svc.RecordErrorDiagnostic(ctx, attempt)
	require.NoError(t, err)
	t.Cleanup(func() { deleteErrorDiagnosticRows(t, record.ID) })

	require.Equal(t, service.ErrorDiagnosticBodyStateSkipped, record.BodyState)
	require.Equal(t, service.ErrorDiagnosticBodySkippedRetentionDisabled, record.BodyReason)

	ciphertext, _, _, _, _ := errorDiagnosticRowFacts(t, record.ID)
	require.Empty(t, ciphertext)
}

// TestErrorDiagnostic_UsageLinkSurvivesUsageDeletion 覆盖
// 「关联删除时诊断不提前级联消失，也不阻断 usage 清理」：删使用记录只置空关联。
func TestErrorDiagnostic_UsageLinkSurvivesUsageDeletion(t *testing.T) {
	ctx := context.Background()
	svc, _ := newErrorDiagnosticTestService(t, service.ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true})

	user, apiKey, account, usageLogID := createErrorDiagnosticUsageFixture(t, ctx)
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM users WHERE id = $1`, user)
		_, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id = $1`, account)
	})

	attempt := errorDiagnosticMessagesAttempt()
	attempt.UpstreamStatusCode = 500
	attempt.UsageLogID = usageLogID

	record, err := svc.RecordErrorDiagnostic(ctx, attempt)
	require.NoError(t, err)
	t.Cleanup(func() { deleteErrorDiagnosticRows(t, record.ID) })

	require.True(t, record.HasUsage)

	linked, err := svc.ListErrorDiagnosticsByUsageLog(ctx, usageLogID, 10)
	require.NoError(t, err)
	require.True(t, containsErrorDiagnosticID(linked, record.ID))

	// 删除使用记录：诊断必须保留，且不阻断删除。
	res, err := integrationDB.Exec(`DELETE FROM usage_logs WHERE id = $1`, usageLogID)
	require.NoError(t, err, "删除使用记录不得被诊断阻塞")
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, affected)

	require.Equal(t, 1, countErrorDiagnosticRows(t, record.ID), "诊断不得随使用记录级联消失")

	_, _, usageLogIDAfter, _, _ := errorDiagnosticRowFacts(t, record.ID)
	require.False(t, usageLogIDAfter.Valid, "使用记录删除后关联置空，诊断按自身期限保留")

	// 仍可按元数据到期读取（第 30 天之前），并仍保留原始安全事实。
	stored, err := svc.GetErrorDiagnostic(ctx, record.ID)
	require.NoError(t, err)
	require.Equal(t, 500, stored.UpstreamStatusCode)
	require.False(t, stored.HasUsage, "关联被解除后不再声称有使用记录")

	// 指向已删除使用记录的关联查询不再返回该行，但记录本身还在。
	linkedAfter, err := svc.ListErrorDiagnosticsByUsageLog(ctx, usageLogID, 10)
	require.NoError(t, err)
	require.Empty(t, linkedAfter)

	_ = apiKey
}

func createErrorDiagnosticUsageFixture(t *testing.T, ctx context.Context) (userID, apiKeyID, accountID, usageLogID int64) {
	t.Helper()
	suffix := uuid.NewString()

	user := mustCreateUser(t, integrationEntClient, &service.User{Email: "diag-" + suffix + "@example.com"})
	apiKey := mustCreateApiKey(t, integrationEntClient, &service.APIKey{
		UserID: user.ID, Key: "sk-diag-" + suffix, Name: "diag",
	})
	account := mustCreateAccount(t, integrationEntClient, &service.Account{Name: "acc-diag-" + suffix})

	require.NoError(t, integrationDB.QueryRow(`
		INSERT INTO usage_logs (user_id, api_key_id, account_id, request_id, model)
		VALUES ($1, $2, $3, $4, 'claude-sonnet-4-5')
		RETURNING id`, user.ID, apiKey.ID, account.ID, "diag-"+suffix).Scan(&usageLogID))

	return user.ID, apiKey.ID, account.ID, usageLogID
}

// TestErrorDiagnostic_DefaultOffWritesNothingToPrimary 覆盖
// 生产默认关闭：未启用时连元数据都不落库。
func TestErrorDiagnostic_DefaultOffWritesNothingToPrimary(t *testing.T) {
	ctx := context.Background()
	svc, _ := newErrorDiagnosticTestService(t, service.ErrorDiagnosticSettings{})

	before := countAllErrorDiagnosticRows(t)

	_, err := svc.RecordErrorDiagnostic(ctx, errorDiagnosticMessagesAttempt())
	require.ErrorIs(t, err, service.ErrErrorDiagnosticDisabled)
	require.Equal(t, before, countAllErrorDiagnosticRows(t), "默认关闭时不得写入任何行")
	require.False(t, svc.CaptureEnabled(ctx))
}

// exactlyOneMiBErrorDiagnosticJSON 造一段恰好 1 MiB 的合法 JSON 正文，
// 作为与生产判定无关的独立 sentinel 输入。
func exactlyOneMiBErrorDiagnosticJSON() []byte {
	head := []byte(`{"padding":"`)
	tail := []byte(`"}`)
	body := make([]byte, 0, service.ErrorDiagnosticMaxBodyBytes)
	body = append(body, head...)
	body = append(body, bytes.Repeat([]byte("a"), service.ErrorDiagnosticMaxBodyBytes-len(head)-len(tail))...)
	body = append(body, tail...)
	return body
}

// TestErrorDiagnosticSchemaMatchesNumberedMigration 断言运行库确实由编号迁移建立了
// 收窄契约：opaque 主键、白名单 CHECK、usage 关联为 ON DELETE SET NULL。
func TestErrorDiagnosticSchemaMatchesNumberedMigration(t *testing.T) {
	tx := testTx(t)

	requireForeignKeyOnDelete(t, tx, "error_diagnostic_records", "usage_log_id", "usage_logs", "SET NULL")
	requireColumn(t, tx, "error_diagnostic_records", "diagnostic_id", "text", 0, false)
	requireColumn(t, tx, "error_diagnostic_records", "usage_log_id", "bigint", 0, true)
	requireColumn(t, tx, "error_diagnostic_records", "body_ciphertext", "bytea", 0, true)
	requireColumn(t, tx, "error_diagnostic_records", "metadata_expires_at", "timestamp with time zone", 0, false)
	requireColumn(t, tx, "error_diagnostic_records", "body_expires_at", "timestamp with time zone", 0, true)

	// 白名单与边界由数据库兜底，应用层之外也写不进不合格内容。
	requireCheckConstraintViolation(t, `
		INSERT INTO error_diagnostic_records
			(diagnostic_id, protocol, attempt_index, stage, upstream_status, body_state, body_reason, metadata_expires_at)
		VALUES (REPEAT('a', 32), 'antigravity.generate', 0, 'wire', 400, 'skipped', 'skipped_too_large', NOW() + INTERVAL '30 days')`)

	requireCheckConstraintViolation(t, `
		INSERT INTO error_diagnostic_records
			(diagnostic_id, protocol, attempt_index, stage, upstream_status, body_state, body_reason, metadata_expires_at)
		VALUES (REPEAT('a', 32), 'messages', 0, 'wire', 200, 'skipped', 'skipped_too_large', NOW() + INTERVAL '30 days')`)

	requireCheckConstraintViolation(t, `
		INSERT INTO error_diagnostic_records
			(diagnostic_id, protocol, attempt_index, stage, upstream_status, body_state, body_reason, metadata_expires_at)
		VALUES ('not-an-opaque-id', 'messages', 0, 'wire', 400, 'skipped', 'skipped_too_large', NOW() + INTERVAL '30 days')`)

	requireCheckConstraintViolation(t, `
		INSERT INTO error_diagnostic_records
			(diagnostic_id, protocol, attempt_index, stage, upstream_status, body_state, body_reason, metadata_expires_at)
		VALUES (REPEAT('b', 32), 'messages', 0, 'wire', 400, 'skipped', 'totally_made_up_reason', NOW() + INTERVAL '30 days')`)
}

// requireCheckConstraintViolation 直接在自动提交连接上尝试写入，
// 因为 PostgreSQL 一旦在事务内触发约束错误就会把整个事务标记为 aborted，
// 后续语句只会得到「transaction is aborted」，无法逐个验证约束。
// 这些语句全部失败，因此不会留下任何行。
func requireCheckConstraintViolation(t *testing.T, query string) {
	t.Helper()
	_, err := integrationDB.Exec(query)
	require.Error(t, err, "数据库必须拒绝不合格内容")
	require.Contains(t, err.Error(), "error_diagnostic_records", "必须是本表的约束失败")
	require.Contains(t, err.Error(), "violates check constraint", "必须是白名单 CHECK 生效，而不是其他错误")
}

// TestErrorDiagnostic_CountAndPageAgreeWithRealRows 覆盖 total 与分页在真实库上一致：
// total 精确等于当前未过期行数，且分页不重复、不遗漏、不把已到期行算进去。
func TestErrorDiagnostic_CountAndPageAgreeWithRealRows(t *testing.T) {
	ctx := context.Background()
	svc, _ := newErrorDiagnosticTestService(t, service.ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true})

	// 基线：该协议下其他用例可能留下行，因此只比较本次新增的差值。
	const created = 3
	var ids []string
	for i := 0; i < created; i++ {
		attempt := errorDiagnosticMessagesAttempt()
		attempt.UpstreamStatusCode = 500
		attempt.AttemptIndex = i
		record, err := svc.RecordErrorDiagnostic(ctx, attempt)
		require.NoError(t, err)
		ids = append(ids, record.ID)
	}
	t.Cleanup(func() { deleteErrorDiagnosticRows(t, ids...) })

	baseline, err := svc.CountRecentErrorDiagnostics(ctx, service.ErrorDiagnosticProtocolMessages)
	require.NoError(t, err)
	require.GreaterOrEqual(t, baseline, int64(created))

	// total 必须与列出的行数一致：分页窗口取满时两者相等。
	page, err := svc.ListRecentErrorDiagnosticPage(ctx, service.ErrorDiagnosticProtocolMessages, 0, service.ErrorDiagnosticMaxListLimit)
	require.NoError(t, err)
	require.EqualValues(t, baseline, len(page), "total 必须与列表同源，不能出现 total>0 而列表为空")

	// 本次新增的行都在第一页里，且没有任何重复标识。
	seen := map[string]bool{}
	for _, record := range page {
		require.False(t, seen[record.ID], "分页不得返回重复行")
		seen[record.ID] = true
	}
	for _, id := range ids {
		require.True(t, seen[id], "新写入的诊断必须出现在第一页")
	}

	// 窗口小于 total 时，第二页与第一页不重叠。
	if baseline > 1 {
		first, err := svc.ListRecentErrorDiagnosticPage(ctx, service.ErrorDiagnosticProtocolMessages, 0, 1)
		require.NoError(t, err)
		require.Len(t, first, 1)
		second, err := svc.ListRecentErrorDiagnosticPage(ctx, service.ErrorDiagnosticProtocolMessages, 1, 1)
		require.NoError(t, err)
		require.Len(t, second, 1)
		require.NotEqual(t, first[0].ID, second[0].ID)
	}

	// 时间推进到第 30 天之后：total 与分页都必须同时不再包含这些行。
	for _, id := range ids {
		backdateErrorDiagnostic(t, id, service.ErrorDiagnosticMetadataRetention+time.Minute)
	}
	after, err := svc.CountRecentErrorDiagnostics(ctx, service.ErrorDiagnosticProtocolMessages)
	require.NoError(t, err)
	require.Equal(t, baseline-int64(created), after, "第 30 天后 total 不得再统计这些行")

	pageAfter, err := svc.ListRecentErrorDiagnosticPage(ctx, service.ErrorDiagnosticProtocolMessages, 0, service.ErrorDiagnosticMaxListLimit)
	require.NoError(t, err)
	require.EqualValues(t, after, len(pageAfter), "total 与分页必须同时排除已到期行")
	for _, id := range ids {
		require.False(t, containsErrorDiagnosticID(pageAfter, id), "已到期的行不得出现在分页里")
	}
}

// TestErrorDiagnostic_PurgedBodyStillReportsGone 覆盖清理已经物理删除密文之后，
// 状态仍然是「曾留存、已清除」，因此读取仍必须拒绝（对应管理端的 410 语义）。
func TestErrorDiagnostic_PurgedBodyStillReportsGone(t *testing.T) {
	ctx := context.Background()
	svc, cleanup := newErrorDiagnosticTestService(t, service.ErrorDiagnosticSettings{
		Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true,
	})

	attempt := errorDiagnosticMessagesAttempt()
	attempt.UpstreamStatusCode = 400
	attempt.Body = []byte(`{"model":"claude","messages":[{"role":"user","content":"sentinel"}]}`)
	attempt.BodyReadComplete = true

	record, err := svc.RecordErrorDiagnostic(ctx, attempt)
	require.NoError(t, err)
	t.Cleanup(func() { deleteErrorDiagnosticRows(t, record.ID) })
	require.Equal(t, service.ErrorDiagnosticBodyStateStored, record.BodyState)

	// 推进到第 7 天，再让清理真正把密文列物理清除。
	backdateErrorDiagnostic(t, record.ID, service.ErrorDiagnosticBodyRetention+time.Minute)
	_, _, err = cleanup.RunOnce(ctx)
	require.NoError(t, err)

	ciphertext, _, _, _, bodyExpiresAt := errorDiagnosticRowFacts(t, record.ID)
	require.Empty(t, ciphertext, "密文必须已被物理清除")

	// 关键：状态是 purged 而不是 not_observed，原因仍是 retained，
	// 因此管理端可以把「清理已执行」映射成 410 而不是 409。
	metadata, err := svc.GetErrorDiagnostic(ctx, record.ID)
	require.NoError(t, err)
	require.Equal(t, service.ErrorDiagnosticBodyStatePurged, metadata.BodyState)
	require.Equal(t, service.ErrorDiagnosticBodyRetained, metadata.BodyReason)
	require.False(t, metadata.BodyReadableAt(time.Now()))
	require.True(t, bodyExpiresAt.Valid)

	_, err = svc.ReadErrorDiagnosticBody(ctx, record.ID)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone)
}

// TestErrorDiagnostic_OffsetBeyondTwoHundredIsHonored 覆盖真实翻页：
// 偏移超过 200 仍必须原样下推到 SQL，返回的是该偏移处的真实行，
// 而不是被夹到窗口边界后把上一页的行当成下一页。
//
// 期望值用一条独立的原生 SQL 查询算出，不由被测仓储反推。
func TestErrorDiagnostic_OffsetBeyondTwoHundredIsHonored(t *testing.T) {
	ctx := context.Background()
	svc, _ := newErrorDiagnosticTestService(t, service.ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true})

	const rows = 205
	var ids []string
	for i := 0; i < rows; i++ {
		attempt := errorDiagnosticMessagesAttempt()
		attempt.UpstreamStatusCode = 500
		attempt.AttemptIndex = i
		record, err := svc.RecordErrorDiagnostic(ctx, attempt)
		require.NoError(t, err)
		ids = append(ids, record.ID)
	}
	t.Cleanup(func() { deleteErrorDiagnosticRows(t, ids...) })

	total, err := svc.CountRecentErrorDiagnostics(ctx, service.ErrorDiagnosticProtocolMessages)
	require.NoError(t, err)
	require.GreaterOrEqual(t, total, int64(rows), "本次写入的行必须都在可读范围内")

	// 逐页遍历，断言不重复、不遗漏，且每一页都与原生 SQL 完全一致。
	seen := map[string]bool{}
	reached := 0
	for offset := 0; offset < int(total)+50; offset += 100 {
		page, err := svc.ListRecentErrorDiagnosticPage(ctx, service.ErrorDiagnosticProtocolMessages, offset, 100)
		require.NoError(t, err)
		require.Equal(t, expectedErrorDiagnosticPage(t, offset, 100), errorDiagnosticIDs(page),
			"offset %d 的页必须与原生 SQL 一致", offset)

		for _, record := range page {
			require.False(t, seen[record.ID], "跨页不得出现重复行（offset %d）", offset)
			seen[record.ID] = true
		}
		reached += len(page)
	}
	require.EqualValues(t, total, reached, "逐页遍历必须恰好覆盖全部可读行")
	for _, id := range ids {
		require.True(t, seen[id], "本次写入的行必须能被翻页取到")
	}

	// 超过 200 的偏移必须仍能取到真实行（夹到 200 时会与第 200 偏移页完全重合）。
	deep, err := svc.ListRecentErrorDiagnosticPage(ctx, service.ErrorDiagnosticProtocolMessages, 800, 100)
	require.NoError(t, err)
	require.Equal(t, expectedErrorDiagnosticPage(t, 800, 100), errorDiagnosticIDs(deep))
	require.NotEqual(t, expectedErrorDiagnosticPage(t, 200, 100), errorDiagnosticIDs(deep),
		"offset 800 不得被夹到 200 而返回同一批行")
}

func expectedErrorDiagnosticPage(t *testing.T, offset, limit int) []string {
	t.Helper()
	rows, err := integrationDB.Query(`
		SELECT diagnostic_id FROM error_diagnostic_records
		WHERE metadata_expires_at > NOW()
		  AND protocol = $1
		ORDER BY created_at DESC, diagnostic_id DESC
		OFFSET $2 LIMIT $3
	`, service.ErrorDiagnosticProtocolMessages, offset, limit)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	out := make([]string, 0, limit)
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
}

func errorDiagnosticIDs(records []service.ErrorDiagnosticRecord) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.ID)
	}
	return out
}

// TestErrorDiagnosticCleanupBacklogIsObservable 覆盖「落后要能度量」：
// 在线主库上超期但尚未物理清除的行必须能被计数，并给出最老超期时长。
// 这是物理残留的度量，不代表这些行仍可被应用读取。
func TestErrorDiagnosticCleanupBacklogIsObservable(t *testing.T) {
	ctx := context.Background()
	svc, cleanup := newErrorDiagnosticTestService(t, service.ErrorDiagnosticSettings{
		Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true,
	})

	before, err := cleanup.Backlog(ctx)
	require.NoError(t, err)

	// 造一行正文与元数据都已超期的记录：正文 7 天、元数据 30 天，因此推进 40 天。
	attempt := errorDiagnosticMessagesAttempt()
	attempt.UpstreamStatusCode = 500
	attempt.Body = []byte(`{"model":"claude","messages":[{"role":"user","content":"sentinel"}]}`)
	attempt.BodyReadComplete = true

	record, err := svc.RecordErrorDiagnostic(ctx, attempt)
	require.NoError(t, err)
	t.Cleanup(func() { deleteErrorDiagnosticRows(t, record.ID) })
	require.Equal(t, service.ErrorDiagnosticBodyStateStored, record.BodyState)

	const overdue = 40 * 24 * time.Hour
	backdateErrorDiagnostic(t, record.ID, overdue)

	// 超期期间应用读取必须已经被拒绝——它不再是可读内容，只是物理残留。
	_, err = svc.GetErrorDiagnostic(ctx, record.ID)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticNotFound)
	_, err = svc.ReadErrorDiagnosticBody(ctx, record.ID)
	require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone)

	after, err := cleanup.Backlog(ctx)
	require.NoError(t, err)

	require.GreaterOrEqual(t, after.BodiesOverdue-before.BodiesOverdue, int64(1), "超期的正文必须被计入积压")
	require.GreaterOrEqual(t, after.RecordsOverdue-before.RecordsOverdue, int64(1), "超期的元数据必须被计入积压")

	// 最老超期项是正文：它在 created_at + 7 天后到期，而整行被推进了 40 天，
	// 因此此刻已经超期 33 天（元数据到期更晚，只超期 10 天）。
	expectedOldest := overdue - service.ErrorDiagnosticBodyRetention
	oldestSeconds := after.OldestOverdueSeconds(time.Now())
	require.GreaterOrEqual(t, oldestSeconds, int64(expectedOldest.Seconds())-60,
		"最老超期时长必须反映真实落后程度（允许一分钟的时钟误差）")
	require.LessOrEqual(t, oldestSeconds, int64(expectedOldest.Seconds())+60,
		"最老超期时长不得被高估")

	// 清理之后积压回落，证明度量的是「尚未物理清除」而不是「曾经过期」。
	_, _, err = cleanup.RunOnce(ctx)
	require.NoError(t, err)
	require.Zero(t, countErrorDiagnosticRows(t, record.ID), "超期记录必须已被物理删除")

	cleared, err := cleanup.Backlog(ctx)
	require.NoError(t, err)
	require.Less(t, cleared.RecordsOverdue, after.RecordsOverdue, "清理后积压必须回落")
}
