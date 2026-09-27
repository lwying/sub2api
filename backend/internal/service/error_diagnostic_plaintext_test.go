//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

// 票据 08／09 的新明文层：独立开关、独立逐字风险确认、独立到期规则。
//
// 这里锁定的是服务层那部分事实：
//   - 新明文层默认关闭，且**旧共享确认不能启用它**（版本化逐字确认是门槛的一部分）；
//   - 两个新层（正文 / 429 头值）互相独立，谁也打不开谁；
//   - 明文正文开启不依赖任何加密密钥（新格式没有密钥要求）；
//   - 明文行的可读性由「是否已关联 usage」决定，而不是旧的七天窗口。

func newPlaintextSettingsService(values map[string]string) (*SettingService, *errorDiagnosticOperatorSettingRepo) {
	repo := newErrorDiagnosticOperatorSettingRepo(values)
	svc := NewSettingService(repo, &config.Config{})
	// 这里的用例讨论的是「逐字确认」这道门槛，部署前提必须显式成立：探针缺失时两个新明文层
	// 会先被**部署前提**关掉（fail closed），用例就测不到自己关心的那道门槛了。
	// 部署前提本身的关闭行为由 plaintext_capture_support_test.go 覆盖。
	svc.SetPlaintextCaptureSupportProbe(supportedPlaintextCaptureProbe())
	return svc, repo
}

func plaintextStoredSettings(t *testing.T, repo *errorDiagnosticOperatorSettingRepo) ErrorDiagnosticSettings {
	t.Helper()
	return repo.storedSettings()
}

// TestPlaintextLayersRequireTheirOwnStoredFlagAndAcknowledgement 覆盖两个新层的门槛：
// 存量布尔值 + 覆盖当前语句版本的逐字确认，缺一不可。
func TestPlaintextLayersRequireTheirOwnStoredFlagAndAcknowledgement(t *testing.T) {
	ctx := context.Background()

	t.Run("stored flag without any confirmation is not enough", func(t *testing.T) {
		values := map[string]string{
			SettingKeyErrorDiagnostic: `{"enabled":true,"risk_acknowledged":true,"plain_body_enabled":true,"plain_header_values_enabled":true}`,
		}
		service, _ := newPlaintextSettingsService(values)
		settings, err := service.GetErrorDiagnosticSettings(ctx)
		require.NoError(t, err)
		require.False(t, settings.PlainBodyCaptureAllowed())
		require.False(t, settings.PlainHeaderValuesCaptureAllowed())
	})

	t.Run("the legacy shared statement does not enable the new layers", func(t *testing.T) {
		// 升级前共享确认只覆盖原密文声明，不授权升级后的新采集或明文留存。
		sharedAck := ErrorDiagnosticRiskAcknowledgement{
			Version:     "v2026.09.24.1",
			Phrase:      ErrorDiagnosticRiskAcknowledgementPhraseEN,
			AdminUserID: 7,
			AcceptedAt:  time.Now().UTC(),
		}
		payload, err := json.Marshal(sharedAck)
		require.NoError(t, err)
		values := map[string]string{
			SettingKeyErrorDiagnostic:                    `{"enabled":true,"risk_acknowledged":true,"plain_body_enabled":true,"plain_header_values_enabled":true}`,
			SettingKeyErrorDiagnosticRiskAcknowledgement: string(payload),
		}
		service, _ := newPlaintextSettingsService(values)
		settings, err := service.GetErrorDiagnosticSettings(ctx)
		require.NoError(t, err)
		require.False(t, settings.CaptureAllowed(), "the old shared acknowledgement pauses all new capture after upgrade")
		require.False(t, settings.PlainBodyCaptureAllowed())
		require.False(t, settings.PlainHeaderValuesCaptureAllowed())
	})

	t.Run("each new layer needs its own current statement", func(t *testing.T) {
		bodyAck := ErrorDiagnosticRiskAcknowledgement{
			Version:     ErrorDiagnosticPlainBodyRiskAcknowledgementVersion,
			Phrase:      ErrorDiagnosticPlainBodyRiskAcknowledgementPhraseEN,
			AdminUserID: 7,
			AcceptedAt:  time.Now().UTC(),
		}
		payload, err := json.Marshal(bodyAck)
		require.NoError(t, err)
		values := map[string]string{
			SettingKeyErrorDiagnostic:                         `{"enabled":true,"risk_acknowledged":true,"plain_body_enabled":true,"plain_header_values_enabled":true}`,
			SettingKeyErrorDiagnosticPlainBodyRiskAck:         string(payload),
			SettingKeyErrorDiagnosticRiskAcknowledgement:      mustSharedAckJSON(t),
			SettingKeyErrorDiagnosticPlainHeaderValuesRiskAck: "",
		}
		service, _ := newPlaintextSettingsService(values)
		settings, err := service.GetErrorDiagnosticSettings(ctx)
		require.NoError(t, err)
		require.True(t, settings.PlainBodyCaptureAllowed(), "body layer acknowledged")
		require.False(t, settings.PlainHeaderValuesCaptureAllowed(), "header layer has no confirmation yet")
	})

	t.Run("plaintext body needs no encryption key", func(t *testing.T) {
		values := map[string]string{
			SettingKeyErrorDiagnostic:                    `{"enabled":true,"risk_acknowledged":true,"plain_body_enabled":true}`,
			SettingKeyErrorDiagnosticPlainBodyRiskAck:    mustPlainBodyAckJSON(t),
			SettingKeyErrorDiagnosticRiskAcknowledgement: mustSharedAckJSON(t),
		}
		// 没有配置稳定密钥：旧密文层不可用，新明文层不受影响。
		service, _ := newPlaintextSettingsService(values)
		require.False(t, service.ErrorDiagnosticBodyEncryptionKeyAvailable())
		settings, err := service.GetErrorDiagnosticSettings(ctx)
		require.NoError(t, err)
		require.False(t, settings.BodyCaptureAllowed())
		require.True(t, settings.PlainBodyCaptureAllowed())
	})

	t.Run("capture still has to be running", func(t *testing.T) {
		values := map[string]string{
			SettingKeyErrorDiagnostic:                 `{"enabled":true,"plain_body_enabled":true}`,
			SettingKeyErrorDiagnosticPlainBodyRiskAck: mustPlainBodyAckJSON(t),
		}
		service, _ := newPlaintextSettingsService(values)
		settings, err := service.GetErrorDiagnosticSettings(ctx)
		require.NoError(t, err)
		require.False(t, settings.PlainBodyCaptureAllowed(), "no capture, no rows, no plaintext")
	})
}

func mustSharedAckJSON(t *testing.T) string {
	t.Helper()
	payload, err := json.Marshal(ErrorDiagnosticRiskAcknowledgement{
		Version:     ErrorDiagnosticRiskAcknowledgementVersion,
		Phrase:      ErrorDiagnosticRiskAcknowledgementPhraseEN,
		AdminUserID: 7,
		AcceptedAt:  time.Now().UTC(),
	})
	require.NoError(t, err)
	return string(payload)
}

func mustPlainBodyAckJSON(t *testing.T) string {
	t.Helper()
	payload, err := json.Marshal(ErrorDiagnosticRiskAcknowledgement{
		Version:     ErrorDiagnosticPlainBodyRiskAcknowledgementVersion,
		Phrase:      ErrorDiagnosticPlainBodyRiskAcknowledgementPhraseEN,
		AdminUserID: 7,
		AcceptedAt:  time.Now().UTC(),
	})
	require.NoError(t, err)
	return string(payload)
}

// TestPlaintextStatementMismatchKeepsLayerOff 覆盖「版本对但原文任意」的伪造记录：
// 逐字比对是这道门槛的全部意义，只校验版本不足以采信。
func TestPlaintextStatementMismatchKeepsLayerOff(t *testing.T) {
	ctx := context.Background()
	forged, err := json.Marshal(ErrorDiagnosticRiskAcknowledgement{
		Version:     ErrorDiagnosticPlainBodyRiskAcknowledgementVersion,
		Phrase:      "error diagnostic plaintext bodies are retained",
		AdminUserID: 7,
		AcceptedAt:  time.Now().UTC(),
	})
	require.NoError(t, err)
	values := map[string]string{
		SettingKeyErrorDiagnostic:                    `{"enabled":true,"risk_acknowledged":true,"plain_body_enabled":true}`,
		SettingKeyErrorDiagnosticPlainBodyRiskAck:    string(forged),
		SettingKeyErrorDiagnosticRiskAcknowledgement: mustSharedAckJSON(t),
	}
	service, _ := newPlaintextSettingsService(values)
	settings, err := service.GetErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.False(t, settings.PlainBodyCaptureAllowed())
}

// TestUpdateOperatorSettingsBindsPlaintextLayersToTheirOwnPhrase 覆盖运维入口：
// 打开新明文层的同一次更新必须逐字确认**它自己的**语句，旧共享语句不能代替。
func TestUpdateOperatorSettingsBindsPlaintextLayersToTheirOwnPhrase(t *testing.T) {
	ctx := context.Background()

	t.Run("the shared statement cannot open the plaintext body layer", func(t *testing.T) {
		service, repo := newPlaintextSettingsService(map[string]string{})
		_, err := service.UpdateErrorDiagnosticOperatorSettings(ctx, ErrorDiagnosticOperatorUpdateInput{
			Enabled:          true,
			PlainBodyEnabled: true,
			Language:         "en",
			Phrase:           ErrorDiagnosticRiskAcknowledgementPhraseEN,
			PlainBodyPhrase:  ErrorDiagnosticRiskAcknowledgementPhraseEN,
			AdminUserID:      7,
		})
		require.ErrorIs(t, err, ErrErrorDiagnosticPlainBodyRiskAcknowledgementInvalid)
		require.Zero(t, repo.writeCount(), "a refused update writes nothing")
	})

	t.Run("the plaintext statement opens only its own layer", func(t *testing.T) {
		service, repo := newPlaintextSettingsService(map[string]string{})
		status, err := service.UpdateErrorDiagnosticOperatorSettings(ctx, ErrorDiagnosticOperatorUpdateInput{
			Enabled:          true,
			PlainBodyEnabled: true,
			Language:         "en",
			Phrase:           ErrorDiagnosticRiskAcknowledgementPhraseEN,
			PlainBodyPhrase:  ErrorDiagnosticPlainBodyRiskAcknowledgementPhraseEN,
			AdminUserID:      7,
		})
		require.NoError(t, err)
		require.True(t, status.PlainBodyRetentionEnabled)
		require.True(t, status.PlainBodyRetentionAllowed)
		require.False(t, status.PlainHeaderValueRetentionEnabled)
		require.False(t, status.PlainHeaderValueRetentionAllowed)

		stored := plaintextStoredSettings(t, repo)
		require.True(t, stored.PlainBodyRetentionEnabled)
		require.False(t, stored.PlainHeaderValueRetentionEnabled)
		require.NotEmpty(t, repo.storedValue(SettingKeyErrorDiagnosticPlainBodyRiskAck))
		require.Empty(t, repo.storedValue(SettingKeyErrorDiagnosticPlainHeaderValuesRiskAck))
	})

	t.Run("plaintext body layer needs no key", func(t *testing.T) {
		service, _ := newPlaintextSettingsService(map[string]string{})
		status, err := service.UpdateErrorDiagnosticOperatorSettings(ctx, ErrorDiagnosticOperatorUpdateInput{
			Enabled:          true,
			PlainBodyEnabled: true,
			Language:         "zh",
			Phrase:           ErrorDiagnosticRiskAcknowledgementPhraseZH,
			PlainBodyPhrase:  ErrorDiagnosticPlainBodyRiskAcknowledgementPhraseZH,
			AdminUserID:      7,
		})
		require.NoError(t, err)
		require.True(t, status.PlainBodyRetentionAllowed)
	})

	t.Run("disabling turns every plaintext layer off", func(t *testing.T) {
		values := map[string]string{
			SettingKeyErrorDiagnostic:                    `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":true,"header_values_enabled":true,"plain_body_enabled":true,"plain_header_values_enabled":true}`,
			SettingKeyErrorDiagnosticPlainBodyRiskAck:    mustPlainBodyAckJSON(t),
			SettingKeyErrorDiagnosticRiskAcknowledgement: mustSharedAckJSON(t),
		}
		service, repo := newPlaintextSettingsService(values)
		status, err := service.UpdateErrorDiagnosticOperatorSettings(ctx, ErrorDiagnosticOperatorUpdateInput{Enabled: false})
		require.NoError(t, err)
		require.False(t, status.PlainBodyRetentionEnabled)
		require.False(t, status.PlainHeaderValueRetentionEnabled)
		stored := plaintextStoredSettings(t, repo)
		require.False(t, stored.PlainBodyRetentionEnabled)
		require.False(t, stored.PlainHeaderValueRetentionEnabled)
	})
}

// TestOperatorStatusReportsStoredFlagWithStalePlaintextAck 覆盖「存量开着但不生效」：
// 界面必须能看出两层各自为什么没生效，而不是一个无法解释的开启。
func TestOperatorStatusReportsStoredFlagWithStalePlaintextAck(t *testing.T) {
	ctx := context.Background()
	values := map[string]string{
		SettingKeyErrorDiagnostic:                    `{"enabled":true,"risk_acknowledged":true,"plain_body_enabled":true,"plain_header_values_enabled":true}`,
		SettingKeyErrorDiagnosticRiskAcknowledgement: mustSharedAckJSON(t),
	}
	service, _ := newPlaintextSettingsService(values)
	status, err := service.GetErrorDiagnosticOperatorStatus(ctx)
	require.NoError(t, err)
	require.True(t, status.PlainBodyRetentionEnabled, "stored intent is reported verbatim")
	require.False(t, status.PlainBodyRetentionAllowed)
	require.True(t, status.PlainHeaderValueRetentionEnabled)
	require.False(t, status.PlainHeaderValueRetentionAllowed)
	require.NotEmpty(t, status.PlainBodyRiskVersion)
	require.NotEmpty(t, status.PlainBodyRiskPhraseEN)
	require.NotEmpty(t, status.PlainBodyRiskPhraseZH)
	require.NotEmpty(t, status.PlainHeaderRiskVersion)
}

// TestDecideErrorDiagnosticPlainBody 覆盖新明文正文层的判定顺序。
//
// 与旧密文层同形，但**不看任何密钥**：新格式本来就不加密。
func TestDecideErrorDiagnosticPlainBody(t *testing.T) {
	validJSON := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
	large := make([]byte, ErrorDiagnosticMaxBodyBytes+1)
	for i := range large {
		large[i] = 'a'
	}

	cases := []struct {
		name           string
		attempt        ErrorDiagnosticAttempt
		captureAllowed bool
		state          string
		reason         string
		retained       bool
	}{
		{
			name:           "eligible body is retained as plaintext",
			attempt:        ErrorDiagnosticAttempt{Body: validJSON, BodyReadComplete: true, BodyVerdict: ErrorDiagnosticBodyVerdictComplete},
			captureAllowed: true,
			state:          ErrorDiagnosticBodyStateStored,
			reason:         ErrorDiagnosticPlainBodyRetained,
			retained:       true,
		},
		{
			name:           "not requested is not observed",
			attempt:        ErrorDiagnosticAttempt{BodyVerdict: ErrorDiagnosticBodyVerdictNotRequested},
			captureAllowed: true,
			state:          ErrorDiagnosticBodyStateNotObserved,
			reason:         ErrorDiagnosticBodyNotObserved,
		},
		{
			name:           "transport withheld an oversized body",
			attempt:        ErrorDiagnosticAttempt{BodyVerdict: ErrorDiagnosticBodyVerdictTooLarge},
			captureAllowed: true,
			state:          ErrorDiagnosticBodyStateSkipped,
			reason:         ErrorDiagnosticBodySkippedTooLarge,
		},
		{
			name:           "oversized bytes are never truncated into a claim of completeness",
			attempt:        ErrorDiagnosticAttempt{Body: large, BodyReadComplete: true},
			captureAllowed: true,
			state:          ErrorDiagnosticBodyStateSkipped,
			reason:         ErrorDiagnosticBodySkippedTooLarge,
		},
		{
			name:           "incomplete read is skipped",
			attempt:        ErrorDiagnosticAttempt{Body: validJSON, BodyReadComplete: false},
			captureAllowed: true,
			state:          ErrorDiagnosticBodyStateSkipped,
			reason:         ErrorDiagnosticBodySkippedIncompleteRead,
		},
		{
			name:           "non text json is skipped",
			attempt:        ErrorDiagnosticAttempt{Body: []byte("not json"), BodyReadComplete: true},
			captureAllowed: true,
			state:          ErrorDiagnosticBodyStateSkipped,
			reason:         ErrorDiagnosticBodySkippedNotTextJSON,
		},
		{
			name:           "known structured credential refuses the whole body",
			attempt:        ErrorDiagnosticAttempt{Body: []byte(`{"fallback_credit_token":"x"}`), BodyReadComplete: true},
			captureAllowed: true,
			state:          ErrorDiagnosticBodyStateSkipped,
			reason:         ErrorDiagnosticBodySkippedKnownCredential,
		},
		{
			name:           "attachment refuses the whole body",
			attempt:        ErrorDiagnosticAttempt{Body: []byte(`{"messages":[{"content":[{"type":"image","source":{"data":"aGk="}}]}]}`), BodyReadComplete: true},
			captureAllowed: true,
			state:          ErrorDiagnosticBodyStateSkipped,
			reason:         ErrorDiagnosticBodySkippedAttachment,
		},
		{
			name:           "gate closed skips with the plaintext-layer reason",
			attempt:        ErrorDiagnosticAttempt{Body: validJSON, BodyReadComplete: true},
			captureAllowed: false,
			state:          ErrorDiagnosticBodyStateSkipped,
			reason:         ErrorDiagnosticBodySkippedRetentionDisabled,
		},
		{
			name:           "cipher suppression verdict does not apply to the plaintext layer",
			attempt:        ErrorDiagnosticAttempt{BodyVerdict: ErrorDiagnosticBodyVerdictSuppressedEncryptionUnavailable},
			captureAllowed: true,
			state:          ErrorDiagnosticBodyStateNotObserved,
			reason:         ErrorDiagnosticBodyNotObserved,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := DecideErrorDiagnosticPlainBody(tc.attempt, tc.captureAllowed)
			require.Equal(t, tc.state, decision.State)
			require.Equal(t, tc.reason, decision.Reason)
			require.Equal(t, tc.retained, decision.Retained())
			if tc.retained {
				require.Equal(t, tc.attempt.Body, decision.Payload)
			} else {
				require.Empty(t, decision.Payload)
			}
		})
	}
}

// TestPlainBodyReadabilityFollowsUsageNotTheCiphertextWindow 覆盖新明文的到期规则：
// 已关联 usage 的行随 usage 存在；未关联的行在 metadata_expires_at 整点立即不可读。
func TestPlainBodyReadabilityFollowsUsageNotTheCiphertextWindow(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Second)
	future := now.Add(time.Hour)

	base := ErrorDiagnosticRecord{
		PlainRecord:       true,
		PlainBodyState:    ErrorDiagnosticBodyStateStored,
		PlainBodyReason:   ErrorDiagnosticPlainBodyRetained,
		PlainBodyStored:   true,
		MetadataExpiresAt: future,
	}
	require.True(t, base.PlainBodyReadableAt(now))

	unlinkedExpired := base
	unlinkedExpired.MetadataExpiresAt = expired
	require.False(t, unlinkedExpired.PlainBodyReadableAt(now), "unlinked rows stop exactly at the 30 day boundary")

	// 到期时刻本身就不算可读（不是「之后」），与旧元数据窗口同一约定。
	atBoundary := base
	atBoundary.MetadataExpiresAt = now
	require.False(t, atBoundary.PlainBodyReadableAt(now))

	linked := unlinkedExpired
	linked.PlainLinked = true
	require.True(t, linked.PlainBodyReadableAt(now), "linked rows follow their usage, not the 30 day cutoff")

	notStored := linked
	notStored.PlainBodyStored = false
	require.False(t, notStored.PlainBodyReadableAt(now))

	// 旧密文列不参与新明文层的判定：明文行没有密文也可以读。
	require.False(t, base.BodyReadableAt(now))
}

// TestDescribePlainBodyStateDistinguishesPurgedFromNeverRetained 覆盖「曾留存、现已清除」
// 与「从未留存」的可区分性（清理只置空载荷，保留原因码）。
func TestDescribePlainBodyStateDistinguishesPurgedFromNeverRetained(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	require.Equal(t, ErrorDiagnosticBodyStatePurged,
		DescribePlainBodyState(ErrorDiagnosticPlainBodyRetained, false, false, now.Add(-time.Hour), now))
	require.Equal(t, ErrorDiagnosticBodyStateNotObserved,
		DescribePlainBodyState(ErrorDiagnosticBodyNotObserved, false, false, now.Add(-time.Hour), now))
	require.Equal(t, ErrorDiagnosticBodyStateSkipped,
		DescribePlainBodyState(ErrorDiagnosticBodySkippedTooLarge, false, false, now.Add(-time.Hour), now))
	require.Equal(t, ErrorDiagnosticBodyStateExpired,
		DescribePlainBodyState(ErrorDiagnosticPlainBodyRetained, true, false, now.Add(-time.Hour), now))
	require.Equal(t, ErrorDiagnosticBodyStateStored,
		DescribePlainBodyState(ErrorDiagnosticPlainBodyRetained, true, false, now.Add(time.Hour), now))
	require.Equal(t, ErrorDiagnosticBodyStateStored,
		DescribePlainBodyState(ErrorDiagnosticPlainBodyRetained, true, true, now.Add(-time.Hour), now),
		"a linked row has no fixed window of its own")
}

// TestRecordErrorDiagnosticWritesThePlaintextFormat 覆盖写入口：开启新明文正文后，
// 行以新格式落库（明文列 + 可验证的关联摘要），旧密文列一个字都不写。
func TestRecordErrorDiagnosticWritesThePlaintextFormat(t *testing.T) {
	payload := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
	repo := newErrorDiagnosticRepoFake()
	cipher := &errorDiagnosticCipherFake{version: 3}
	settings := errorDiagnosticSettingsFixed{settings: ErrorDiagnosticSettings{
		Enabled:                   true,
		RiskAcknowledged:          true,
		PlainBodyRetentionEnabled: true,
	}}
	diagnostics := NewErrorDiagnosticService(repo, settings, cipher)
	fingerprinter, err := NewRequestAuditFingerprinter(strings.Repeat("f", 32))
	require.NoError(t, err)
	fingerprint, err := fingerprinter.BeginForUser(7)
	require.NoError(t, err)
	ctx := WithRequestAuditFingerprint(context.WithValue(context.Background(), ctxkey.RequestID, "local-abc"), fingerprint)

	_, err = diagnostics.RecordErrorDiagnostic(ctx, ErrorDiagnosticAttempt{
		Protocol:           ErrorDiagnosticProtocolMessages,
		AttemptIndex:       2,
		Stage:              ErrorDiagnosticStageWire,
		UpstreamStatusCode: 500,
		Body:               payload,
		BodyReadComplete:   true,
		BodyVerdict:        ErrorDiagnosticBodyVerdictComplete,
	})
	require.NoError(t, err)
	require.Len(t, repo.created, 1)
	write := repo.created[0]
	require.True(t, write.PlainRecord)
	require.Equal(t, ErrorDiagnosticBodyStateStored, write.PlainBodyState)
	require.Equal(t, ErrorDiagnosticPlainBodyRetained, write.PlainBodyReason)
	require.Equal(t, payload, write.PlainBodyPayload)
	require.Empty(t, write.BodyCiphertext, "the plaintext layer never touches the ciphertext column")
	require.Empty(t, cipher.encrypted, "no plaintext is handed to a cipher")
	require.Equal(t, fingerprint.DigestIdentifier("local_request", "local-abc"), write.PlainLinkDigest)
	require.Equal(t, 2, write.PlainLinkAttemptIndex)
	require.Equal(t, 500, write.PlainLinkWireStatus)

	// 计数只累加事件，不含任何载荷字节：明文层的观测不能变成第二条正文旁路。
	counters := diagnostics.Counters()
	require.EqualValues(t, 1, counters.PlainBodyStored)
	require.EqualValues(t, 0, counters.BodyStored, "明文留存不得被算进密文层的计数")
	require.NotContains(t, fmt.Sprintf("%+v", counters), "claude")
}

func TestRecordErrorDiagnosticPreservesLegacyHeadersWhenOnlyPlainBodyIsEnabled(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	cipher := &errorDiagnosticCipherFake{version: 3}
	settings := errorDiagnosticSettingsFixed{settings: ErrorDiagnosticSettings{
		Enabled: true, RiskAcknowledged: true,
		HeaderValueRetentionEnabled: true, PlainBodyRetentionEnabled: true,
	}}
	diagnostics := NewErrorDiagnosticService(repo, settings, cipher)
	_, err := diagnostics.RecordErrorDiagnostic(context.Background(), ErrorDiagnosticAttempt{
		Protocol: ErrorDiagnosticProtocolMessages, Stage: ErrorDiagnosticStageWire,
		AttemptIndex: 1, UpstreamStatusCode: 429,
		HeaderValues: ErrorDiagnosticHeaderValues{Response: map[string]string{"Retry-After": "30"}},
	})
	require.NoError(t, err)
	require.Len(t, repo.created, 1)
	require.Equal(t, ErrorDiagnosticHeaderStateStored, repo.created[0].HeaderState)
	require.NotEmpty(t, repo.created[0].HeaderCiphertext)
}

func TestRecordErrorDiagnosticPreservesLegacyBodyWhenOnlyPlainHeadersAreEnabled(t *testing.T) {
	payload := []byte(`{"model":"claude"}`)
	repo := newErrorDiagnosticRepoFake()
	cipher := &errorDiagnosticCipherFake{version: 3}
	settings := errorDiagnosticSettingsFixed{settings: ErrorDiagnosticSettings{
		Enabled: true, RiskAcknowledged: true,
		BodyRetentionEnabled: true, PlainHeaderValueRetentionEnabled: true,
	}}
	service := NewErrorDiagnosticService(repo, settings, cipher)
	_, err := service.RecordErrorDiagnostic(context.Background(), ErrorDiagnosticAttempt{
		Protocol: ErrorDiagnosticProtocolMessages, Stage: ErrorDiagnosticStageWire,
		AttemptIndex: 1, UpstreamStatusCode: 500, Body: payload,
		BodyReadComplete: true, BodyVerdict: ErrorDiagnosticBodyVerdictComplete,
	})
	require.NoError(t, err)
	require.Len(t, repo.created, 1)
	require.Equal(t, ErrorDiagnosticBodyStateStored, repo.created[0].BodyState)
	require.NotEmpty(t, repo.created[0].BodyCiphertext)
}

// TestRecordErrorDiagnosticWithoutAuditableCorrelationStaysUnlinkable 覆盖「不能猜关联」：
// 没有可验证的关联摘要时明文行照写，但绝不用时间、账号或客户端可控 ID 兜底猜测。
func TestRecordErrorDiagnosticWithoutAuditableCorrelationStaysUnlinkable(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	settings := errorDiagnosticSettingsFixed{settings: ErrorDiagnosticSettings{
		Enabled:                   true,
		RiskAcknowledged:          true,
		PlainBodyRetentionEnabled: true,
	}}
	diagnostics := NewErrorDiagnosticService(repo, settings, nil)
	// 只有 Request-ID、没有审计关联摘要：无法验证「同一次逻辑请求」，因此不可关联。
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "local-abc")
	_, err := diagnostics.RecordErrorDiagnostic(ctx, ErrorDiagnosticAttempt{
		Protocol:           ErrorDiagnosticProtocolMessages,
		AttemptIndex:       1,
		Stage:              ErrorDiagnosticStageWire,
		UpstreamStatusCode: 503,
		Body:               []byte(`{"a":1}`),
		BodyReadComplete:   true,
	})
	require.NoError(t, err)
	require.Len(t, repo.created, 1)
	require.True(t, repo.created[0].PlainRecord)
	require.Empty(t, repo.created[0].PlainLinkDigest)
}

// TestRecordErrorDiagnosticKeepsTheLegacyFormatWhenPlaintextIsOff 是回归：新层默认关闭时，
// 密文层的行为与原因码一字未改。
func TestRecordErrorDiagnosticKeepsTheLegacyFormatWhenPlaintextIsOff(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	cipher := &errorDiagnosticCipherFake{version: 3}
	settings := errorDiagnosticSettingsFixed{settings: ErrorDiagnosticSettings{
		Enabled:              true,
		RiskAcknowledged:     true,
		BodyRetentionEnabled: true,
	}}
	diagnostics := NewErrorDiagnosticService(repo, settings, cipher)
	body := []byte(`{"model":"claude"}`)
	record, err := diagnostics.RecordErrorDiagnostic(context.Background(), ErrorDiagnosticAttempt{
		Protocol:           ErrorDiagnosticProtocolMessages,
		AttemptIndex:       1,
		Stage:              ErrorDiagnosticStageWire,
		UpstreamStatusCode: 429,
		Body:               body,
		BodyReadComplete:   true,
	})
	require.NoError(t, err)
	require.False(t, repo.created[0].PlainRecord)
	require.Equal(t, ErrorDiagnosticBodyRetained, repo.created[0].BodyReason)
	require.NotEmpty(t, repo.created[0].BodyCiphertext)
	require.Empty(t, repo.created[0].PlainBodyPayload)
	require.Equal(t, ErrorDiagnosticBodyStateStored, record.BodyState)
}

// plaintextDiagnosticRepoFake 在既有内存替身上补出新明文层的读取与清理能力。
//
// 用嵌入而不是改既有替身：旧格式的替身不该被迫长出明文层的方法，而且「存储层没有这个能力」
// 本身就是要被覆盖的一种事实（服务侧必须报明确的不可用，不能报「正文已消失」）。
type plaintextDiagnosticRepoFake struct {
	*errorDiagnosticRepoFake
	plainBody          map[string][]byte
	plainHeaders       map[string]ErrorDiagnosticHeaderValues
	plainCleared       int64
	plainHeaderCleared int64
}

func newPlaintextDiagnosticRepoFake() *plaintextDiagnosticRepoFake {
	return &plaintextDiagnosticRepoFake{
		errorDiagnosticRepoFake: newErrorDiagnosticRepoFake(),
		plainBody:               map[string][]byte{},
		plainHeaders:            map[string]ErrorDiagnosticHeaderValues{},
	}
}

func (f *plaintextDiagnosticRepoFake) ReadErrorDiagnosticPlainBody(_ context.Context, id string, now time.Time) ([]byte, error) {
	record, ok := f.records[id]
	if !ok || !record.PlainBodyReadableAt(now) {
		return nil, ErrErrorDiagnosticBodyGone
	}
	body, ok := f.plainBody[id]
	if !ok {
		return nil, ErrErrorDiagnosticBodyGone
	}
	return append([]byte(nil), body...), nil
}

func (f *plaintextDiagnosticRepoFake) ReadErrorDiagnosticPlainHeaderValues(_ context.Context, id string, now time.Time) (ErrorDiagnosticHeaderValues, error) {
	record, ok := f.records[id]
	if !ok || !record.PlainHeaderValuesReadableAt(now) {
		return ErrorDiagnosticHeaderValues{}, ErrErrorDiagnosticHeaderValuesGone
	}
	values, ok := f.plainHeaders[id]
	if !ok {
		return ErrorDiagnosticHeaderValues{}, ErrErrorDiagnosticHeaderValuesGone
	}
	return values, nil
}

func (f *plaintextDiagnosticRepoFake) ClearExpiredErrorDiagnosticPlainBodies(context.Context, time.Time, int) (int64, error) {
	return f.plainCleared, nil
}

func (f *plaintextDiagnosticRepoFake) ClearExpiredErrorDiagnosticPlainHeaderValues(context.Context, time.Time, int) (int64, error) {
	return f.plainHeaderCleared, nil
}

// TestPlaintextBodyIsServedWithoutAnyCipher 覆盖读路径：新明文行不需要、也不使用密钥。
func TestPlaintextBodyIsServedWithoutAnyCipher(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	id, err := NewErrorDiagnosticID()
	require.NoError(t, err)
	repo := newPlaintextDiagnosticRepoFake()
	repo.records[id] = ErrorDiagnosticRecord{
		ID:                id,
		Protocol:          ErrorDiagnosticProtocolMessages,
		PlainRecord:       true,
		PlainBodyState:    ErrorDiagnosticBodyStateStored,
		PlainBodyReason:   ErrorDiagnosticPlainBodyRetained,
		PlainBodyStored:   true,
		MetadataExpiresAt: now.Add(48 * time.Hour),
	}
	repo.plainBody[id] = []byte(`{"model":"claude"}`)

	// cipher 为 nil：部署没有稳定密钥，新明文层照样可读。
	diagnostics := NewErrorDiagnosticService(repo, errorDiagnosticSettingsFixed{}, nil)
	diagnostics.now = func() time.Time { return now }
	body, err := diagnostics.ReadErrorDiagnosticBody(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, `{"model":"claude"}`, string(body))
	require.EqualValues(t, 1, diagnostics.Counters().PlainBodyReads)
}

// TestPlaintextBodyReadFailsClosedAtTheThirtyDayBoundary 覆盖未关联行的整点拒绝：
// 到期后必须立刻不可读，而不是等清理跑完。
func TestPlaintextBodyReadFailsClosedAtTheThirtyDayBoundary(t *testing.T) {
	boundary := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	id, err := NewErrorDiagnosticID()
	require.NoError(t, err)
	repo := newPlaintextDiagnosticRepoFake()
	repo.records[id] = ErrorDiagnosticRecord{
		ID:                id,
		Protocol:          ErrorDiagnosticProtocolMessages,
		PlainRecord:       true,
		PlainBodyState:    ErrorDiagnosticBodyStateStored,
		PlainBodyReason:   ErrorDiagnosticPlainBodyRetained,
		PlainBodyStored:   true,
		MetadataExpiresAt: boundary,
	}
	repo.plainBody[id] = []byte(`{"model":"claude"}`)

	diagnostics := NewErrorDiagnosticService(repo, errorDiagnosticSettingsFixed{}, nil)
	diagnostics.now = func() time.Time { return boundary }
	_, err = diagnostics.ReadErrorDiagnosticBody(context.Background(), id)
	require.ErrorIs(t, err, ErrErrorDiagnosticBodyGone)

	// 已关联 usage 的行没有这个窗口：同一时刻仍然可读。
	linked := repo.records[id]
	linked.PlainLinked = true
	repo.records[id] = linked
	body, err := diagnostics.ReadErrorDiagnosticBody(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, `{"model":"claude"}`, string(body))
}

// TestPlaintextBodyReadIsUnavailableWhenStorageCannotServeIt 覆盖「存储层没有这个能力」：
// 必须是明确的不可用，而不是显示成「正文从未留存」。
func TestPlaintextBodyReadIsUnavailableWhenStorageCannotServeIt(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	id, err := NewErrorDiagnosticID()
	require.NoError(t, err)
	repo := newErrorDiagnosticRepoFake()
	repo.records[id] = ErrorDiagnosticRecord{
		ID:                id,
		Protocol:          ErrorDiagnosticProtocolMessages,
		PlainRecord:       true,
		PlainBodyState:    ErrorDiagnosticBodyStateStored,
		PlainBodyReason:   ErrorDiagnosticPlainBodyRetained,
		PlainBodyStored:   true,
		MetadataExpiresAt: now.Add(time.Hour),
	}
	diagnostics := NewErrorDiagnosticService(repo, errorDiagnosticSettingsFixed{}, nil)
	diagnostics.now = func() time.Time { return now }
	_, err = diagnostics.ReadErrorDiagnosticBody(context.Background(), id)
	require.ErrorIs(t, err, ErrErrorDiagnosticUnavailable)
}

// TestLegacyEncryptedBodyStillRequiresTheKey 是回归：旧密文行仍然需要旧密钥，
// 缺钥时绝不能因为「有明文路径」而误报成可读。
func TestLegacyEncryptedBodyStillRequiresTheKey(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	id, err := NewErrorDiagnosticID()
	require.NoError(t, err)
	repo := newErrorDiagnosticRepoFake()
	repo.records[id] = ErrorDiagnosticRecord{
		ID:                id,
		Protocol:          ErrorDiagnosticProtocolMessages,
		BodyState:         ErrorDiagnosticBodyStateStored,
		BodyReason:        ErrorDiagnosticBodyRetained,
		BodyStored:        true,
		BodyExpiresAt:     now.Add(24 * time.Hour),
		MetadataExpiresAt: now.Add(48 * time.Hour),
	}
	repo.body = map[string][]byte{id: []byte("enc:{}")}

	withoutKey := NewErrorDiagnosticService(repo, errorDiagnosticSettingsFixed{}, nil)
	withoutKey.now = func() time.Time { return now }
	_, err = withoutKey.ReadErrorDiagnosticBody(context.Background(), id)
	require.ErrorIs(t, err, ErrErrorDiagnosticBodyGone)

	// 替身自己也要有解密能力：真实仓储的密文列由它解密，服务侧的 cipher 决定「允不允许读」。
	cipher := &errorDiagnosticCipherFake{}
	repo.caller = cipher
	withKey := NewErrorDiagnosticService(repo, errorDiagnosticSettingsFixed{}, cipher)
	withKey.now = func() time.Time { return now }
	body, err := withKey.ReadErrorDiagnosticBody(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "{}", string(body))
}
