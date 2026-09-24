//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 票据 02 的验收标准：「…已知结构化凭据、请求读取不完整、**缺密钥**和队列溢出
// 仅有安全元数据及稳定原因码」。
//
// 「缺密钥」因此不是「没采集」，而是「要求过留存、部署此刻做不到」：
//   - 传输层不得为注定被丢弃的字节做 tee（零明文复制）；
//   - 该次尝试必须留下稳定原因码 skipped_encryption_unavailable，
//     而不是 not_observed（后者的语义是「本次没有 opt-in 正文采集」）。
//
// 这些用例锁定这条接缝契约本身：封闭抑制结论的判定、映射与写入结果。
// 端到端（真实 Messages 观察者 + 真实 Record 服务 + 真实门控读取链路）见
// error_diagnostic_messages_test.go 的
// TestMessagesErrorDiagnosticReportsClosedSuppressionReasonWithoutStableKey。

// TestErrorDiagnosticSettings_MissingKeySuppressionIsExplicit 覆盖封闭抑制的判据：
// 它必须同时要求「要求过留存」与「没有可用稳定密钥」，且不得改变任何有效门控结论。
func TestErrorDiagnosticSettings_MissingKeySuppressionIsExplicit(t *testing.T) {
	cases := []struct {
		name         string
		settings     ErrorDiagnosticSettings
		keyAvailable bool
		want         bool
	}{
		{
			name:         "no retention requested and no key is a normal off state",
			settings:     ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true},
			keyAvailable: false,
			want:         false,
		},
		{
			name:         "retention requested with a key available is not a suppression",
			settings:     ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true},
			keyAvailable: true,
			want:         false,
		},
		{
			// 读取器已收窄（真实 SettingService 的行为）：有效值为 false，意图留在 Requested。
			name: "narrowed effective value still carries the stored intent",
			settings: ErrorDiagnosticSettings{
				Enabled: true, RiskAcknowledged: true,
				BodyRetentionEnabled:   false,
				BodyRetentionRequested: true,
			},
			keyAvailable: false,
			want:         true,
		},
		{
			// 读取器没有收窄（例如只回存量值的替身）：布尔值本身就是意图。
			name:         "unnarrowed stored value is itself the intent",
			settings:     ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true},
			keyAvailable: false,
			want:         true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.settings.BodyRetentionSuppressedByMissingKey(tc.keyAvailable))

			// 只读提示字段绝不参与门控：采集与正文留存的结论一字未变。
			require.Equal(t, tc.settings.Enabled && tc.settings.RiskAcknowledged, tc.settings.CaptureAllowed())
			require.Equal(t,
				tc.settings.Enabled && tc.settings.RiskAcknowledged && tc.settings.BodyRetentionEnabled,
				tc.settings.BodyCaptureAllowed())
		})
	}

	// 只带意图（有效值已被收窄）的配置不得因此被判为可留存：意图不是许可。
	narrowed := ErrorDiagnosticSettings{
		Enabled: true, RiskAcknowledged: true, BodyRetentionRequested: true,
	}
	require.False(t, narrowed.BodyCaptureAllowed(), "缺密钥时仍不得留存正文")
	require.False(t, narrowed.BodyRetentionSuppressedByMissingKey(true),
		"有密钥时不存在抑制，即使存量意图字段仍为真")
}

// TestDecideErrorDiagnosticBody_ClosedSuppressionNeverRetains 覆盖封闭抑制结论的映射：
// 它映射到既有的稳定原因码，且**永远不会**导致留存——即使体量、完整性与内容分类全部合格，
// 即使策略与加密器都可用。
func TestDecideErrorDiagnosticBody_ClosedSuppressionNeverRetains(t *testing.T) {
	eligibleBody := []byte(`{"model":"claude","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	require.True(t, ClassifyErrorDiagnosticBody(eligibleBody).IsTextJSON,
		"夹具必须是合格正文，否则「没有留存」会由别的原因解释")

	cases := []struct {
		name    string
		attempt ErrorDiagnosticAttempt
	}{
		{
			name:    "suppression with withheld bytes",
			attempt: ErrorDiagnosticAttempt{BodyVerdict: ErrorDiagnosticBodyVerdictSuppressedEncryptionUnavailable},
		},
		{
			// 封闭结论：即使被夹带字节也不会被保存（它不是留存通道）。
			name: "suppression that carries bytes anyway",
			attempt: ErrorDiagnosticAttempt{
				BodyVerdict: ErrorDiagnosticBodyVerdictSuppressedEncryptionUnavailable,
				Body:        eligibleBody, BodyReadComplete: true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 策略与加密器都「可用」：结论仍必须是跳过，绝不能因为门控允许就留存。
			state, reason, retain := DecideErrorDiagnosticBody(tc.attempt, true, true)
			require.Equal(t, ErrorDiagnosticBodyStateSkipped, state)
			require.Equal(t, ErrorDiagnosticBodySkippedEncryptionUnavailable, reason)
			require.False(t, retain)

			// 该结论是接缝的合法输入，并在对外状态里映射成 skipped（不是 not_observed）。
			require.True(t, ErrorDiagnosticBodyVerdictAllowed(tc.attempt.BodyVerdict))
			now := time.Now()
			require.Equal(t, ErrorDiagnosticBodyStateSkipped,
				DescribeBodyState(reason, false, time.Time{}, now))
			require.NotEqual(t, ErrorDiagnosticBodyStateNotObserved,
				DescribeBodyState(reason, false, time.Time{}, now),
				"缺密钥不得被显示成「本次没有 opt-in 正文采集」")
		})
	}
}

// TestErrorDiagnostic_ClosedSuppressionVerdictRecordsTheStableReason 覆盖真实 Record 服务的
// 写入结果：封闭抑制结论留下安全元数据与稳定原因码，且没有任何字节进入加密路径。
func TestErrorDiagnostic_ClosedSuppressionVerdictRecordsTheStableReason(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	cipher := &errorDiagnosticCipherFake{version: 1}
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), cipher)

	attempt := messagesAttempt()
	attempt.BodyVerdict = ErrorDiagnosticBodyVerdictSuppressedEncryptionUnavailable

	record, err := svc.RecordErrorDiagnostic(context.Background(), attempt)
	require.NoError(t, err, "缺密钥只影响正文，元数据仍要落库")
	require.Equal(t, ErrorDiagnosticBodyStateSkipped, record.BodyState)
	require.Equal(t, ErrorDiagnosticBodySkippedEncryptionUnavailable, record.BodyReason)
	require.Len(t, repo.created, 1)
	require.Empty(t, repo.created[0].BodyCiphertext, "缺密钥不得留下任何密文")
	require.Empty(t, cipher.encrypted, "缺密钥不得让任何明文进入加密路径")
	require.EqualValues(t, 1, svc.Counters().BodySkipped)
	require.Zero(t, svc.Counters().BodyStored)

	// 对照：正常关闭（没有 opt-in 正文采集）仍然只能是 not_observed——
	// 两种状态必须保持可区分，否则封闭抑制就没有意义。
	offAttempt := messagesAttempt()
	offAttempt.BodyVerdict = ErrorDiagnosticBodyVerdictNotRequested
	offRecord, err := svc.RecordErrorDiagnostic(context.Background(), offAttempt)
	require.NoError(t, err)
	require.Equal(t, ErrorDiagnosticBodyStateNotObserved, offRecord.BodyState)
	require.Equal(t, ErrorDiagnosticBodyNotObserved, offRecord.BodyReason)
}
