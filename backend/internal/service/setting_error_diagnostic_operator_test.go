//go:build unit

package service

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 运维开关的两个留存层（正文 / 429 头值）在服务层的判定。
//
// 两层门槛同形而互相独立：各自需要「本次逐字确认 + 自己那个开关 + 可用稳定密钥」，
// 谁也不代替谁，谁也不牵连谁。这里锁定的是服务层的那部分：
//   - 请求里没有本次确认时绝不因为库里已有一条确认而放行（不静默开启）；
//   - 缺密钥时按层拒绝，且被拒的更新不留下任何部分状态；
//   - 关闭是兜底方向：不需要确认或身份，并把两层一起关掉。

// errorDiagnosticOperatorSettingRepo 是可写的设置仓储替身（门控键 + 确认键）。
//
// 与只读的门控替身分开：这里的用例要通过**真实写入**证明「哪个键被写了、被拒的请求
// 一个键都没写」，因此 SetMultiple 必须真的落值并计数，而不是 panic。
type errorDiagnosticOperatorSettingRepo struct {
	mu     sync.Mutex
	values map[string]string
	writes int
}

func newErrorDiagnosticOperatorSettingRepo(values map[string]string) *errorDiagnosticOperatorSettingRepo {
	if values == nil {
		values = map[string]string{}
	}
	return &errorDiagnosticOperatorSettingRepo{values: values}
}

func (s *errorDiagnosticOperatorSettingRepo) Get(context.Context, string) (*Setting, error) {
	panic("unexpected Get call")
}

func (s *errorDiagnosticOperatorSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value, ok := s.values[key]; ok {
		return value, nil
	}
	return "", ErrSettingNotFound
}

func (s *errorDiagnosticOperatorSettingRepo) Set(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
	s.writes++
	return nil
}

func (s *errorDiagnosticOperatorSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	panic("unexpected GetMultiple call")
}

func (s *errorDiagnosticOperatorSettingRepo) SetMultiple(ctx context.Context, settings map[string]string) error {
	for key, value := range settings {
		if err := s.Set(ctx, key, value); err != nil {
			return err
		}
	}
	return nil
}

func (s *errorDiagnosticOperatorSettingRepo) GetAll(context.Context) (map[string]string, error) {
	panic("unexpected GetAll call")
}

func (s *errorDiagnosticOperatorSettingRepo) Delete(context.Context, string) error {
	panic("unexpected Delete call")
}

func (s *errorDiagnosticOperatorSettingRepo) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// storedSettings 读出落库的门控形状（落库字段，不含进程内派生字段）。
func (s *errorDiagnosticOperatorSettingRepo) storedSettings() ErrorDiagnosticSettings {
	s.mu.Lock()
	raw := s.values[SettingKeyErrorDiagnostic]
	s.mu.Unlock()
	var settings ErrorDiagnosticSettings
	if raw == "" {
		return settings
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		panic("stored error diagnostic settings are not decodable: " + err.Error())
	}
	return settings
}

func (s *errorDiagnosticOperatorSettingRepo) storedValue(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[key]
}

// errorDiagnosticOperatorServiceWithKey 构造带（或不带）稳定密钥的设置服务。
//
// keyConfigured=false 等同启动时随机生成主密钥的部署：两层留存都拿不出稳定密钥，
// 而元数据采集不依赖密钥。
func errorDiagnosticOperatorServiceWithKey(values map[string]string, keyConfigured bool) (*SettingService, *errorDiagnosticOperatorSettingRepo) {
	repo := newErrorDiagnosticOperatorSettingRepo(values)
	cfg := &config.Config{}
	if keyConfigured {
		cfg.Totp.EncryptionKey = errorDiagnosticStableEncryptionKey
		cfg.Totp.EncryptionKeyConfigured = true
	}
	return NewSettingService(repo, cfg), repo
}

func enabledWithHeaderValuesRequest() ErrorDiagnosticOperatorUpdateInput {
	return ErrorDiagnosticOperatorUpdateInput{
		Enabled:                     true,
		HeaderValueRetentionEnabled: true,
		Language:                    "en",
		Phrase:                      ErrorDiagnosticRiskAcknowledgementPhraseEN,
		AdminUserID:                 7,
	}
}

// TestUpdateErrorDiagnosticOperatorSettings_HeaderValueRetentionGate 覆盖头值留存的
// 开门条件：本次逐字确认 + 自己的开关 + 可用稳定密钥，缺一不可，且被拒时不写任何键。
func TestUpdateErrorDiagnosticOperatorSettings_HeaderValueRetentionGate(t *testing.T) {
	ctx := context.Background()

	t.Run("no stable key refuses the header value layer", func(t *testing.T) {
		settings, repo := errorDiagnosticOperatorServiceWithKey(nil, false)
		_, err := settings.UpdateErrorDiagnosticOperatorSettings(ctx, enabledWithHeaderValuesRequest())

		require.ErrorIs(t, err, ErrErrorDiagnosticHeaderValueRetentionKeyUnavailable)
		require.Zero(t, repo.writeCount(), "被拒的更新不得写入任何门控状态")
	})

	t.Run("missing phrase refuses even with the flag set", func(t *testing.T) {
		settings, repo := errorDiagnosticOperatorServiceWithKey(nil, true)
		input := enabledWithHeaderValuesRequest()
		input.Phrase = ""

		_, err := settings.UpdateErrorDiagnosticOperatorSettings(ctx, input)

		require.ErrorIs(t, err, ErrErrorDiagnosticRiskAcknowledgementRequired)
		require.Zero(t, repo.writeCount())
	})

	t.Run("flag and phrase and key together enable only the header value layer", func(t *testing.T) {
		settings, repo := errorDiagnosticOperatorServiceWithKey(nil, true)
		status, err := settings.UpdateErrorDiagnosticOperatorSettings(ctx, enabledWithHeaderValuesRequest())

		require.NoError(t, err)
		require.True(t, status.Enabled)
		require.True(t, status.CaptureAllowed)
		require.True(t, status.HeaderValueRetentionEnabled, "存量开关如实回显")
		require.True(t, status.HeaderValueRetentionAllowed)
		require.False(t, status.BodyRetentionEnabled, "头值开关不得顺带打开正文留存")
		require.False(t, status.BodyRetentionAllowed)

		stored := repo.storedSettings()
		require.True(t, stored.HeaderValuesCaptureAllowed())
		require.False(t, stored.BodyCaptureAllowed())

		// 采集侧读到的有效结论与运维界面的结论同源。
		effective, err := settings.GetErrorDiagnosticSettings(ctx)
		require.NoError(t, err)
		require.True(t, effective.HeaderValuesCaptureAllowed())
		require.False(t, effective.BodyCaptureAllowed())
	})

	t.Run("body retention stays independent of the header value layer", func(t *testing.T) {
		settings, repo := errorDiagnosticOperatorServiceWithKey(nil, true)
		input := enabledWithHeaderValuesRequest()
		input.HeaderValueRetentionEnabled = false
		input.BodyRetentionEnabled = true

		status, err := settings.UpdateErrorDiagnosticOperatorSettings(ctx, input)

		require.NoError(t, err)
		require.True(t, status.BodyRetentionAllowed)
		require.False(t, status.HeaderValueRetentionEnabled, "正文开关不得顺带打开头值留存")
		require.False(t, status.HeaderValueRetentionAllowed)
		require.False(t, repo.storedSettings().HeaderValuesCaptureAllowed())
	})
}

// TestUpdateErrorDiagnosticOperatorSettings_HeaderValuesNeedAFreshAcknowledgement 覆盖
// 「不静默开启」：库里已有一条当前版本的有效确认、门控也仍是开着的，都不能代替本次确认。
func TestUpdateErrorDiagnosticOperatorSettings_HeaderValuesNeedAFreshAcknowledgement(t *testing.T) {
	ctx := context.Background()
	values := map[string]string{
		SettingKeyErrorDiagnostic:                    `{"enabled":true,"risk_acknowledged":true}`,
		SettingKeyErrorDiagnosticRiskAcknowledgement: errorDiagnosticAckJSON(ErrorDiagnosticRiskAcknowledgementVersion),
	}
	settings, repo := errorDiagnosticOperatorServiceWithKey(values, true)

	require.True(t, repo.storedSettings().CaptureAllowed(), "存量门控本来就在运行")

	input := enabledWithHeaderValuesRequest()
	input.Phrase = ""
	_, err := settings.UpdateErrorDiagnosticOperatorSettings(ctx, input)

	require.ErrorIs(t, err, ErrErrorDiagnosticRiskAcknowledgementRequired,
		"已有确认不得代替本次逐字确认")
	require.False(t, repo.storedSettings().HeaderValueRetentionEnabled,
		"被拒的更新不得留下头值留存开关")
}

// TestUpdateErrorDiagnosticOperatorSettings_DisableClearsBothRetentionLayers 锁定兜底方向：
// 关闭不需要确认或身份，并把两个留存开关一起关掉（请求里写成 true 也不接受）。
func TestUpdateErrorDiagnosticOperatorSettings_DisableClearsBothRetentionLayers(t *testing.T) {
	ctx := context.Background()
	values := map[string]string{
		SettingKeyErrorDiagnostic:                    `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":true,"header_values_enabled":true}`,
		SettingKeyErrorDiagnosticRiskAcknowledgement: errorDiagnosticAckJSON(ErrorDiagnosticRiskAcknowledgementVersion),
	}
	settings, repo := errorDiagnosticOperatorServiceWithKey(values, true)

	status, err := settings.UpdateErrorDiagnosticOperatorSettings(ctx, ErrorDiagnosticOperatorUpdateInput{
		Enabled:                     false,
		BodyRetentionEnabled:        true,
		HeaderValueRetentionEnabled: true,
		AdminUserID:                 0,
	})

	require.NoError(t, err)
	require.False(t, status.Enabled)
	require.False(t, status.BodyRetentionEnabled)
	require.False(t, status.HeaderValueRetentionEnabled)
	require.False(t, status.CaptureAllowed)
	require.False(t, status.HeaderValueRetentionAllowed)

	stored := repo.storedSettings()
	require.False(t, stored.BodyCaptureAllowed())
	require.False(t, stored.HeaderValuesCaptureAllowed())

	// 确认记录作为历史审计保留：关闭不抹掉已经发生过的确认。
	require.NotEmpty(t, repo.storedValue(SettingKeyErrorDiagnosticRiskAcknowledgement))

	// 关闭后采集侧读到的是全关。
	effective, err := settings.GetErrorDiagnosticSettings(ctx)
	require.NoError(t, err)
	require.False(t, effective.CaptureAllowed())
	require.False(t, effective.HeaderValuesCaptureAllowed())
}

// TestGetErrorDiagnosticOperatorStatus_HeaderValuesStoredVersusEffective 锁定「存量开关」与
// 「有效结论」是两件事：存量开着而缺密钥（或确认过期）时，头值层的结论必须明确为不可用，
// 而**读取器**里的头值开关保持不变——传输接缝要靠它推导「要求过、但没有密钥」的封闭抑制，
// 收窄会把这个配置故障退化成 not_observed（丢掉 skipped_encryption_unavailable）。
func TestGetErrorDiagnosticOperatorStatus_HeaderValuesStoredVersusEffective(t *testing.T) {
	ctx := context.Background()
	storedOn := `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":true,"header_values_enabled":true}`
	currentAck := errorDiagnosticAckJSON(ErrorDiagnosticRiskAcknowledgementVersion)

	t.Run("without a stable key the conclusion is off while the stored intent stays", func(t *testing.T) {
		settings, _ := errorDiagnosticOperatorServiceWithKey(map[string]string{
			SettingKeyErrorDiagnostic:                    storedOn,
			SettingKeyErrorDiagnosticRiskAcknowledgement: currentAck,
		}, false)

		status, err := settings.GetErrorDiagnosticOperatorStatus(ctx)
		require.NoError(t, err)
		require.True(t, status.HeaderValueRetentionEnabled, "存量意图照实回显")
		require.False(t, status.HeaderValueRetentionAllowed, "没有稳定密钥就不是有效头值留存")
		require.False(t, status.BodyRetentionAllowed)
		require.True(t, status.CaptureAllowed, "两层留存都不可用不影响元数据采集")

		effective, err := settings.GetErrorDiagnosticSettings(ctx)
		require.NoError(t, err)
		require.False(t, effective.BodyRetentionEnabled, "正文那一层读出来即已收窄")
		require.True(t, effective.BodyRetentionRequested, "正文的存量意图仍被带出")
		require.True(t, effective.HeaderValueRetentionEnabled,
			"头值开关不得在读取器上被收窄：接缝据此表达「要求过、但拿不出密钥」")
		require.True(t, effective.HeaderValuesCaptureAllowed())
	})

	t.Run("with a stale acknowledgement nothing is effective", func(t *testing.T) {
		settings, _ := errorDiagnosticOperatorServiceWithKey(map[string]string{
			SettingKeyErrorDiagnostic: storedOn,
			// 语句内容变化前的确认：形状完整，但不覆盖当前语句。
			SettingKeyErrorDiagnosticRiskAcknowledgement: errorDiagnosticAckJSONWithPhrase(
				"v2026.09.24", "old statement without the header value layer"),
		}, true)

		status, err := settings.GetErrorDiagnosticOperatorStatus(ctx)
		require.NoError(t, err)
		require.True(t, status.HeaderValueRetentionEnabled, "存量意图照实回显")
		require.False(t, status.RiskAcknowledgementCurrent)
		require.False(t, status.CaptureAllowed)
		require.False(t, status.HeaderValueRetentionAllowed, "确认过期时头值留存不得生效")
		require.False(t, status.BodyRetentionAllowed)

		effective, err := settings.GetErrorDiagnosticSettings(ctx)
		require.NoError(t, err)
		require.False(t, effective.CaptureAllowed())
		require.False(t, effective.HeaderValuesCaptureAllowed())
	})

	t.Run("with a current acknowledgement and a key both layers are effective", func(t *testing.T) {
		settings, _ := errorDiagnosticOperatorServiceWithKey(map[string]string{
			SettingKeyErrorDiagnostic:                    storedOn,
			SettingKeyErrorDiagnosticRiskAcknowledgement: currentAck,
		}, true)

		status, err := settings.GetErrorDiagnosticOperatorStatus(ctx)
		require.NoError(t, err)
		require.True(t, status.RiskAcknowledgementCurrent)
		require.True(t, status.CaptureAllowed)
		require.True(t, status.HeaderValueRetentionAllowed)
		require.True(t, status.BodyRetentionAllowed)
	})
}

// TestErrorDiagnosticHeaderValueRetentionAllowed 把结论函数本身钉住：三层条件必须同时成立，
// 任何一个为假都只能得到「不可用」。
func TestErrorDiagnosticHeaderValueRetentionAllowed(t *testing.T) {
	captureOn := ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true}

	cases := []struct {
		name         string
		settings     ErrorDiagnosticSettings
		keyAvailable bool
		want         bool
	}{
		{name: "capture off", settings: ErrorDiagnosticSettings{}, keyAvailable: true},
		{name: "header flag off", settings: captureOn, keyAvailable: true},
		{name: "no key", settings: ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true, HeaderValueRetentionEnabled: true}},
		{
			name:         "all three",
			settings:     ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true, HeaderValueRetentionEnabled: true},
			keyAvailable: true,
			want:         true,
		},
		{
			name:         "body flag alone never enables the header layer",
			settings:     ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true},
			keyAvailable: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, errorDiagnosticHeaderValueRetentionAllowed(tc.settings, tc.keyAvailable))
		})
	}
}

// TestErrorDiagnosticRiskAcknowledgementStatementCoversHeaderValues 锁定语句内容随行为一起
// 变化：书面确认必须把 429 头值这一层（7 天、只采白名单、排除凭据头）说清楚，否则「按语句
// 确认」覆盖不到头值留存这一层事实。逐字文本由管理员 API 的测试钉住，这里只守住要点。
func TestErrorDiagnosticRiskAcknowledgementStatementCoversHeaderValues(t *testing.T) {
	require.Contains(t, ErrorDiagnosticRiskAcknowledgementPhraseEN, "429 header values are retained for 7 days")
	require.Contains(t, ErrorDiagnosticRiskAcknowledgementPhraseZH, "429 头值保留 7 天")
	require.NotContains(t, ErrorDiagnosticRiskAcknowledgementPhraseEN, "header values are retained for 30 days")

	for name, statement := range map[string]string{
		"en": ErrorDiagnosticRiskAcknowledgementPhraseEN,
		"zh": ErrorDiagnosticRiskAcknowledgementPhraseZH,
	} {
		require.Contains(t, statement, "Cookie", name)
		require.Contains(t, statement, "Authorization", name)
		require.Contains(t, statement, "Set-Cookie", name)
		require.Contains(t, statement, "X-Api-Key", name)
	}
	require.Contains(t, ErrorDiagnosticRiskAcknowledgementPhraseEN, "429 header values")
	require.Contains(t, ErrorDiagnosticRiskAcknowledgementPhraseZH, "429 头值")
	require.Contains(t, ErrorDiagnosticRiskAcknowledgementPhraseEN, "allowlist")
	require.Contains(t, ErrorDiagnosticRiskAcknowledgementPhraseZH, "白名单")

	// 两条语句仍需各自完整可确认；版本变化后旧记录的原文不再匹配（由管理员 API 的
	// 逐字文本测试持有旧语句字面量）。
	acknowledgement := ErrorDiagnosticRiskAcknowledgement{
		Version:     ErrorDiagnosticRiskAcknowledgementVersion,
		Phrase:      ErrorDiagnosticRiskAcknowledgementPhraseZH,
		AdminUserID: 7,
	}
	require.True(t, acknowledgement.CoversCurrentStatement())
}
