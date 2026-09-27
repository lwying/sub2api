//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// 票据 10 / ADR 0007：三类新明文采集（值明细、独立诊断正文、独立诊断 429 头值）都有同一个
// 部署前提——数据库必须能保证「明文随所属 usage 消失」。本文件锁定这条前提的三条消费路径：
//
//	采集侧：前提不成立时三类**新**明文一律关闭，旧格式（元数据、旧密文、旧记录读取）一字不改；
//	开启侧：前提不成立时拒绝开启，且**关闭永远放行**（紧急关闭不能被探针挡住）；
//	运维侧：把原因码如实回显，不泄露数据库错误原文。
//
// 探针在本文件里是合成替身：它只回答结论，不碰真实数据库（真实目录查询由仓储层的集成测试
// 用真实 schema 验证）。

// plaintextSupportProbeStub 是 PlaintextCaptureSupportProbe 的合成替身。
type plaintextSupportProbeStub struct {
	support PlaintextCaptureSupport
	err     error
	calls   int
}

func (p *plaintextSupportProbeStub) ProbePlaintextCaptureSupport(context.Context) (PlaintextCaptureSupport, error) {
	p.calls++
	return p.support, p.err
}

// supportedPlaintextCaptureProbe 造一个「部署前提成立」的探针替身。
//
// 只有明确要验证部署前提的用例才该注入别的结论；验证其它门槛（逐字确认、密钥、期限）的
// 用例必须显式满足部署前提，否则两类原因会混在一起，用例就不再是在测它自己那件事。
func supportedPlaintextCaptureProbe() PlaintextCaptureSupportProbe {
	return &plaintextSupportProbeStub{support: PlaintextCaptureSupport{Supported: true}}
}

// supportReasonFromError 取出开启被拒时随错误回显的部署原因码。
func supportReasonFromError(t *testing.T, err error) string {
	t.Helper()
	var appErr *infraerrors.ApplicationError
	require.ErrorAs(t, err, &appErr)
	return appErr.Metadata["plaintext_capture_support"]
}

// plaintextSupportFixtures 造一份「全部存量开关都开着、书面确认都是当前版本」的设置：
// 只有这样，前提不成立时出现的 false 才只可能来自部署前提，而不是别的门槛。
func plaintextSupportFixtures(t *testing.T) map[string]string {
	t.Helper()
	valueDetailSettings, err := json.Marshal(RequestAuditValueDetailSettings{Enabled: true, RiskAcknowledged: true})
	require.NoError(t, err)
	valueDetailAck, err := json.Marshal(RequestAuditValueDetailRiskAcknowledgement{
		Version:     RequestAuditValueDetailRiskAcknowledgementVersion,
		Phrase:      RequestAuditValueDetailRiskAcknowledgementPhraseEN,
		AdminUserID: 7,
		AcceptedAt:  time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	diagnosticSettings, err := json.Marshal(ErrorDiagnosticSettings{
		Enabled:                          true,
		RiskAcknowledged:                 true,
		BodyRetentionEnabled:             true,
		HeaderValueRetentionEnabled:      true,
		PlainBodyRetentionEnabled:        true,
		PlainHeaderValueRetentionEnabled: true,
	})
	require.NoError(t, err)
	return map[string]string{
		SettingKeyRequestAuditValueDetail:                    string(valueDetailSettings),
		SettingKeyRequestAuditValueDetailRiskAcknowledgement: string(valueDetailAck),
		SettingKeyErrorDiagnostic:                            string(diagnosticSettings),
		SettingKeyErrorDiagnosticRiskAcknowledgement:         errorDiagnosticAckJSON(ErrorDiagnosticRiskAcknowledgementVersion),
		SettingKeyErrorDiagnosticPlainBodyRiskAck: errorDiagnosticAckJSONWithPhrase(
			ErrorDiagnosticPlainBodyRiskAcknowledgementVersion, ErrorDiagnosticPlainBodyRiskAcknowledgementPhraseEN),
		SettingKeyErrorDiagnosticPlainHeaderValuesRiskAck: errorDiagnosticAckJSONWithPhrase(
			ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementVersion, ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementPhraseEN),
	}
}

func plaintextSupportService(t *testing.T, values map[string]string, probe PlaintextCaptureSupportProbe) (*SettingService, *valueDetailSettingRepoStub) {
	t.Helper()
	repo := &valueDetailSettingRepoStub{values: values}
	svc := NewSettingService(repo, valueDetailConfigWithStableKey(t))
	svc.SetPlaintextCaptureSupportProbe(probe)
	return svc, repo
}

// TestPlaintextCaptureSupportClosesOnlyNewPlaintextModes 是本次变更的核心用例：
// 无论部署前提以哪种方式不成立（分区、所有权外键缺失、形态未知、探针查不出来、探针没接），
// 三类**新明文**都必须关闭，而旧格式必须一字不动。
func TestPlaintextCaptureSupportClosesOnlyNewPlaintextModes(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		probe PlaintextCaptureSupportProbe
		want  string
	}{
		{
			name:  "supported",
			probe: &plaintextSupportProbeStub{support: PlaintextCaptureSupport{Supported: true}},
			want:  PlaintextCaptureSupportReasonSupported,
		},
		{
			name:  "partitioned usage_logs",
			probe: &plaintextSupportProbeStub{support: PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonPartitionedUsageLogs}},
			want:  PlaintextCaptureSupportReasonPartitionedUsageLogs,
		},
		{
			name:  "missing ownership foreign key",
			probe: &plaintextSupportProbeStub{support: PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonMissingOwnership}},
			want:  PlaintextCaptureSupportReasonMissingOwnership,
		},
		{
			name:  "unknown deployment shape",
			probe: &plaintextSupportProbeStub{support: PlaintextCaptureSupport{Reason: "something_new"}},
			want:  PlaintextCaptureSupportReasonUnknownDeployment,
		},
		{
			name:  "probe error",
			probe: &plaintextSupportProbeStub{err: errors.New(`pq: password authentication failed for user "sub2api"`)},
			want:  PlaintextCaptureSupportReasonProbeFailed,
		},
		{
			name:  "probe not wired",
			probe: nil,
			want:  PlaintextCaptureSupportReasonProbeUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := plaintextSupportService(t, plaintextSupportFixtures(t), tc.probe)
			wantNew := tc.want == PlaintextCaptureSupportReasonSupported

			support := svc.PlaintextCaptureSupport(ctx)
			require.Equal(t, wantNew, support.Supported)
			require.Equal(t, tc.want, support.Reason)

			// 一、值明细：新值明细一律明文，因此这条门控必须连同部署前提一起判定。
			gate := svc.RequestAuditValueDetailGate(ctx)
			require.Equal(t, wantNew, gate.CaptureAllowed,
				"部署前提不成立时不得开启值明细采集（旧密文读取不经过这条门控）")
			require.True(t, gate.EncryptionAvailable, "旧密钥可用性与部署前提无关，仍须如实报告")

			// 二、独立诊断：两个新明文层关闭，三层旧门槛一字不动。
			diag, err := svc.GetErrorDiagnosticSettings(ctx)
			require.NoError(t, err)
			require.Equal(t, wantNew, diag.PlainBodyCaptureAllowed(), "新明文正文层必须随部署前提关闭")
			require.Equal(t, wantNew, diag.PlainHeaderValuesCaptureAllowed(), "新明文 429 头值层必须随部署前提关闭")
			require.Equal(t, wantNew, diag.PlaintextCaptureAllowed())
			require.True(t, diag.CaptureAllowed(), "诊断元数据采集与部署形态无关，必须保持开启")
			require.True(t, diag.BodyCaptureAllowed(), "旧密文正文留存与部署形态无关，必须保持开启")
			require.True(t, diag.HeaderValuesCaptureAllowed(), "旧密文头值留存与部署形态无关，必须保持开启")

			if !wantNew {
				// 收窄只发生在有效值上：存量意图必须原样保留，界面才能回答
				// 「运维要求过明文吗」（是）与「此刻允许采集吗」（否）两个不同的问题。
				stored, err := svc.ReadStoredErrorDiagnosticSettings(ctx)
				require.NoError(t, err)
				require.True(t, stored.PlainBodyRetentionEnabled)
				require.True(t, stored.PlainHeaderValueRetentionEnabled)
			}
		})
	}
}

// TestPlaintextCaptureSupportEnableRejectedButDisableAlwaysAllowed 锁定开启/关闭的不对称：
// 前提不成立时开启被拒（且不写任何键），关闭永远放行且不必查探针。
func TestPlaintextCaptureSupportEnableRejectedButDisableAlwaysAllowed(t *testing.T) {
	ctx := context.Background()

	t.Run("value detail enable rejected with reason in metadata", func(t *testing.T) {
		probe := &plaintextSupportProbeStub{support: PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonPartitionedUsageLogs}}
		svc, repo := plaintextSupportService(t, plaintextSupportFixtures(t), probe)
		before := repo.values[SettingKeyRequestAuditValueDetail]

		_, err := svc.UpdateRequestAuditValueDetailOperatorSettings(ctx, RequestAuditValueDetailOperatorUpdateInput{
			Enabled:     true,
			Language:    "en",
			Phrase:      RequestAuditValueDetailRiskAcknowledgementPhraseEN,
			AdminUserID: 7,
		})
		require.ErrorIs(t, err, ErrRequestAuditValueDetailDeploymentUnsupported)
		require.Equal(t, PlaintextCaptureSupportReasonPartitionedUsageLogs, supportReasonFromError(t, err),
			"拒绝必须带上闭集原因码，界面才能说清「为什么开不了」")
		require.Equal(t, before, repo.values[SettingKeyRequestAuditValueDetail], "被拒的更新不得留下任何状态")
	})

	t.Run("plaintext diagnostic enable rejected with reason in metadata", func(t *testing.T) {
		probe := &plaintextSupportProbeStub{support: PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonMissingOwnership}}
		svc, repo := plaintextSupportService(t, map[string]string{}, probe)

		_, err := svc.UpdateErrorDiagnosticOperatorSettings(ctx, ErrorDiagnosticOperatorUpdateInput{
			Enabled:                  true,
			Phrase:                   ErrorDiagnosticRiskAcknowledgementPhraseEN,
			PlainHeaderValuesEnabled: true,
			PlainHeaderValuesPhrase:  ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementPhraseEN,
			AdminUserID:              7,
		})
		require.ErrorIs(t, err, ErrErrorDiagnosticPlaintextDeploymentUnsupported)
		require.Equal(t, PlaintextCaptureSupportReasonMissingOwnership, supportReasonFromError(t, err))
		require.Empty(t, repo.values, "被拒的更新不得写入门控键或确认键")
	})

	t.Run("legacy diagnostic layers still enable on an unsupported deployment", func(t *testing.T) {
		// 部署前提只挡新明文：旧密文层的开启与它无关，否则一次手工分区会连旧能力都开不回来。
		probe := &plaintextSupportProbeStub{support: PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonPartitionedUsageLogs}}
		svc, _ := plaintextSupportService(t, map[string]string{}, probe)

		status, err := svc.UpdateErrorDiagnosticOperatorSettings(ctx, ErrorDiagnosticOperatorUpdateInput{
			Enabled:                     true,
			BodyRetentionEnabled:        true,
			HeaderValueRetentionEnabled: true,
			Phrase:                      ErrorDiagnosticRiskAcknowledgementPhraseEN,
			AdminUserID:                 7,
		})
		require.NoError(t, err)
		require.True(t, status.BodyRetentionAllowed, "旧密文正文留存不受部署前提影响")
		require.True(t, status.HeaderValueRetentionAllowed, "旧密文头值留存不受部署前提影响")
		require.False(t, status.PlainBodyRetentionAllowed)
		require.Equal(t, 1, probe.calls,
			"旧层开启本身不需要部署前提；这一次调用来自更新后回显的运维状态")
	})

	t.Run("disable always succeeds without a probe", func(t *testing.T) {
		// 紧急关闭：没有探针、存量全开、部署形态未知，也必须能一键关掉全部四层，且不查探针。
		svc, _ := plaintextSupportService(t, plaintextSupportFixtures(t), nil)

		valueDetailStatus, err := svc.UpdateRequestAuditValueDetailOperatorSettings(ctx, RequestAuditValueDetailOperatorUpdateInput{Enabled: false})
		require.NoError(t, err)
		require.False(t, valueDetailStatus.Enabled)

		diagStatus, err := svc.UpdateErrorDiagnosticOperatorSettings(ctx, ErrorDiagnosticOperatorUpdateInput{Enabled: false})
		require.NoError(t, err)
		require.False(t, diagStatus.Enabled)
		require.False(t, diagStatus.BodyRetentionEnabled)
		require.False(t, diagStatus.HeaderValueRetentionEnabled)
		require.False(t, diagStatus.PlainBodyRetentionEnabled)
		require.False(t, diagStatus.PlainHeaderValueRetentionEnabled)
		require.False(t, svc.RequestAuditValueDetailGate(ctx).CaptureAllowed)
	})

	t.Run("enable succeeds on a supported deployment", func(t *testing.T) {
		probe := &plaintextSupportProbeStub{support: PlaintextCaptureSupport{Supported: true}}
		svc, _ := plaintextSupportService(t, map[string]string{}, probe)

		valueDetailStatus, err := svc.UpdateRequestAuditValueDetailOperatorSettings(ctx, RequestAuditValueDetailOperatorUpdateInput{
			Enabled:     true,
			Language:    "en",
			Phrase:      RequestAuditValueDetailRiskAcknowledgementPhraseEN,
			AdminUserID: 7,
		})
		require.NoError(t, err)
		require.True(t, valueDetailStatus.CaptureAllowed)

		diagStatus, err := svc.UpdateErrorDiagnosticOperatorSettings(ctx, ErrorDiagnosticOperatorUpdateInput{
			Enabled:                  true,
			PlainBodyEnabled:         true,
			PlainBodyPhrase:          ErrorDiagnosticPlainBodyRiskAcknowledgementPhraseEN,
			PlainHeaderValuesEnabled: true,
			PlainHeaderValuesPhrase:  ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementPhraseEN,
			Phrase:                   ErrorDiagnosticRiskAcknowledgementPhraseEN,
			AdminUserID:              7,
		})
		require.NoError(t, err)
		require.True(t, diagStatus.PlainBodyRetentionAllowed)
		require.True(t, diagStatus.PlainHeaderValueRetentionAllowed)
		require.True(t, diagStatus.PlaintextCaptureSupported)
	})
}

// TestPlaintextCaptureSupportStatusDisclosesReasonWithoutDatabaseError 锁定运维状态：
// 必须同时给出结论与原因码，且原因码里绝不出现数据库错误原文。
func TestPlaintextCaptureSupportStatusDisclosesReasonWithoutDatabaseError(t *testing.T) {
	ctx := context.Background()

	t.Run("partitioned deployment is explained", func(t *testing.T) {
		probe := &plaintextSupportProbeStub{support: PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonPartitionedUsageLogs}}
		svc, _ := plaintextSupportService(t, plaintextSupportFixtures(t), probe)

		status, err := svc.GetErrorDiagnosticOperatorStatus(ctx)
		require.NoError(t, err, "探针结论不支持不是状态接口的故障")
		require.True(t, status.Enabled, "存量值如实回显")
		require.True(t, status.CaptureAllowed, "旧门槛不受影响")
		require.False(t, status.PlaintextCaptureSupported)
		require.Equal(t, PlaintextCaptureSupportReasonPartitionedUsageLogs, status.PlaintextCaptureSupportReason)
		require.False(t, status.PlainBodyRetentionAllowed, "允许结论必须与采集侧同源")
		require.False(t, status.PlainHeaderValueRetentionAllowed)
		require.True(t, status.BodyRetentionAllowed)

		valueDetailStatus, err := svc.GetRequestAuditValueDetailOperatorStatus(ctx)
		require.NoError(t, err)
		require.False(t, valueDetailStatus.PlaintextCaptureSupported)
		require.Equal(t, PlaintextCaptureSupportReasonPartitionedUsageLogs, valueDetailStatus.PlaintextCaptureSupportReason)
		require.False(t, valueDetailStatus.CaptureAllowed)
		require.True(t, valueDetailStatus.Enabled, "存量值仍如实回显")
		require.True(t, valueDetailStatus.EncryptionKeyAvailable, "旧密文可读性不受部署前提影响")
	})

	t.Run("probe failure is reported as probe_failed without the driver error", func(t *testing.T) {
		const driverError = `pq: password authentication failed for user "sub2api"`
		probe := &plaintextSupportProbeStub{err: errors.New(driverError)}
		svc, _ := plaintextSupportService(t, plaintextSupportFixtures(t), probe)

		status, err := svc.GetErrorDiagnosticOperatorStatus(ctx)
		require.NoError(t, err)
		require.False(t, status.PlaintextCaptureSupported)
		require.Equal(t, PlaintextCaptureSupportReasonProbeFailed, status.PlaintextCaptureSupportReason)
		require.NotContains(t, status.PlaintextCaptureSupportReason, "password")
		require.NotContains(t, status.PlaintextCaptureSupportReason, "pq:")
		require.False(t, status.PlainBodyRetentionAllowed)
		require.True(t, status.BodyRetentionAllowed, "旧层不受探针故障影响")

		valueDetailStatus, err := svc.GetRequestAuditValueDetailOperatorStatus(ctx)
		require.NoError(t, err)
		require.Equal(t, PlaintextCaptureSupportReasonProbeFailed, valueDetailStatus.PlaintextCaptureSupportReason)
	})

	t.Run("supported deployment reports supported", func(t *testing.T) {
		probe := &plaintextSupportProbeStub{support: PlaintextCaptureSupport{Supported: true}}
		svc, _ := plaintextSupportService(t, plaintextSupportFixtures(t), probe)

		status, err := svc.GetRequestAuditValueDetailOperatorStatus(ctx)
		require.NoError(t, err)
		require.True(t, status.PlaintextCaptureSupported)
		require.Equal(t, PlaintextCaptureSupportReasonSupported, status.PlaintextCaptureSupportReason)
		require.True(t, status.CaptureAllowed)
	})
}

// TestPlaintextCaptureSupportCachesButEnableProbesFresh 锁定缓存的两条边界：
// 采集热路径不重复查目录，开启路径永远问此刻的结论。
func TestPlaintextCaptureSupportCachesButEnableProbesFresh(t *testing.T) {
	ctx := context.Background()
	probe := &plaintextSupportProbeStub{support: PlaintextCaptureSupport{Supported: true}}
	svc, _ := plaintextSupportService(t, map[string]string{}, probe)

	require.True(t, svc.PlaintextCaptureSupport(ctx).Supported)
	require.True(t, svc.PlaintextCaptureSupport(ctx).Supported)
	require.Equal(t, 1, probe.calls, "TTL 内的重复读取不得重复查目录")

	// 失败关闭方向必须立刻生效：结论变了以后，缓存里的旧「支持」不能继续放行一次开启。
	probe.support = PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonPartitionedUsageLogs}
	_, err := svc.UpdateRequestAuditValueDetailOperatorSettings(ctx, RequestAuditValueDetailOperatorUpdateInput{
		Enabled:     true,
		Language:    "en",
		Phrase:      RequestAuditValueDetailRiskAcknowledgementPhraseEN,
		AdminUserID: 7,
	})
	require.ErrorIs(t, err, ErrRequestAuditValueDetailDeploymentUnsupported,
		"开启必须绕过缓存，用此刻的数据库形态判定")
	require.Equal(t, 2, probe.calls)

	// 开启路径把新鲜结论写回缓存：更新响应里随后回显的状态与本次判定同源，不再额外查一次。
	require.False(t, svc.PlaintextCaptureSupport(ctx).Supported)
	require.Equal(t, 2, probe.calls)

}

// TestProvideSettingServiceInjectsPlaintextCaptureSupportProbe 锁定生产装配路径：
// 构造器必须把探针接进 SettingService，否则线上三类新明文采集永远开启不了。
func TestProvideSettingServiceInjectsPlaintextCaptureSupportProbe(t *testing.T) {
	// ProvideSettingService 会安装进程级解析器（Codex / Claude / Antigravity 版本），
	// 测试结束后恢复默认，避免污染同包其它用例。
	t.Cleanup(func() {
		SetCodexCanonicalUserAgentResolver(nil)
		claude.SetCLIVersionResolver(nil)
		antigravity.SetUserAgentVersionResolver(nil)
	})

	probe := &plaintextSupportProbeStub{support: PlaintextCaptureSupport{Supported: true}}
	svc := ProvideSettingService(&valueDetailSettingRepoStub{}, nil, nil, probe, &config.Config{})

	require.True(t, svc.PlaintextCaptureSupport(context.Background()).Supported,
		"构造器必须注入探针：漏接会让三类新明文采集在线上永远关闭")
	require.Equal(t, 1, probe.calls)

	// 显式传 nil（离线测试/没有探针的部署）必须 fail closed，而不是当成「支持」。
	closed := ProvideSettingService(&valueDetailSettingRepoStub{}, nil, nil, nil, &config.Config{})
	require.False(t, closed.PlaintextCaptureSupport(context.Background()).Supported)
}
