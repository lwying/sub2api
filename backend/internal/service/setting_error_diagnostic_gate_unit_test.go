//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 采集门控的唯一判定点：有效采集 = 存量布尔值为真 **且** 存在覆盖当前语句版本的有效书面确认。
// 这些用例锁定「直接改库不足以打开采集」以及「关闭态不读取确认键」。

type errorDiagnosticAckReaderStub struct {
	current bool
	err     error
	calls   int
}

func (r *errorDiagnosticAckReaderStub) ErrorDiagnosticRiskAcknowledgementCurrent(context.Context) (bool, error) {
	r.calls++
	return r.current, r.err
}

func enabledStoredSettings() ErrorDiagnosticSettings {
	return ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true}
}

func TestApplyErrorDiagnosticRiskAcknowledgement_RequiresWrittenAcknowledgement(t *testing.T) {
	ctx := context.Background()

	t.Run("stored booleans alone are not enough", func(t *testing.T) {
		// 越权改写／历史脏数据把布尔值写成真，但没有当前版本的有效确认。
		reader := &errorDiagnosticAckReaderStub{current: false}
		effective := ApplyErrorDiagnosticRiskAcknowledgement(ctx, enabledStoredSettings(), reader)

		require.False(t, effective.CaptureAllowed(), "没有书面确认就不得采集")
		require.False(t, effective.BodyCaptureAllowed(), "没有书面确认就不得留存正文")
		require.False(t, effective.Enabled)
		require.False(t, effective.RiskAcknowledged)
		require.False(t, effective.BodyRetentionEnabled)
		require.Equal(t, 1, reader.calls)
	})

	t.Run("current acknowledgement allows capture", func(t *testing.T) {
		reader := &errorDiagnosticAckReaderStub{current: true}
		effective := ApplyErrorDiagnosticRiskAcknowledgement(ctx, enabledStoredSettings(), reader)

		require.True(t, effective.CaptureAllowed())
		require.True(t, effective.BodyCaptureAllowed())
		require.Equal(t, 1, reader.calls)
	})

	t.Run("acknowledgement read failure degrades to off", func(t *testing.T) {
		// 确认键损坏时采集必须停止，而不是让采集热路径去处理这个错误。
		reader := &errorDiagnosticAckReaderStub{err: errors.New("ack store unavailable")}
		effective := ApplyErrorDiagnosticRiskAcknowledgement(ctx, enabledStoredSettings(), reader)

		require.False(t, effective.CaptureAllowed())
		require.Equal(t, 1, reader.calls)
	})

	t.Run("missing reader degrades to off", func(t *testing.T) {
		effective := ApplyErrorDiagnosticRiskAcknowledgement(ctx, enabledStoredSettings(), nil)
		require.False(t, effective.CaptureAllowed())
	})
}

func TestApplyErrorDiagnosticRiskAcknowledgement_OffDoesNotReadTheAckKey(t *testing.T) {
	// 关闭态（默认态）绝不能依赖确认记录，也不该为它产生额外读取：
	// 这是门控的常态路径，必须保持零额外成本且不受确认键损坏影响。
	reader := &errorDiagnosticAckReaderStub{current: false, err: errors.New("ack store unavailable")}

	for name, settings := range map[string]ErrorDiagnosticSettings{
		"all false":             {},
		"enabled only":          {Enabled: true},
		"risk ack without flag": {RiskAcknowledged: true},
		"body flag without ack": {Enabled: true, BodyRetentionEnabled: true},
	} {
		t.Run(name, func(t *testing.T) {
			require.False(t, settings.CaptureAllowed())
			effective := ApplyErrorDiagnosticRiskAcknowledgement(context.Background(), settings, reader)
			require.Equal(t, settings, effective, "关闭态必须原样返回存量值")
			require.Zero(t, reader.calls, "关闭态不得读取确认键")
		})
	}
}

// errorDiagnosticSettingRepoStub 服务于组合测试：同时提供门控键与确认键。
type errorDiagnosticSettingRepoStub struct {
	mu        sync.Mutex
	values    map[string]string
	readCalls map[string]int
}

func newErrorDiagnosticSettingRepoStub(values map[string]string) *errorDiagnosticSettingRepoStub {
	return &errorDiagnosticSettingRepoStub{values: values, readCalls: map[string]int{}}
}

func (s *errorDiagnosticSettingRepoStub) Get(context.Context, string) (*Setting, error) {
	panic("unexpected Get call")
}

func (s *errorDiagnosticSettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readCalls[key]++
	if value, ok := s.values[key]; ok {
		return value, nil
	}
	return "", ErrSettingNotFound
}

func (s *errorDiagnosticSettingRepoStub) calls(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readCalls[key]
}

func (s *errorDiagnosticSettingRepoStub) Set(context.Context, string, string) error {
	panic("unexpected Set call")
}

func (s *errorDiagnosticSettingRepoStub) GetMultiple(context.Context, []string) (map[string]string, error) {
	panic("unexpected GetMultiple call")
}

func (s *errorDiagnosticSettingRepoStub) SetMultiple(context.Context, map[string]string) error {
	panic("unexpected SetMultiple call")
}

func (s *errorDiagnosticSettingRepoStub) GetAll(context.Context) (map[string]string, error) {
	panic("unexpected GetAll call")
}

func (s *errorDiagnosticSettingRepoStub) Delete(context.Context, string) error {
	panic("unexpected Delete call")
}

// errorDiagnosticAckJSON 造一条**有效**的确认记录：当前版本语句 + 该版本的英文原文。
//
// 刻意不接收 *testing.T：门控替身（如 errorDiagnosticSettingsRepoStub）要在没有 t 的
// GetValue 里回这条记录。固定字面量的 Marshal 不可能失败，真失败说明类型被改坏，应当立刻停。
func errorDiagnosticAckJSON(version string) string {
	return errorDiagnosticAckJSONWithPhrase(version, ErrorDiagnosticRiskAcknowledgementPhraseEN)
}

// errorDiagnosticAckJSONWithPhrase 造一条指定语句原文的确认记录，供「原文不匹配」用例使用。
func errorDiagnosticAckJSONWithPhrase(version, phrase string) string {
	payload, err := json.Marshal(ErrorDiagnosticRiskAcknowledgement{
		Version:     version,
		Phrase:      phrase,
		AdminUserID: 7,
		AcceptedAt:  time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		panic("error diagnostic risk acknowledgement fixture is not marshalable: " + err.Error())
	}
	return string(payload)
}

// TestErrorDiagnosticAckFixtureIsAcceptedAsCurrent 锁定测试夹具本身是有效记录：
// 否则「当前版本确认放行」这条用例会因为夹具不完整而假通过（实际永远走拒绝分支）。
func TestErrorDiagnosticAckFixtureIsAcceptedAsCurrent(t *testing.T) {
	repo := newErrorDiagnosticSettingRepoStub(map[string]string{
		SettingKeyErrorDiagnosticRiskAcknowledgement: errorDiagnosticAckJSON(ErrorDiagnosticRiskAcknowledgementVersion),
	})
	settings := NewSettingService(repo, nil)

	current, err := settings.ErrorDiagnosticRiskAcknowledgementCurrent(context.Background())
	require.NoError(t, err)
	require.True(t, current, "夹具必须是当前版本的有效确认，否则放行用例没有意义")
}

// TestGetErrorDiagnosticSettings_ComposesStoredValueWithWrittenAcknowledgement 覆盖
// 采集侧真正使用的读取方法：它必须把存量值与书面确认合并，且存量读取方法保持独立。
func TestGetErrorDiagnosticSettings_ComposesStoredValueWithWrittenAcknowledgement(t *testing.T) {
	ctx := context.Background()
	onBlob := `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":true}`

	cases := []struct {
		name         string
		values       map[string]string
		wantCapture  bool
		wantStoredOn bool
	}{
		{
			name:         "stored on with no acknowledgement record",
			values:       map[string]string{SettingKeyErrorDiagnostic: onBlob},
			wantCapture:  false,
			wantStoredOn: true,
		},
		{
			name: "stored on with a stale-version acknowledgement",
			values: map[string]string{
				SettingKeyErrorDiagnostic:                    onBlob,
				SettingKeyErrorDiagnosticRiskAcknowledgement: errorDiagnosticAckJSON("v2020.01.01"),
			},
			wantCapture:  false,
			wantStoredOn: true,
		},
		{
			name: "stored on with a current-version acknowledgement",
			values: map[string]string{
				SettingKeyErrorDiagnostic:                    onBlob,
				SettingKeyErrorDiagnosticRiskAcknowledgement: errorDiagnosticAckJSON(ErrorDiagnosticRiskAcknowledgementVersion),
			},
			wantCapture:  true,
			wantStoredOn: true,
		},
		{
			name:         "no settings at all",
			values:       map[string]string{},
			wantCapture:  false,
			wantStoredOn: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newErrorDiagnosticSettingRepoStub(tc.values)
			settings := NewSettingService(repo, nil)

			// 有效结论：采集侧必须使用它。
			effective, err := settings.GetErrorDiagnosticSettings(ctx)
			require.NoError(t, err)
			require.Equal(t, tc.wantCapture, effective.CaptureAllowed())

			// 存量结论：仅运维展示使用，必须如实反映库里写了什么。
			stored, err := settings.ReadStoredErrorDiagnosticSettings(ctx)
			require.NoError(t, err)
			require.Equal(t, tc.wantStoredOn, stored.CaptureAllowed())

			if !tc.wantStoredOn {
				// 存量关闭时不读取确认键：关闭态不依赖确认记录。
				require.Zero(t, repo.calls(SettingKeyErrorDiagnosticRiskAcknowledgement))
			}
		})
	}
}

// TestErrorDiagnosticRiskAcknowledgementRequiresExactCurrentPhrase 覆盖确认校验的第二道门槛：
// 版本、管理员 ID 与确认时间齐全还不够——语句原文必须与当前版本的两条语句之一**逐字**相同。
//
// 只校验版本时，直接写库（或其它管理入口）落一条「版本正确、原文任意」的记录就能打开采集，
// 而「逐字确认」正是这道门槛的全部意义。这里同时锁定两种合法语言与各种不匹配形态，
// 并要求采集侧结论、运维界面结论与确认校验三者一致。
func TestErrorDiagnosticRiskAcknowledgementRequiresExactCurrentPhrase(t *testing.T) {
	ctx := context.Background()
	onBlob := `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":true}`

	// 两条语句必须彼此不同且非空，否则「原文逐字匹配」会退化成空集或恒真。
	require.NotEmpty(t, ErrorDiagnosticRiskAcknowledgementPhraseEN)
	require.NotEmpty(t, ErrorDiagnosticRiskAcknowledgementPhraseZH)
	require.NotEqual(t, ErrorDiagnosticRiskAcknowledgementPhraseEN, ErrorDiagnosticRiskAcknowledgementPhraseZH)

	cases := []struct {
		name        string
		version     string
		phrase      string
		wantCurrent bool
	}{
		{
			name:        "exact english phrase",
			version:     ErrorDiagnosticRiskAcknowledgementVersion,
			phrase:      ErrorDiagnosticRiskAcknowledgementPhraseEN,
			wantCurrent: true,
		},
		{
			name:        "exact chinese phrase",
			version:     ErrorDiagnosticRiskAcknowledgementVersion,
			phrase:      ErrorDiagnosticRiskAcknowledgementPhraseZH,
			wantCurrent: true,
		},
		{
			name:    "empty phrase",
			version: ErrorDiagnosticRiskAcknowledgementVersion,
			phrase:  "",
		},
		{
			// 伪造：版本、管理员 ID、确认时间都对，但原文是任意字符串。
			name:    "arbitrary phrase with the current version",
			version: ErrorDiagnosticRiskAcknowledgementVersion,
			phrase:  "I accept all risks",
		},
		{
			// 别的版本的语句原文也不算：确认过的必须是当前这段文字。
			name:    "statement text of another version",
			version: ErrorDiagnosticRiskAcknowledgementVersion,
			phrase:  "error diagnostic bodies are retained for 30 days",
		},
		{
			name:    "trailing space is not verbatim",
			version: ErrorDiagnosticRiskAcknowledgementVersion,
			phrase:  ErrorDiagnosticRiskAcknowledgementPhraseEN + " ",
		},
		{
			name:    "correct phrase but stale version",
			version: "v2020.01.01",
			phrase:  ErrorDiagnosticRiskAcknowledgementPhraseEN,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newErrorDiagnosticSettingRepoStub(map[string]string{
				SettingKeyErrorDiagnostic:                    onBlob,
				SettingKeyErrorDiagnosticRiskAcknowledgement: errorDiagnosticAckJSONWithPhrase(tc.version, tc.phrase),
			})
			// 带上稳定密钥，使本用例的变量只剩「语句原文」：否则正文留存会因为缺密钥
			// 而被另一道条件关掉，掩掉这里真正要断言的差异。
			settings := NewSettingService(repo, &config.Config{Totp: config.TotpConfig{
				EncryptionKey:           errorDiagnosticStableEncryptionKey,
				EncryptionKeyConfigured: true,
			}})

			current, err := settings.ErrorDiagnosticRiskAcknowledgementCurrent(ctx)
			require.NoError(t, err)
			require.Equal(t, tc.wantCurrent, current, "有效确认 = 版本匹配 且 原文逐字相同")

			// 采集侧：判定点必须与确认校验同结论，否则伪造记录仍能打开采集。
			effective, err := settings.GetErrorDiagnosticSettings(ctx)
			require.NoError(t, err)
			require.Equal(t, tc.wantCurrent, effective.CaptureAllowed())
			require.Equal(t, tc.wantCurrent, effective.BodyCaptureAllowed(),
				"正文留存比采集更窄：确认不成立时同样不得留存")

			// 运维界面：存量值如实回显，但校验结论不得比采集侧更宽松。
			status, err := settings.GetErrorDiagnosticOperatorStatus(ctx)
			require.NoError(t, err)
			require.Equal(t, tc.wantCurrent, status.RiskAcknowledgementCurrent)
			require.Equal(t, tc.wantCurrent, status.CaptureAllowed)
			require.True(t, status.Enabled, "存量布尔值如实回显")
			require.True(t, status.RiskAcknowledged, "存量布尔值如实回显")
			require.NotNil(t, status.RiskAcknowledgement,
				"记录本身仍要可见，界面才能解释「存了记录但没放行」")
		})
	}
}

// TestGetErrorDiagnosticSettings_DisablesBodyRetentionWithoutStableKey 覆盖「有效正文留存」的
// 第三道条件：稳定密钥不可用时，有效结论里的正文留存必须关掉，而元数据采集照常。
//
// 只收窄正文这一层：如果把「布尔值开着、但没有密钥」如实回报成可留存，接缝就会 tee 出
// 一份注定被判为 skipped_encryption_unavailable 的明文，白白复制最多 1 MiB 请求正文。
func TestGetErrorDiagnosticSettings_DisablesBodyRetentionWithoutStableKey(t *testing.T) {
	ctx := context.Background()
	values := map[string]string{
		SettingKeyErrorDiagnostic:                    `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":true}`,
		SettingKeyErrorDiagnosticRiskAcknowledgement: errorDiagnosticAckJSON(ErrorDiagnosticRiskAcknowledgementVersion),
	}

	// 没有稳定密钥：cfg 缺失等同启动时自动生成密钥的部署（密钥每次重启都会变）。
	withoutKey := NewSettingService(newErrorDiagnosticSettingRepoStub(values), nil)
	effective, err := withoutKey.GetErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.True(t, effective.CaptureAllowed(), "元数据采集不因缺密钥而关闭")
	require.False(t, effective.BodyRetentionEnabled, "没有稳定密钥时不得报告正文留存可用")
	require.False(t, effective.BodyCaptureAllowed())
	// 收窄只动有效值：存量意图必须一并带出，否则接缝只能把这次尝试记成 not_observed，
	// 而票据 02 要求缺密钥时留下稳定原因码再跳过（见
	// ErrorDiagnosticBodyVerdictSuppressedEncryptionUnavailable）。
	require.True(t, effective.BodyRetentionRequested, "存量要求过留存这一事实不得被收窄吞掉")
	require.True(t, effective.BodyRetentionSuppressedByMissingKey(false))

	// 运维结论必须与采集侧一致：界面不得显示得比采集侧更宽松。
	status, err := withoutKey.GetErrorDiagnosticOperatorStatus(ctx)
	require.NoError(t, err)
	require.True(t, status.CaptureAllowed)
	require.False(t, status.BodyEncryptionKeyAvailable)
	require.False(t, status.BodyRetentionAllowed)

	// 存量值仍如实回显，界面才能解释「存量开着、但有效结论是不可留存」。
	stored, err := withoutKey.ReadStoredErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.True(t, stored.BodyRetentionEnabled, "存量读取不受密钥影响")
	require.False(t, stored.BodyRetentionRequested,
		"意图是读取器在收窄时派生的进程内提示，不是落库字段")

	// 配好稳定密钥后，同一份存量值就真的可留存了。
	withKey := NewSettingService(newErrorDiagnosticSettingRepoStub(values), &config.Config{Totp: config.TotpConfig{
		EncryptionKey:           errorDiagnosticStableEncryptionKey,
		EncryptionKeyConfigured: true,
	}})
	effective, err = withKey.GetErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.True(t, effective.CaptureAllowed())
	require.True(t, effective.BodyRetentionEnabled)
	require.True(t, effective.BodyCaptureAllowed())
	require.True(t, effective.BodyRetentionRequested)
	require.False(t, effective.BodyRetentionSuppressedByMissingKey(true),
		"有密钥时不存在抑制：那是「要求过、做不到」，不是「有要求就报故障」")

	status, err = withKey.GetErrorDiagnosticOperatorStatus(ctx)
	require.NoError(t, err)
	require.True(t, status.BodyEncryptionKeyAvailable)
	require.True(t, status.BodyRetentionAllowed)
}

// TestGetErrorDiagnosticSettings_MetadataOnlyIsNotAMissingKeySuppression 锁定两种「不留存正文」
// 必须保持可区分：正常关闭（存量没开正文）与配置故障（存量开了却没有密钥）。
//
// 只有后者才是封闭抑制：把前者也报成抑制会让每一行都留下误导性的原因码。
func TestGetErrorDiagnosticSettings_MetadataOnlyIsNotAMissingKeySuppression(t *testing.T) {
	ctx := context.Background()
	values := map[string]string{
		SettingKeyErrorDiagnostic:                    `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":false}`,
		SettingKeyErrorDiagnosticRiskAcknowledgement: errorDiagnosticAckJSON(ErrorDiagnosticRiskAcknowledgementVersion),
	}
	// 刻意不给 cfg：即使缺密钥，没开正文留存也不是配置故障。
	settings := NewSettingService(newErrorDiagnosticSettingRepoStub(values), nil)

	effective, err := settings.GetErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.True(t, effective.CaptureAllowed())
	require.False(t, effective.BodyRetentionEnabled)
	require.False(t, effective.BodyRetentionRequested, "存量没要求过留存，就不该带出意图")
	require.False(t, effective.BodyRetentionSuppressedByMissingKey(false),
		"没有要求过留存：这是正常关闭，不是缺密钥抑制")
}

// TestReadStoredErrorDiagnosticSettings_IgnoresAcknowledgement 覆盖
// 存量读取不得被确认记录影响：否则运维界面无法展示「存量开着、但不允许采集」。
func TestReadStoredErrorDiagnosticSettings_IgnoresAcknowledgement(t *testing.T) {
	ctx := context.Background()
	repo := newErrorDiagnosticSettingRepoStub(map[string]string{
		SettingKeyErrorDiagnostic: `{"enabled":true,"risk_acknowledged":true}`,
	})
	settings := NewSettingService(repo, nil)

	stored, err := settings.ReadStoredErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.True(t, stored.Enabled)
	require.True(t, stored.RiskAcknowledged)
	require.Zero(t, repo.calls(SettingKeyErrorDiagnosticRiskAcknowledgement), "存量读取不得触碰确认键")
}
