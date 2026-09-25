package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 错误诊断运维开关（管理员 API）的验收测试。
//
// 覆盖：生产默认关闭、逐字风险确认（缺失／不匹配／本地化）、每次更新都重新校验、
// 正文留存需要密钥、关闭永远允许且强制关掉正文、非管理员（路由层）拒绝、
// 读取／写入失败不得伪造成「已关闭」或「已开启」、响应与日志都不出现密钥或正文原文。

const errorDiagnosticOperatorTestKeyHex = "abababababababababababababababababababababababababababababababab"

// errorDiagnosticOperatorRepoStub 是可注入失败的设置仓储替身。
type errorDiagnosticOperatorRepoStub struct {
	values    map[string]string
	failGet   map[string]error
	failWrite error
}

func (s *errorDiagnosticOperatorRepoStub) Get(ctx context.Context, key string) (*service.Setting, error) {
	value, err := s.GetValue(ctx, key)
	if err != nil {
		return nil, err
	}
	return &service.Setting{Key: key, Value: value}, nil
}

func (s *errorDiagnosticOperatorRepoStub) GetValue(_ context.Context, key string) (string, error) {
	if err, ok := s.failGet[key]; ok && err != nil {
		return "", err
	}
	if s.values == nil {
		return "", nil
	}
	return s.values[key], nil
}

func (s *errorDiagnosticOperatorRepoStub) Set(ctx context.Context, key, value string) error {
	if s.failWrite != nil {
		return s.failWrite
	}
	if s.values == nil {
		s.values = map[string]string{}
	}
	s.values[key] = value
	return nil
}

func (s *errorDiagnosticOperatorRepoStub) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		value, err := s.GetValue(ctx, key)
		if err != nil {
			return nil, err
		}
		if value != "" {
			out[key] = value
		}
	}
	return out, nil
}

func (s *errorDiagnosticOperatorRepoStub) SetMultiple(ctx context.Context, settings map[string]string) error {
	if s.failWrite != nil {
		return s.failWrite
	}
	for key, value := range settings {
		if err := s.Set(ctx, key, value); err != nil {
			return err
		}
	}
	return nil
}

func (s *errorDiagnosticOperatorRepoStub) GetAll(_ context.Context) (map[string]string, error) {
	out := make(map[string]string, len(s.values))
	for key, value := range s.values {
		out[key] = value
	}
	return out, nil
}

func (s *errorDiagnosticOperatorRepoStub) Delete(_ context.Context, key string) error {
	delete(s.values, key)
	return nil
}

// errorDiagnosticOperatorTestConfig 构造测试配置。
//
// keyConfigured=false 模拟启动时随机生成主密钥（重启即换钥）的部署：
// 正文密文重启后不可解，正文留存必须被拒绝，而元数据采集不依赖密钥。
func errorDiagnosticOperatorTestConfig(keyConfigured bool) *config.Config {
	cfg := &config.Config{Default: config.DefaultConfig{UserConcurrency: 5}}
	if keyConfigured {
		cfg.Totp.EncryptionKey = errorDiagnosticOperatorTestKeyHex
		cfg.Totp.EncryptionKeyConfigured = true
	}
	return cfg
}

func newErrorDiagnosticOperatorHandler(t *testing.T, stored map[string]string, keyConfigured bool) (*SettingHandler, *errorDiagnosticOperatorRepoStub) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := &errorDiagnosticOperatorRepoStub{values: stored}
	svc := service.NewSettingService(repo, errorDiagnosticOperatorTestConfig(keyConfigured))
	return NewSettingHandler(svc, nil, nil, nil, nil, nil, nil), repo
}

func getErrorDiagnosticOperatorSettings(t *testing.T, h *SettingHandler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/error-diagnostic", nil)
	h.GetErrorDiagnosticOperatorSettings(c)
	return rec
}

func putErrorDiagnosticOperatorSettings(t *testing.T, h *SettingHandler, payload map[string]any, operatorID int64, authMethod string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/error-diagnostic", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "error-diagnostic-test")
	if operatorID > 0 {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: operatorID})
	}
	if authMethod != "" {
		c.Set("auth_method", authMethod)
	}
	h.UpdateErrorDiagnosticOperatorSettings(c)
	return rec
}

func decodeErrorDiagnosticOperatorStatus(t *testing.T, rec *httptest.ResponseRecorder) service.ErrorDiagnosticOperatorStatus {
	t.Helper()
	var envelope struct {
		Code   int                                   `json:"code"`
		Reason string                                `json:"reason"`
		Data   service.ErrorDiagnosticOperatorStatus `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	return envelope.Data
}

func errorDiagnosticOperatorErrorReason(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Code   int    `json:"code"`
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	return envelope.Reason
}

func errorDiagnosticOperatorSettingsFrom(t *testing.T, repo *errorDiagnosticOperatorRepoStub) service.ErrorDiagnosticSettings {
	t.Helper()
	var settings service.ErrorDiagnosticSettings
	raw := repo.values[service.SettingKeyErrorDiagnostic]
	if raw != "" {
		require.NoError(t, json.Unmarshal([]byte(raw), &settings))
	}
	return settings
}

// 生产默认关闭：没有任何存量键时，读取到的是全关，且不写任何门控键。
func TestErrorDiagnosticOperatorSettingsDefaultOff(t *testing.T) {
	h, repo := newErrorDiagnosticOperatorHandler(t, nil, true)

	rec := getErrorDiagnosticOperatorSettings(t, h)
	require.Equal(t, http.StatusOK, rec.Code)
	status := decodeErrorDiagnosticOperatorStatus(t, rec)
	require.False(t, status.Enabled)
	require.False(t, status.BodyRetentionEnabled)
	require.False(t, status.HeaderValueRetentionEnabled)
	require.False(t, status.CaptureAllowed)
	require.False(t, status.BodyRetentionAllowed)
	require.False(t, status.HeaderValueRetentionAllowed)
	require.Nil(t, status.RiskAcknowledgement)
	require.False(t, status.RiskAcknowledgementCurrent)
	require.Equal(t, service.ErrorDiagnosticRiskAcknowledgementVersion, status.RiskVersion)
	require.Empty(t, repo.values, "默认关闭时不得写入任何门控键")

	// 门控本身仍走既有 reader：未显式开启时采集与正文都不允许。
	settings, err := h.settingService.GetErrorDiagnosticSettings(context.Background())
	require.NoError(t, err)
	require.False(t, settings.CaptureAllowed())
	require.False(t, settings.BodyCaptureAllowed())
}

// 逐字确认语句必须把已声明的剩余风险写全：正文与 429 头值同为 7 天／元数据 30 天、
// 仅在线主库物理删除与 API 到期拒绝、旧副本／备份／PITR／人工导出仍可恢复、且不是合规
// 擦除手段；头值这一层还必须写明「只采自白名单，且绝不含凭据头」。
// 语句被弱化时这条测试先失败，而不是悄悄放宽。
func TestErrorDiagnosticRiskPhraseStatesDeclaredResidualRisks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		phrase string
		parts  []string
	}{
		{
			name:   "en",
			phrase: service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
			parts: []string{
				"7 days", "30 days", "online primary database",
				"rejected immediately", "delay without a guaranteed maximum",
				"may contain credentials that filtering cannot identify",
				"replicas", "backups", "PITR", "manual exports",
				"not a compliance deletion",
				// 429 头值这一条窄路径必须同样写在书面确认里：到期 7 天、只采白名单、
				// 且明确排除凭据头（Cookie／Authorization／X-Api-Key）。
				"429 header values", "allowlist",
				"Authorization", "Cookie", "Set-Cookie", "X-Api-Key",
			},
		},
		{
			name:   "zh",
			phrase: service.ErrorDiagnosticRiskAcknowledgementPhraseZH,
			parts: []string{
				"正文与上游 429 头值保留 7 天", "元数据保留 30 天", "在线主库",
				"立即拒绝 API 读取", "延迟且不保证上限",
				"可能含无法识别的凭据",
				"只读副本", "备份", "PITR", "人工导出",
				"不是合规删除",
				"白名单", "Authorization", "Cookie", "Set-Cookie", "X-Api-Key",
			},
		},
	} {
		require.NotEmpty(t, tc.phrase, tc.name)
		for _, part := range tc.parts {
			require.Contains(t, tc.phrase, part, tc.name)
		}
	}
}

// 声明原文与版本被钉住：措辞改动必须同时改本测试并提升版本常量，
// 不允许在不提升版本的情况下放宽或模糊已声明的剩余风险（尤其是物理删除的延迟不保证上限）。
func TestErrorDiagnosticRiskPhraseIsPinned(t *testing.T) {
	require.Equal(t, "v2026.09.24.1", service.ErrorDiagnosticRiskAcknowledgementVersion)
	require.Equal(t,
		"Error diagnostic bodies and upstream 429 header values are retained for 7 days and sanitized metadata for 30 days; "+
			"at expiry API reads are rejected immediately, while physical deletion is performed only in the online primary database "+
			"by periodic cleanup that backlog or downtime can delay without a guaranteed maximum; "+
			"retained text is arbitrary client content and may contain credentials that filtering cannot identify; "+
			"header values are captured only from an allowlist that excludes credential headers such as Authorization, Cookie, Set-Cookie and X-Api-Key, "+
			"and header names outside that list are never captured; "+
			"copies in replicas, backups, PITR and manual exports may persist and can be recovered there, "+
			"and this feature is not a compliance deletion or data-subject erasure tool.",
		service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	)
	require.Equal(t,
		"错误诊断正文与上游 429 头值保留 7 天、净化元数据保留 30 天；到期后立即拒绝 API 读取，"+
			"但物理删除只在在线主库由周期清理执行，积压或停机可能使其延迟且不保证上限；"+
			"留存正文是任意客户端文本，可能含无法识别的凭据；"+
			"头值只采自白名单，绝不含 Authorization、Cookie、Set-Cookie、X-Api-Key 等凭据头，"+
			"名单之外的头名一律不采；"+
			"只读副本、备份、PITR 与人工导出中的旧数据可能仍留存且可恢复，"+
			"本功能不是合规删除或数据主体擦除手段。",
		service.ErrorDiagnosticRiskAcknowledgementPhraseZH,
	)
}

// errorDiagnosticPreviousRiskVersion／errorDiagnosticPreviousRiskPhraseEN 是上一版
// （正文单层留存时期的）确认语句与版本，逐字保留。
//
// 它们不引用当前常量：这里要证明的正是「语句内容变化后，按旧语句做过的确认不再覆盖当前
// 语句」，如果跟着当前常量漂移，这条证据就自己失效了。
const (
	errorDiagnosticPreviousRiskVersion  = "v2026.09.24"
	errorDiagnosticPreviousRiskPhraseEN = "Error diagnostic bodies are retained for 7 days and sanitized metadata for 30 days; " +
		"at expiry API reads are rejected immediately, while physical deletion is performed only in the online primary database " +
		"by periodic cleanup that backlog or downtime can delay without a guaranteed maximum; " +
		"retained text is arbitrary client content and may contain credentials that filtering cannot identify; " +
		"copies in replicas, backups, PITR and manual exports may persist and can be recovered there, " +
		"and this feature is not a compliance deletion or data-subject erasure tool."
)

// 语句版本必须随语句内容一起提升：旧版本的确认记录不能覆盖当前语句。
//
// 这条测试是「不因为库里有一条旧确认就静默放行」的证据：语句里多出 429 头值这一层
// 留存事实后，按旧语句确认过的部署必须被判为确认过期，只能重新逐字确认当前语句。
func TestErrorDiagnosticRiskPhraseVersionTracksStatementChanges(t *testing.T) {
	require.NotEqual(t, errorDiagnosticPreviousRiskVersion, service.ErrorDiagnosticRiskAcknowledgementVersion)
	require.NotEqual(t, errorDiagnosticPreviousRiskPhraseEN, service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
		"语句内容变化必须同时提升版本，否则旧确认会被继续采信")

	previous := service.ErrorDiagnosticRiskAcknowledgement{
		Version:     errorDiagnosticPreviousRiskVersion,
		Phrase:      errorDiagnosticPreviousRiskPhraseEN,
		AdminUserID: 7,
		AcceptedAt:  time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
	}
	require.False(t, previous.CoversCurrentStatement(), "旧版本语句不构成当前版本的确认")

	// 版本对、原文旧（同一版本下语句被改写）同样不覆盖：版本与原文都必须匹配。
	require.False(t, service.ErrorDiagnosticRiskAcknowledgement{
		Version:     service.ErrorDiagnosticRiskAcknowledgementVersion,
		Phrase:      errorDiagnosticPreviousRiskPhraseEN,
		AdminUserID: 7,
		AcceptedAt:  time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
	}.CoversCurrentStatement(), "版本匹配但原文是旧语句，不得放行")
}

// 开启必须携带逐字风险确认：缺失与不匹配都拒绝，且不留下部分状态。
func TestErrorDiagnosticOperatorSettingsEnableRequiresTypedRiskAcknowledgement(t *testing.T) {
	h, repo := newErrorDiagnosticOperatorHandler(t, nil, true)

	missing := putErrorDiagnosticOperatorSettings(t, h, map[string]any{"enabled": true}, 42, "")
	require.Equal(t, http.StatusBadRequest, missing.Code)
	require.Equal(t, "ERROR_DIAGNOSTIC_RISK_ACK_REQUIRED", errorDiagnosticOperatorErrorReason(t, missing))
	require.Empty(t, repo.values)

	mismatched := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "language": "en", "phrase": "I acknowledge",
	}, 42, "")
	require.Equal(t, http.StatusBadRequest, mismatched.Code)
	require.Equal(t, "ERROR_DIAGNOSTIC_RISK_ACK_INVALID", errorDiagnosticOperatorErrorReason(t, mismatched))
	require.Empty(t, repo.values)

	ok := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusOK, ok.Code)
	status := decodeErrorDiagnosticOperatorStatus(t, ok)
	require.True(t, status.Enabled)
	require.True(t, status.CaptureAllowed)
	require.False(t, status.BodyRetentionEnabled)
	require.NotNil(t, status.RiskAcknowledgement)
	require.Equal(t, int64(42), status.RiskAcknowledgement.AdminUserID)
	require.Equal(t, service.ErrorDiagnosticRiskAcknowledgementVersion, status.RiskAcknowledgement.Version)
	require.Equal(t, service.ErrorDiagnosticRiskAcknowledgementPhraseEN, status.RiskAcknowledgement.Phrase)
	require.False(t, status.RiskAcknowledgement.AcceptedAt.IsZero())
	require.True(t, status.RiskAcknowledgementCurrent)

	settings := errorDiagnosticOperatorSettingsFrom(t, repo)
	require.True(t, settings.CaptureAllowed())
	require.False(t, settings.BodyCaptureAllowed())

	// 书面确认的原文／版本／时间／操作员 ID 是持久审计记录，不是布尔替身。
	raw := repo.values[service.SettingKeyErrorDiagnosticRiskAcknowledgement]
	require.NotEmpty(t, raw)
	require.Contains(t, raw, fmt.Sprintf(`"admin_user_id":%d`, int64(42)))
	require.Contains(t, raw, service.ErrorDiagnosticRiskAcknowledgementVersion)
	require.Contains(t, raw, service.ErrorDiagnosticRiskAcknowledgementPhraseEN)
}

// 本地化语句：zh* 语言要求中文语句，用英文语句在同一语言下拒绝。
func TestErrorDiagnosticOperatorSettingsLocalizedPhrase(t *testing.T) {
	h, repo := newErrorDiagnosticOperatorHandler(t, nil, true)

	wrongLanguage := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "language": "zh-CN", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 7, "")
	require.Equal(t, http.StatusBadRequest, wrongLanguage.Code)
	require.Equal(t, "ERROR_DIAGNOSTIC_RISK_ACK_INVALID", errorDiagnosticOperatorErrorReason(t, wrongLanguage))
	require.Empty(t, repo.values)

	ok := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "language": "zh-CN", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseZH,
	}, 7, "")
	require.Equal(t, http.StatusOK, ok.Code)
	status := decodeErrorDiagnosticOperatorStatus(t, ok)
	require.True(t, status.Enabled)
	require.Equal(t, service.ErrorDiagnosticRiskAcknowledgementPhraseZH, status.RiskAcknowledgement.Phrase)
}

// 已开启不等于「永久确认」：每次更新都必须重新逐字确认，不能凭已有布尔值绕过。
func TestErrorDiagnosticOperatorSettingsRevalidatesAcknowledgementOnEveryUpdate(t *testing.T) {
	h, repo := newErrorDiagnosticOperatorHandler(t, map[string]string{
		service.SettingKeyErrorDiagnostic: `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":false}`,
		service.SettingKeyErrorDiagnosticRiskAcknowledgement: fmt.Sprintf(
			`{"version":%q,"phrase":%q,"admin_user_id":1,"accepted_at":"2026-09-24T00:00:00Z"}`,
			service.ErrorDiagnosticRiskAcknowledgementVersion,
			service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
		),
	}, true)

	bypass := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "body_retention_enabled": true,
	}, 42, "")
	require.Equal(t, http.StatusBadRequest, bypass.Code)
	require.Equal(t, "ERROR_DIAGNOSTIC_RISK_ACK_REQUIRED", errorDiagnosticOperatorErrorReason(t, bypass))
	require.False(t, errorDiagnosticOperatorSettingsFrom(t, repo).BodyCaptureAllowed(),
		"缺少逐字确认时不得留下正文留存开关")

	ok := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "body_retention_enabled": true,
		"language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusOK, ok.Code)
	require.True(t, errorDiagnosticOperatorSettingsFrom(t, repo).BodyCaptureAllowed())
}

// 正文留存还需要可用的加密密钥：缺密钥时拒绝打开留存，但元数据采集本身仍可开启。
func TestErrorDiagnosticOperatorSettingsBodyRetentionRequiresEncryptionKey(t *testing.T) {
	h, repo := newErrorDiagnosticOperatorHandler(t, nil, false)

	rec := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "body_retention_enabled": true,
		"language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "ERROR_DIAGNOSTIC_BODY_KEY_UNAVAILABLE", errorDiagnosticOperatorErrorReason(t, rec))
	require.Empty(t, repo.values, "缺密钥时不得写入任何门控状态")

	metadataOnly := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusOK, metadataOnly.Code)
	status := decodeErrorDiagnosticOperatorStatus(t, metadataOnly)
	require.True(t, status.Enabled)
	require.False(t, status.BodyEncryptionKeyAvailable)
	require.False(t, status.BodyRetentionEnabled)
	require.False(t, status.BodyRetentionAllowed)
}

// 429 头值留存是与正文正交的第二层：它有自己的开关与自己的门槛（本次确认 + 可用密钥），
// 不会被正文开关顺带打开，也不会因为正文关闭而不可用。
func TestErrorDiagnosticOperatorSettingsHeaderValuesRetentionIsIndependentOfBody(t *testing.T) {
	ctx := context.Background()

	// 只开头值、不写正文：允许，且正文层保持关闭。
	headerOnly, headerRepo := newErrorDiagnosticOperatorHandler(t, nil, true)
	rec := putErrorDiagnosticOperatorSettings(t, headerOnly, map[string]any{
		"enabled": true, "header_values_enabled": true,
		"language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusOK, rec.Code)
	status := decodeErrorDiagnosticOperatorStatus(t, rec)
	require.True(t, status.CaptureAllowed)
	require.True(t, status.HeaderValueRetentionEnabled, "存量开关如实回显")
	require.True(t, status.HeaderValueRetentionAllowed)
	require.False(t, status.BodyRetentionEnabled, "头值开关不得顺带打开正文留存")
	require.False(t, status.BodyRetentionAllowed)

	settings := errorDiagnosticOperatorSettingsFrom(t, headerRepo)
	require.True(t, settings.HeaderValuesCaptureAllowed())
	require.False(t, settings.BodyCaptureAllowed())

	// 只开正文、不写头值：头值层保持关闭。
	bodyOnly, bodyRepo := newErrorDiagnosticOperatorHandler(t, nil, true)
	rec = putErrorDiagnosticOperatorSettings(t, bodyOnly, map[string]any{
		"enabled": true, "body_retention_enabled": true,
		"language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusOK, rec.Code)
	status = decodeErrorDiagnosticOperatorStatus(t, rec)
	require.True(t, status.BodyRetentionAllowed)
	require.False(t, status.HeaderValueRetentionEnabled, "正文开关不得顺带打开头值留存")
	require.False(t, status.HeaderValueRetentionAllowed)
	require.False(t, errorDiagnosticOperatorSettingsFrom(t, bodyRepo).HeaderValuesCaptureAllowed())

	// 两者都开也互不干扰（共用同一把稳定密钥）。
	both, _ := newErrorDiagnosticOperatorHandler(t, nil, true)
	rec = putErrorDiagnosticOperatorSettings(t, both, map[string]any{
		"enabled": true, "body_retention_enabled": true, "header_values_enabled": true,
		"language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusOK, rec.Code)
	status = decodeErrorDiagnosticOperatorStatus(t, rec)
	require.True(t, status.BodyRetentionAllowed)
	require.True(t, status.HeaderValueRetentionAllowed)

	// 采集侧同样认这个开关：状态里写着可留存，门控读出来也必须可留存。
	effective, err := both.settingService.GetErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.True(t, effective.HeaderValuesCaptureAllowed())
	require.True(t, effective.BodyCaptureAllowed())
}

// 头值留存与正文一样需要可用的稳定密钥：缺密钥时拒绝打开，且不留下任何部分状态。
func TestErrorDiagnosticOperatorSettingsHeaderValuesRequireEncryptionKey(t *testing.T) {
	h, repo := newErrorDiagnosticOperatorHandler(t, nil, false)

	rec := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "header_values_enabled": true,
		"language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "ERROR_DIAGNOSTIC_HEADER_KEY_UNAVAILABLE", errorDiagnosticOperatorErrorReason(t, rec))
	require.Empty(t, repo.values, "缺密钥时不得写入任何门控状态")

	// 密钥不是采集的前置条件：只开采集仍然可以。
	metadataOnly := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusOK, metadataOnly.Code)
	status := decodeErrorDiagnosticOperatorStatus(t, metadataOnly)
	require.True(t, status.CaptureAllowed)
	require.False(t, status.BodyEncryptionKeyAvailable)
	require.False(t, status.HeaderValueRetentionAllowed)
	require.False(t, status.HeaderValueRetentionEnabled)
}

// 存量的头值开关开着、但部署拿不出稳定密钥时，状态必须如实显示「存量开着、结论是不可用」：
// 既不能把不可用伪装成关闭，也不能报成有效留存。
//
// 同一份存量值下，正文那一层的收窄会写回读取器（BodyRetentionEnabled 变 false、意图留在
// BodyRetentionRequested），而头值这一层**刻意不写回**：传输接缝要靠存量开关推导「本次要求过
// 头值留存」，收窄成 false 会让缺密钥的配置故障退化成 not_observed，丢掉稳定原因码。
func TestErrorDiagnosticOperatorSettingsHeaderValuesStoredOnWithoutKeyIsNotEffective(t *testing.T) {
	ctx := context.Background()
	h, _ := newErrorDiagnosticOperatorHandler(t, map[string]string{
		service.SettingKeyErrorDiagnostic: `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":true,"header_values_enabled":true}`,
		service.SettingKeyErrorDiagnosticRiskAcknowledgement: fmt.Sprintf(
			`{"version":%q,"phrase":%q,"admin_user_id":1,"accepted_at":"2026-09-24T00:00:00Z"}`,
			service.ErrorDiagnosticRiskAcknowledgementVersion,
			service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
		),
	}, false)

	rec := getErrorDiagnosticOperatorSettings(t, h)
	require.Equal(t, http.StatusOK, rec.Code)
	status := decodeErrorDiagnosticOperatorStatus(t, rec)
	require.True(t, status.HeaderValueRetentionEnabled, "存量开关如实回显")
	require.False(t, status.HeaderValueRetentionAllowed, "没有稳定密钥就不是有效头值留存")
	require.False(t, status.BodyRetentionAllowed)
	require.False(t, status.BodyEncryptionKeyAvailable)
	require.True(t, status.CaptureAllowed, "两层留存都不可用不影响元数据采集")

	settings, err := h.settingService.GetErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.True(t, settings.CaptureAllowed())
	require.False(t, settings.BodyRetentionEnabled, "正文那一层读出来即已收窄")
	require.True(t, settings.BodyRetentionRequested, "正文的存量意图仍被带出")
	require.True(t, settings.HeaderValueRetentionEnabled, "头值开关不得在读取器上被收窄")
	require.True(t, settings.HeaderValuesCaptureAllowed())
}

// 关闭必须永远可行：不需要逐字确认、不需要操作员身份，并强制关掉正文留存**与头值留存**。
//
// 两个留存开关都不接受「关一半」：即使请求里把它们写成 true，关闭也必须把它们一起关掉。
func TestErrorDiagnosticOperatorSettingsDisableNeedsNoPhraseAndForcesRetentionOff(t *testing.T) {
	h, repo := newErrorDiagnosticOperatorHandler(t, map[string]string{
		service.SettingKeyErrorDiagnostic: `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":true,"header_values_enabled":true}`,
		service.SettingKeyErrorDiagnosticRiskAcknowledgement: fmt.Sprintf(
			`{"version":%q,"phrase":%q,"admin_user_id":1,"accepted_at":"2026-09-24T00:00:00Z"}`,
			service.ErrorDiagnosticRiskAcknowledgementVersion,
			service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
		),
	}, true)

	rec := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": false, "body_retention_enabled": true, "header_values_enabled": true,
	}, 42, "")
	require.Equal(t, http.StatusOK, rec.Code)
	status := decodeErrorDiagnosticOperatorStatus(t, rec)
	require.False(t, status.Enabled)
	require.False(t, status.BodyRetentionEnabled)
	require.False(t, status.HeaderValueRetentionEnabled)
	require.False(t, status.HeaderValueRetentionAllowed)
	settings := errorDiagnosticOperatorSettingsFrom(t, repo)
	require.False(t, settings.CaptureAllowed())
	require.False(t, settings.BodyCaptureAllowed())
	require.False(t, settings.HeaderValuesCaptureAllowed())

	// 兜底方向：机器凭证也必须能关掉（这里不允许出现「关不掉」）。
	byMachine := putErrorDiagnosticOperatorSettings(t, h, map[string]any{"enabled": false}, 0, service.AuditAuthMethodAdminAPIKey)
	require.Equal(t, http.StatusOK, byMachine.Code)

	// 书面确认记录作为历史审计保留，不随关闭被抹掉。
	require.NotEmpty(t, repo.values[service.SettingKeyErrorDiagnosticRiskAcknowledgement])
}

// 库里有一条「上一版语句」的有效形状确认时，不得静默开启任何一层留存：
// 旧确认不覆盖当前语句，因此既没有采集，也没有头值留存，且提交旧语句同样被拒。
func TestErrorDiagnosticOperatorSettingsOutdatedAcknowledgementDoesNotSilentlyEnable(t *testing.T) {
	ctx := context.Background()
	h, repo := newErrorDiagnosticOperatorHandler(t, map[string]string{
		service.SettingKeyErrorDiagnostic: `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":true,"header_values_enabled":true}`,
		service.SettingKeyErrorDiagnosticRiskAcknowledgement: fmt.Sprintf(
			`{"version":%q,"phrase":%q,"admin_user_id":1,"accepted_at":"2026-09-24T00:00:00Z"}`,
			errorDiagnosticPreviousRiskVersion,
			errorDiagnosticPreviousRiskPhraseEN,
		),
	}, true)

	// 状态：存量值照实回显，但校验结论必须全部是「不可用」。
	rec := getErrorDiagnosticOperatorSettings(t, h)
	require.Equal(t, http.StatusOK, rec.Code)
	status := decodeErrorDiagnosticOperatorStatus(t, rec)
	require.True(t, status.Enabled, "存量门控值如实回显")
	require.True(t, status.BodyRetentionEnabled)
	require.True(t, status.HeaderValueRetentionEnabled)
	require.False(t, status.RiskAcknowledgementCurrent, "旧版本语句不覆盖当前语句")
	require.False(t, status.CaptureAllowed)
	require.False(t, status.BodyRetentionAllowed)
	require.False(t, status.HeaderValueRetentionAllowed, "旧确认不得静默打开头值留存")

	// 采集侧同样 fail closed：两层留存都不会运行。
	settings, err := h.settingService.GetErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.False(t, settings.CaptureAllowed())
	require.False(t, settings.BodyCaptureAllowed())
	require.False(t, settings.HeaderValuesCaptureAllowed())

	// 提交旧语句不能重新确认：逐字确认的必须是**当前**这段文字。
	rec = putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "header_values_enabled": true,
		"language": "en", "phrase": errorDiagnosticPreviousRiskPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "ERROR_DIAGNOSTIC_RISK_ACK_INVALID", errorDiagnosticOperatorErrorReason(t, rec))
	require.Contains(t, repo.values[service.SettingKeyErrorDiagnosticRiskAcknowledgement], errorDiagnosticPreviousRiskVersion,
		"被拒的请求不得改写存量的确认记录")

	// 逐字确认当前语句之后，头值留存才真正可开。
	rec = putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "header_values_enabled": true,
		"language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusOK, rec.Code)
	status = decodeErrorDiagnosticOperatorStatus(t, rec)
	require.True(t, status.RiskAcknowledgementCurrent)
	require.True(t, status.HeaderValueRetentionAllowed)
}

// 机器凭证不能代替操作员的书面确认：admin API key 一律不能开启。
func TestErrorDiagnosticOperatorSettingsEnableRejectsAdminAPIKey(t *testing.T) {
	h, repo := newErrorDiagnosticOperatorHandler(t, nil, true)

	rec := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, service.AuditAuthMethodAdminAPIKey)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "ERROR_DIAGNOSTIC_ADMIN_API_KEY_FORBIDDEN", errorDiagnosticOperatorErrorReason(t, rec))
	require.Empty(t, repo.values)
}

// 没有可记录的操作员身份时 fail closed：不能把操作员 ID 记成 0 来假装已书面确认。
func TestErrorDiagnosticOperatorSettingsEnableWithoutOperatorIdentityFailsClosed(t *testing.T) {
	h, repo := newErrorDiagnosticOperatorHandler(t, nil, true)

	rec := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 0, "")
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "ERROR_DIAGNOSTIC_OPERATOR_SESSION_REQUIRED", errorDiagnosticOperatorErrorReason(t, rec))
	require.Empty(t, repo.values)
}

// 读取失败不得渲染成「已关闭」：宁可显式不可用，也不假装门控是关的。
func TestErrorDiagnosticOperatorSettingsReadFailureIsNotRenderedAsOff(t *testing.T) {
	h, repo := newErrorDiagnosticOperatorHandler(t, nil, true)
	repo.failGet = map[string]error{service.SettingKeyErrorDiagnostic: errors.New("storage down")}

	rec := getErrorDiagnosticOperatorSettings(t, h)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "ERROR_DIAGNOSTIC_SETTINGS_UNAVAILABLE", errorDiagnosticOperatorErrorReason(t, rec))
	require.NotContains(t, rec.Body.String(), `"enabled":true`)
}

// 存量值损坏同样按不可用处理，不能用「全关」掩盖解析失败。
func TestErrorDiagnosticOperatorSettingsCorruptStoredValueIsUnavailable(t *testing.T) {
	h, _ := newErrorDiagnosticOperatorHandler(t, map[string]string{
		service.SettingKeyErrorDiagnostic: "{not json",
	}, true)

	rec := getErrorDiagnosticOperatorSettings(t, h)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "ERROR_DIAGNOSTIC_SETTINGS_UNAVAILABLE", errorDiagnosticOperatorErrorReason(t, rec))
}

// 写入失败必须整体失败：不得出现「记录写下了、门控没打开」以外的半开状态，
// 也不得在失败时返回成功。
func TestErrorDiagnosticOperatorSettingsWriteFailureIsReported(t *testing.T) {
	h, repo := newErrorDiagnosticOperatorHandler(t, nil, true)
	repo.failWrite = errors.New("storage down")

	rec := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.GreaterOrEqual(t, rec.Code, 500)
	require.False(t, errorDiagnosticOperatorSettingsFrom(t, repo).CaptureAllowed())
}

// 状态响应只含门控与确认元数据：不出现主密钥、正文或账号级身份字段。
func TestErrorDiagnosticOperatorSettingsStatusExposesNoSecret(t *testing.T) {
	h, _ := newErrorDiagnosticOperatorHandler(t, map[string]string{
		service.SettingKeyErrorDiagnostic: `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":true}`,
		service.SettingKeyErrorDiagnosticRiskAcknowledgement: fmt.Sprintf(
			`{"version":%q,"phrase":%q,"admin_user_id":1,"accepted_at":"2026-09-24T00:00:00Z"}`,
			service.ErrorDiagnosticRiskAcknowledgementVersion,
			service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
		),
	}, true)

	rec := getErrorDiagnosticOperatorSettings(t, h)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.NotContains(t, body, errorDiagnosticOperatorTestKeyHex, "不得回显加密主密钥")
	require.NotContains(t, body, "body_text")
	require.NotContains(t, body, "ciphertext")
	require.NotContains(t, body, "api_key")
	require.NotContains(t, body, "account_id")
	require.Contains(t, body, service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
		"确认语句本身不是秘密，必须回显给管理员界面")
	status := decodeErrorDiagnosticOperatorStatus(t, rec)
	require.True(t, status.BodyEncryptionKeyAvailable)
	require.True(t, status.BodyRetentionAllowed)
}

// 门控被其它途径打开、但缺少（或只有旧版本）书面确认时，状态必须如实暴露这个不一致：
// 存量布尔值照实回显，但校验结论是明确的「不可采集」，而不是假装已确认过。
func TestErrorDiagnosticOperatorSettingsSurfacesMissingOrStaleAcknowledgement(t *testing.T) {
	// 存量门控为真，但没有任何确认记录。
	h, _ := newErrorDiagnosticOperatorHandler(t, map[string]string{
		service.SettingKeyErrorDiagnostic: `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":false}`,
	}, true)
	rec := getErrorDiagnosticOperatorSettings(t, h)
	require.Equal(t, http.StatusOK, rec.Code)
	status := decodeErrorDiagnosticOperatorStatus(t, rec)
	require.True(t, status.Enabled, "存量门控值如实回显")
	require.True(t, status.RiskAcknowledged, "存量门控值如实回显")
	require.False(t, status.CaptureAllowed, "没有当前版本书面确认就不可采集")
	require.Nil(t, status.RiskAcknowledgement)
	require.False(t, status.RiskAcknowledgementCurrent)

	// 确认记录版本落后于当前语句：同样视为「没有覆盖当前语句的书面确认」。
	stale := `{"version":"v0000.00.00","phrase":"old statement","admin_user_id":1,"accepted_at":"2026-01-01T00:00:00Z"}`
	h, _ = newErrorDiagnosticOperatorHandler(t, map[string]string{
		service.SettingKeyErrorDiagnostic:                    `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":false}`,
		service.SettingKeyErrorDiagnosticRiskAcknowledgement: stale,
	}, true)
	rec = getErrorDiagnosticOperatorSettings(t, h)
	require.Equal(t, http.StatusOK, rec.Code)
	status = decodeErrorDiagnosticOperatorStatus(t, rec)
	require.NotNil(t, status.RiskAcknowledgement)
	require.Equal(t, "v0000.00.00", status.RiskAcknowledgement.Version)
	require.False(t, status.RiskAcknowledgementCurrent, "旧版本语句不构成当前版本的确认")
	require.False(t, status.CaptureAllowed)
	require.True(t, status.RiskAcknowledged, "存量门控值仍如实回显")

	// 残缺记录（缺确认时间）不是证据：不算已确认。
	h, repo := newErrorDiagnosticOperatorHandler(t, map[string]string{
		service.SettingKeyErrorDiagnostic: `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":false}`,
		// 版本用当前版本，使这条用例只隔离「缺确认时间」这一个变量。
		service.SettingKeyErrorDiagnosticRiskAcknowledgement: fmt.Sprintf(
			`{"version":%q,"phrase":"x","admin_user_id":1}`, service.ErrorDiagnosticRiskAcknowledgementVersion),
	}, true)
	rec = getErrorDiagnosticOperatorSettings(t, h)
	require.Equal(t, http.StatusOK, rec.Code)
	status = decodeErrorDiagnosticOperatorStatus(t, rec)
	require.Nil(t, status.RiskAcknowledgement)
	require.False(t, status.RiskAcknowledgementCurrent)
	require.False(t, status.CaptureAllowed)
	require.NotEmpty(t, repo.values[service.SettingKeyErrorDiagnosticRiskAcknowledgement],
		"残缺记录不被采信，但也不由本入口擅自删除")
}

// 门控被越权改写（直接写库、其它管理入口或历史脏数据）时，采集侧门控必须 fail closed：
// 布尔值为真但没有当前版本的书面确认，就按全关处理，绝不因为存量值而开始采集。
func TestErrorDiagnosticCaptureGateFailsClosedOnTamperedBlob(t *testing.T) {
	ctx := context.Background()
	tampered := `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":true}`

	// 只有门控键、没有任何书面确认。
	h, repo := newErrorDiagnosticOperatorHandler(t, map[string]string{
		service.SettingKeyErrorDiagnostic: tampered,
	}, true)
	settings, err := h.settingService.GetErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.False(t, settings.CaptureAllowed(), "缺少书面确认时不得采集")
	require.False(t, settings.BodyCaptureAllowed(), "缺少书面确认时也不得留存正文")

	// 只有旧版本的确认记录：同样不构成当前版本的书面确认。
	repo.values[service.SettingKeyErrorDiagnosticRiskAcknowledgement] =
		`{"version":"v0000.00.00","phrase":"old statement","admin_user_id":1,"accepted_at":"2026-01-01T00:00:00Z"}`
	settings, err = h.settingService.GetErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.False(t, settings.CaptureAllowed(), "旧版本确认不构成当前版本的书面确认")

	// 残缺记录（缺确认时间）不是证据（版本用当前版本，只隔离「缺确认时间」这一个变量）。
	repo.values[service.SettingKeyErrorDiagnosticRiskAcknowledgement] = fmt.Sprintf(
		`{"version":%q,"phrase":"x","admin_user_id":1}`, service.ErrorDiagnosticRiskAcknowledgementVersion)
	settings, err = h.settingService.GetErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.False(t, settings.CaptureAllowed())

	// 覆盖当前版本的有效确认才放行（且不误伤正常开启）。
	repo.values[service.SettingKeyErrorDiagnosticRiskAcknowledgement] = fmt.Sprintf(
		`{"version":%q,"phrase":%q,"admin_user_id":1,"accepted_at":"2026-09-24T00:00:00Z"}`,
		service.ErrorDiagnosticRiskAcknowledgementVersion,
		service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	)
	settings, err = h.settingService.GetErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.True(t, settings.CaptureAllowed())
	require.True(t, settings.BodyCaptureAllowed())

	// 存量门控值仍可读取（运维界面用它展示「存了什么」），且不带确认校验。
	stored, err := h.settingService.ReadStoredErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.True(t, stored.CaptureAllowed(), "存量值原样返回，校验只发生在采集门控上")

	// 门控本来就关时保持原样：关闭态不依赖确认记录。
	off, _ := newErrorDiagnosticOperatorHandler(t, map[string]string{
		service.SettingKeyErrorDiagnostic: `{"enabled":false}`,
	}, true)
	settings, err = off.settingService.GetErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.False(t, settings.CaptureAllowed())

	// 确认键读不出来时同样 fail closed（热路径不抛错，采集直接停）。
	broken, brokenRepo := newErrorDiagnosticOperatorHandler(t, map[string]string{
		service.SettingKeyErrorDiagnostic: tampered,
	}, true)
	brokenRepo.failGet = map[string]error{
		service.SettingKeyErrorDiagnosticRiskAcknowledgement: errors.New("storage down"),
	}
	settings, err = broken.settingService.GetErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.False(t, settings.CaptureAllowed(), "确认记录读不出来时不得放行采集")

	// 该故障由运维状态接口显式暴露：报「不可用」，而不是显示成「未确认」。
	rec := getErrorDiagnosticOperatorSettings(t, broken)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "ERROR_DIAGNOSTIC_SETTINGS_UNAVAILABLE", errorDiagnosticOperatorErrorReason(t, rec))
}

// 确认校验只读确认键：不读门控键，因此与门控读取路径不存在递归依赖。
func TestErrorDiagnosticRiskAcknowledgementCurrentDoesNotReadGateKey(t *testing.T) {
	ctx := context.Background()
	h, repo := newErrorDiagnosticOperatorHandler(t, map[string]string{
		service.SettingKeyErrorDiagnosticRiskAcknowledgement: fmt.Sprintf(
			`{"version":%q,"phrase":%q,"admin_user_id":1,"accepted_at":"2026-09-24T00:00:00Z"}`,
			service.ErrorDiagnosticRiskAcknowledgementVersion,
			service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
		),
	}, true)
	repo.failGet = map[string]error{service.SettingKeyErrorDiagnostic: errors.New("gate key must not be read")}

	current, err := h.settingService.ErrorDiagnosticRiskAcknowledgementCurrent(ctx)
	require.NoError(t, err)
	require.True(t, current)
}

// 越权改写后的状态读取必须把「不可采集」显示得毫不含糊。
func TestErrorDiagnosticOperatorSettingsStatusFailsClosedOnTamperedBlob(t *testing.T) {
	h, repo := newErrorDiagnosticOperatorHandler(t, map[string]string{
		service.SettingKeyErrorDiagnostic: `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":true}`,
	}, true)

	rec := getErrorDiagnosticOperatorSettings(t, h)
	require.Equal(t, http.StatusOK, rec.Code)
	status := decodeErrorDiagnosticOperatorStatus(t, rec)
	require.True(t, status.Enabled, "存量门控值如实回显")
	require.True(t, status.RiskAcknowledged)
	require.True(t, status.BodyRetentionEnabled)
	require.False(t, status.CaptureAllowed, "没有当前版本书面确认时不可采集")
	require.False(t, status.BodyRetentionAllowed)
	require.Nil(t, status.RiskAcknowledgement)
	require.False(t, status.RiskAcknowledgementCurrent)

	// 正常开启后（确认记录与门控同批写入）才是真正可采集。
	ok := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "body_retention_enabled": true,
		"language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusOK, ok.Code)
	status = decodeErrorDiagnosticOperatorStatus(t, ok)
	require.True(t, status.CaptureAllowed)
	require.True(t, status.BodyRetentionAllowed)
	require.True(t, status.RiskAcknowledgementCurrent)
	require.NotEmpty(t, repo.values[service.SettingKeyErrorDiagnostic])
}

// 操作员来源信息留在持久记录里供审计，但不随诊断相关的响应面扩散。
func TestErrorDiagnosticOperatorSettingsStatusKeepsOperatorOriginOutOfResponse(t *testing.T) {
	h, repo := newErrorDiagnosticOperatorHandler(t, nil, true)

	rec := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusOK, rec.Code)

	require.Contains(t, repo.values[service.SettingKeyErrorDiagnosticRiskAcknowledgement], `"user_agent":"error-diagnostic-test"`,
		"来源信息是持久记录的一部分")
	require.NotContains(t, rec.Body.String(), "user_agent")
	require.NotContains(t, rec.Body.String(), "ip_address")
}

// errorDiagnosticOperatorLogSink 捕获结构化日志，用于证明开关审计可见且不含敏感原文。
type errorDiagnosticOperatorLogSink struct {
	mu     sync.Mutex
	events []*logger.LogEvent
}

func (s *errorDiagnosticOperatorLogSink) WriteLogEvent(event *logger.LogEvent) {
	if event == nil {
		return
	}
	cloned := *event
	if event.Fields != nil {
		cloned.Fields = make(map[string]any, len(event.Fields))
		for key, value := range event.Fields {
			cloned.Fields[key] = value
		}
	}
	s.mu.Lock()
	s.events = append(s.events, &cloned)
	s.mu.Unlock()
}

func (s *errorDiagnosticOperatorLogSink) fieldValue(message, field string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range s.events {
		if event == nil || event.Message != message || event.Fields == nil {
			continue
		}
		if value, ok := event.Fields[field]; ok {
			return value, true
		}
	}
	return nil, false
}

func (s *errorDiagnosticOperatorLogSink) containsText(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range s.events {
		if event == nil {
			continue
		}
		if strings.Contains(event.Message, substr) {
			return true
		}
		for key, value := range event.Fields {
			if strings.Contains(key, substr) || strings.Contains(fmt.Sprint(value), substr) {
				return true
			}
		}
	}
	return false
}

var errorDiagnosticOperatorLogCaptureMu sync.Mutex

func captureErrorDiagnosticOperatorLogs(t *testing.T) (*errorDiagnosticOperatorLogSink, func()) {
	t.Helper()
	errorDiagnosticOperatorLogCaptureMu.Lock()
	err := logger.Init(logger.InitOptions{
		Level:       "debug",
		Format:      "json",
		ServiceName: "sub2api",
		Environment: "test",
		Output:      logger.OutputOptions{ToStdout: true, ToFile: false},
		Sampling:    logger.SamplingOptions{Enabled: false},
	})
	require.NoError(t, err)
	sink := &errorDiagnosticOperatorLogSink{}
	logger.SetSink(sink)
	return sink, func() {
		logger.SetSink(nil)
		errorDiagnosticOperatorLogCaptureMu.Unlock()
	}
}

// 开关审计可见（操作员、目标状态、确认版本），但绝不出现逐字确认原文或密钥材料。
func TestErrorDiagnosticOperatorSettingsSwitchIsAuditedWithoutPlaintextPhrase(t *testing.T) {
	sink, stop := captureErrorDiagnosticOperatorLogs(t)
	defer stop()

	h, _ := newErrorDiagnosticOperatorHandler(t, nil, true)
	rec := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled": true, "header_values_enabled": true,
		"language": "en", "phrase": service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusOK, rec.Code)

	operatorID, ok := sink.fieldValue("error_diagnostic.operator_settings_updated", "admin_user_id")
	require.True(t, ok, "开启必须留下可审计的运维开关事件")
	require.Equal(t, int64(42), operatorID)
	version, ok := sink.fieldValue("error_diagnostic.operator_settings_updated", "risk_version")
	require.True(t, ok)
	require.Equal(t, service.ErrorDiagnosticRiskAcknowledgementVersion, version)
	// 两个留存层各自可见：审计要能看出本次究竟打开了哪一层，而不是只看一个合并后的门控。
	bodyEnabled, ok := sink.fieldValue("error_diagnostic.operator_settings_updated", "body_retention_enabled")
	require.True(t, ok, "日志必须披露正文留存的开关状态")
	require.Equal(t, false, bodyEnabled)
	headerEnabled, ok := sink.fieldValue("error_diagnostic.operator_settings_updated", "header_values_enabled")
	require.True(t, ok, "日志必须披露头值留存的开关状态")
	require.Equal(t, true, headerEnabled)

	require.False(t, sink.containsText(service.ErrorDiagnosticRiskAcknowledgementPhraseEN),
		"逐字确认原文不得进入日志")
	require.False(t, sink.containsText("I acknowledge that error diagnostic"))
	require.False(t, sink.containsText(errorDiagnosticOperatorTestKeyHex), "密钥材料不得进入日志")
}

// 未授权的运维开关请求不得被当成有效确认写入（防线在路由层，这里锁定 handler 层的 fail closed）。
func TestErrorDiagnosticOperatorSettingsUnavailableServiceFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/error-diagnostic", nil)
	NewSettingHandler(nil, nil, nil, nil, nil, nil, nil).GetErrorDiagnosticOperatorSettings(c)
	require.GreaterOrEqual(t, rec.Code, 500)

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/error-diagnostic",
		bytes.NewBufferString(`{"enabled":true,"phrase":"`+service.ErrorDiagnosticRiskAcknowledgementPhraseEN+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	NewSettingHandler(nil, nil, nil, nil, nil, nil, nil).UpdateErrorDiagnosticOperatorSettings(c)
	require.GreaterOrEqual(t, rec.Code, 500)
}
