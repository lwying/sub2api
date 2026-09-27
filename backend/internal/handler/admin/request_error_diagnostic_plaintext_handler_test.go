//go:build unit

package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/stretchr/testify/require"
)

// 票据 08／09 的新明文格式在管理端披露层的规则：
//   - 到期判据随格式不同（旧密文七天窗口 vs 新明文随 usage／三十天）；
//   - 正文揭示仍然只走原管理员权限（本能力**不**新增 step-up）；
//   - 响应必须说清这是明文留存、以及它是否随 usage 存在；
//   - 已关联 usage 的明文行不受三十天元数据窗口约束。

func newPlainDiagnosticRecord(linked bool) service.ErrorDiagnosticRecord {
	record := service.ErrorDiagnosticRecord{
		ID:                 errorDiagnosticTestID,
		Protocol:           service.ErrorDiagnosticProtocolMessages,
		AttemptIndex:       1,
		UpstreamStatusCode: 429,
		CreatedAt:          errorDiagnosticTestNow.Add(-40 * 24 * time.Hour),
		// 已经过了三十天：旧格式在这里必然不可读，新明文只在「未关联」时不可读。
		MetadataExpiresAt:     errorDiagnosticTestNow.Add(-10 * 24 * time.Hour),
		PlainRecord:           true,
		PlainLinked:           linked,
		PlainOwnerUsageLogID:  0,
		PlainBodyState:        service.ErrorDiagnosticBodyStateStored,
		PlainBodyReason:       service.ErrorDiagnosticPlainBodyRetained,
		PlainBodyStored:       true,
		PlainHeaderState:      service.ErrorDiagnosticHeaderStateStored,
		PlainHeaderReason:     service.ErrorDiagnosticPlainHeaderRetained,
		PlainHeaderStored:     true,
		PlainHeaderEntryCount: 2,
	}
	if linked {
		record.PlainOwnerUsageLogID = 15
		record.UsageLogID = 15
		record.HasUsage = true
	}
	return record
}

func TestPlainDiagnosticMetadataSurvivesTheMetadataWindowOnlyWhileLinked(t *testing.T) {
	for _, tc := range []struct {
		name       string
		linked     bool
		wantStatus int
	}{
		{name: "linked rows follow their usage", linked: true, wantStatus: http.StatusOK},
		{name: "unlinked rows stop exactly at the 30 day cutoff", linked: false, wantStatus: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &errorDiagnosticReaderStub{record: newPlainDiagnosticRecord(tc.linked)}
			router := newErrorDiagnosticTestRouter(reader)

			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID, nil))
			require.Equal(t, tc.wantStatus, response.Code)
			if tc.wantStatus != http.StatusOK {
				return
			}
			var payload map[string]any
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
			data := payload["data"].(map[string]any)
			require.Equal(t, service.ErrorDiagnosticFormatPlaintext, data["body_format"])
			require.Equal(t, service.ErrorDiagnosticFormatPlaintext, data["header_format"])
			require.Equal(t, true, data["usage_linked"])
			// 新明文行的状态来自明文列，而不是旧密文列（旧列在这一行是 not_observed）。
			require.Equal(t, service.ErrorDiagnosticBodyStateStored, data["body_state"])
			require.Equal(t, service.ErrorDiagnosticPlainBodyRetained, data["reason"])
			require.Equal(t, service.ErrorDiagnosticHeaderStateStored, data["header_state"])
			require.Equal(t, service.ErrorDiagnosticPlainHeaderRetained, data["header_reason"])
			require.EqualValues(t, 2, data["header_entry_count"])
		})
	}
}

func TestPlainBodyRevealNeedsNoStepUpAndFollowsTheFormatRules(t *testing.T) {
	linked := newPlainDiagnosticRecord(true)
	reader := &errorDiagnosticReaderStub{
		record: linked,
		body:   []byte(`{"model":"claude"}`),
	}
	router := newErrorDiagnosticTestRouter(reader)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/body", nil))
	require.Equal(t, http.StatusOK, response.Code, "正文揭示沿用原管理员权限，不新增 step-up")
	require.Equal(t, "no-store, private", response.Header().Get("Cache-Control"))

	var payload map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	data := payload["data"].(map[string]any)
	require.Equal(t, `{"model":"claude"}`, data["body_text"])
	require.Equal(t, service.ErrorDiagnosticFormatPlaintext, data["body_format"])
	require.Equal(t, true, data["usage_linked"])
}

func TestPlainHeaderRevealHasNoInventedExpiry(t *testing.T) {
	record := newPlainDiagnosticRecord(true)
	record.MetadataExpiresAt = errorDiagnosticTestNow.Add(24 * time.Hour)
	reader := &errorDiagnosticReaderStub{
		record: record,
		headerValues: service.ErrorDiagnosticHeaderValues{Response: map[string]string{"Retry-After": "30"}},
	}
	router := newErrorDiagnosticTestRouter(reader)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/headers", nil))
	require.Equal(t, http.StatusOK, response.Code)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	data := payload["data"].(map[string]any)
	require.Nil(t, data["header_expires_at"], "明文随 usage 不得发出公元 1 年的假到期时间")
}

func TestPlainBodyRevealRefusesUnlinkedRowsAtTheCutoff(t *testing.T) {
	reader := &errorDiagnosticReaderStub{
		record: newPlainDiagnosticRecord(false),
		body:   []byte(`{"model":"claude"}`),
	}
	router := newErrorDiagnosticTestRouter(reader)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/body", nil))
	// 元数据已经过了三十天，未关联的行连详情都不可披露。
	require.Equal(t, http.StatusNotFound, response.Code)
}

func TestPurgedPlainBodyIsNotRevealable(t *testing.T) {
	record := newPlainDiagnosticRecord(true)
	record.MetadataExpiresAt = errorDiagnosticTestNow.Add(24 * time.Hour)
	record.PlainBodyStored = false
	record.PlainBodyState = service.ErrorDiagnosticBodyStatePurged
	reader := &errorDiagnosticReaderStub{record: record, body: []byte(`{"model":"claude"}`)}
	router := newErrorDiagnosticTestRouter(reader)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/body", nil))
	require.Equal(t, http.StatusGone, response.Code)
	require.Zero(t, reader.bodyCalls, "已被清理的明文不得进入读取层")
}

func TestLegacyRowKeepsTheEncryptedFormatLabels(t *testing.T) {
	// 回归：旧格式行的披露一字不改——它仍然是 encrypted，并且仍然受七天窗口约束。
	record := service.ErrorDiagnosticRecord{
		ID:                 errorDiagnosticTestID,
		Protocol:           service.ErrorDiagnosticProtocolMessages,
		AttemptIndex:       1,
		UpstreamStatusCode: 500,
		CreatedAt:          errorDiagnosticTestNow.Add(-time.Hour),
		MetadataExpiresAt:  errorDiagnosticTestNow.Add(24 * time.Hour),
		BodyState:          service.ErrorDiagnosticBodyStateStored,
		BodyReason:         service.ErrorDiagnosticBodyRetained,
		BodyStored:         true,
		BodyExpiresAt:      errorDiagnosticTestNow.Add(24 * time.Hour),
		HeaderState:        service.ErrorDiagnosticHeaderStateNotObserved,
		HeaderReason:       service.ErrorDiagnosticHeaderNotObserved,
	}
	reader := &errorDiagnosticReaderStub{record: record}
	router := newErrorDiagnosticTestRouter(reader)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID, nil))
	require.Equal(t, http.StatusOK, response.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	data := payload["data"].(map[string]any)
	require.Equal(t, service.ErrorDiagnosticFormatEncrypted, data["body_format"])
	require.Equal(t, service.ErrorDiagnosticFormatEncrypted, data["header_format"])
	require.Equal(t, false, data["usage_linked"])
	require.Equal(t, service.ErrorDiagnosticBodyStateStored, data["body_state"])
	require.Equal(t, service.ErrorDiagnosticBodyRetained, data["reason"])
}

// TestErrorDiagnosticOperatorSettingsRequestCarriesPlaintextLayers 覆盖 DTO 不得吞掉
// 新明文层的字段：请求里带了开关与语句就必须真的落到存储层，否则界面能点、后端永远收不到。
func TestErrorDiagnosticOperatorSettingsRequestCarriesPlaintextLayers(t *testing.T) {
	// 明文层不看密钥，因此这里用「没有稳定密钥」的部署，证明它们照样能开。
	h, repo := newErrorDiagnosticOperatorHandler(t, nil, false)

	rec := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled":                     true,
		"language":                    "en",
		"phrase":                      service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
		"plain_body_enabled":          true,
		"plain_body_phrase":           service.ErrorDiagnosticPlainBodyRiskAcknowledgementPhraseEN,
		"plain_header_values_enabled": true,
		"plain_header_values_phrase":  service.ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusOK, rec.Code)

	stored := plaintextStoredOperatorSettings(t, repo)
	require.True(t, stored.PlainBodyRetentionEnabled, "明文正文层必须真的被打开")
	require.True(t, stored.PlainHeaderValueRetentionEnabled, "明文头值层必须真的被打开")
	require.NotEmpty(t, repo.values[service.SettingKeyErrorDiagnosticPlainBodyRiskAck])
	require.NotEmpty(t, repo.values[service.SettingKeyErrorDiagnosticPlainHeaderValuesRiskAck])
}

// plaintextStoredOperatorSettings 读出门控键里落下的存量布尔值。
//
// 键不存在（被拒的更新什么都没写）等同「全关」，而不是读取错误：这里要断言的正是
// 「没有落任何值」。
func plaintextStoredOperatorSettings(t *testing.T, repo *errorDiagnosticOperatorRepoStub) service.ErrorDiagnosticSettings {
	t.Helper()
	raw := repo.values[service.SettingKeyErrorDiagnostic]
	var settings service.ErrorDiagnosticSettings
	if raw == "" {
		return settings
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &settings))
	return settings
}

// TestErrorDiagnosticOperatorSettingsRejectsTheSharedPhraseForPlaintextLayers 覆盖
// 「旧共享语句打不开新明文层」这条门槛在 HTTP 面上同样成立。
func TestErrorDiagnosticOperatorSettingsRejectsTheSharedPhraseForPlaintextLayers(t *testing.T) {
	h, repo := newErrorDiagnosticOperatorHandler(t, nil, true)

	rec := putErrorDiagnosticOperatorSettings(t, h, map[string]any{
		"enabled":            true,
		"language":           "en",
		"phrase":             service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
		"plain_body_enabled": true,
		"plain_body_phrase":  service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
	}, 42, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, repo.values[service.SettingKeyErrorDiagnosticPlainBodyRiskAck],
		"被拒的更新不得留下任何新明文层的确认记录")
	require.False(t, plaintextStoredOperatorSettings(t, repo).PlainBodyRetentionEnabled)
}
