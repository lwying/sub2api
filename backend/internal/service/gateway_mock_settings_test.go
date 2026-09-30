//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGatewayMockSettings_DefaultOffAndRuleValidation(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})

	require.False(t, svc.GatewayMockRuleset(ctx).Enabled, "缺省即关闭")

	// 关键词归一化：去首尾空白 + 拉丁字母小写
	status, err := svc.UpdateGatewayMockOperatorSettings(ctx, GatewayMockOperatorUpdateInput{
		Enabled: true,
		Rules: []GatewayMockRuleInput{
			{Keyword: "  HI  ", Reply: "第一回复", Enabled: true},
			{Keyword: "你好", Reply: "第二回复", Enabled: true},
		},
	})
	require.NoError(t, err)
	require.Len(t, status.Rules, 2)
	require.Equal(t, "hi", status.Rules[0].Keyword)
	require.Equal(t, "你好", status.Rules[1].Keyword)

	ruleset := svc.GatewayMockRuleset(ctx)
	require.True(t, ruleset.Enabled)
	match, ok := ruleset.MatchReply("Hi")
	require.True(t, ok)
	require.Equal(t, "第一回复", match.Reply)
}

func TestGatewayMockSettings_RejectsInvalidRules(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})

	_, err := svc.UpdateGatewayMockOperatorSettings(ctx, GatewayMockOperatorUpdateInput{
		Enabled: true,
		Rules:   []GatewayMockRuleInput{{Keyword: "   ", Reply: "回复"}},
	})
	require.ErrorIs(t, err, ErrGatewayMockRuleKeywordEmpty, "空/纯空白关键词不得保存")

	_, err = svc.UpdateGatewayMockOperatorSettings(ctx, GatewayMockOperatorUpdateInput{
		Enabled: true,
		Rules:   []GatewayMockRuleInput{{Keyword: "hi", Reply: "   "}},
	})
	require.ErrorIs(t, err, ErrGatewayMockRuleReplyEmpty, "空回复不得保存")

	_, err = svc.UpdateGatewayMockOperatorSettings(ctx, GatewayMockOperatorUpdateInput{
		Enabled: true,
		Rules: []GatewayMockRuleInput{
			{Keyword: "hi", Reply: "一"},
			{Keyword: " HI ", Reply: "二"},
		},
	})
	require.ErrorIs(t, err, ErrGatewayMockRuleKeywordTaken, "归一化后同关键词只能有一条规则")
}

func TestGatewayMockSettings_DisabledRulesLeaveTheSnapshot(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})

	_, err := svc.UpdateGatewayMockOperatorSettings(ctx, GatewayMockOperatorUpdateInput{
		Enabled: true,
		Rules: []GatewayMockRuleInput{
			{Keyword: "hi", Reply: "启用", Enabled: true},
			{Keyword: "test", Reply: "未启用", Enabled: false},
		},
	})
	require.NoError(t, err)

	ruleset := svc.GatewayMockRuleset(ctx)
	_, ok := ruleset.MatchReply("hi")
	require.True(t, ok)
	_, ok = ruleset.MatchReply("test")
	require.False(t, ok, "停用的规则不得进入热路径快照")
}

// 保存成功后本实例的下一次判断必须立刻看到新规则，不使用可能残留旧规则的缓存。
func TestGatewayMockSettings_UpdateInvalidatesCacheImmediately(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})

	_, err := svc.UpdateGatewayMockOperatorSettings(ctx, GatewayMockOperatorUpdateInput{
		Enabled: true,
		Rules:   []GatewayMockRuleInput{{Keyword: "hi", Reply: "回复", Enabled: true}},
	})
	require.NoError(t, err)
	_, ok := svc.GatewayMockRuleset(ctx).MatchReply("hi")
	require.True(t, ok)

	// 关闭总开关后立即失效
	_, err = svc.UpdateGatewayMockOperatorSettings(ctx, GatewayMockOperatorUpdateInput{Enabled: false})
	require.NoError(t, err)
	require.False(t, svc.GatewayMockRuleset(ctx).Enabled, "保存后的新判断不得沿用旧结论")
}

// gatewayMockFailingRepo 让设置读取失败，用于验证"读取失败按关闭处理"。
type gatewayMockFailingRepo struct {
	SettingRepository
}

func (gatewayMockFailingRepo) GetValue(context.Context, string) (string, error) {
	return "", errors.New("settings store unavailable")
}

// 读取失败必须按关闭处理，绝不能 fail-open 到"拦截"。
func TestGatewayMockSettings_ReadFailureKeepsCapabilityOff(t *testing.T) {
	svc := NewSettingService(gatewayMockFailingRepo{}, &config.Config{})
	require.False(t, svc.GatewayMockRuleset(context.Background()).Enabled)
}

func TestGatewayMockPresets_SeededButNotEnabled(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})

	status, created, err := svc.SeedGatewayMockPresets(ctx)
	require.NoError(t, err)
	require.Positive(t, created)
	require.False(t, status.Enabled, "预置规则不得顺带打开总开关")
	for _, rule := range status.Rules {
		require.False(t, rule.Enabled, "预置规则默认不生效")
	}

	// 已有规则时不得重复播种
	_, created, err = svc.SeedGatewayMockPresets(ctx)
	require.NoError(t, err)
	require.Zero(t, created)

	// 且预置内容不包含确认过的高误杀风险短词
	encoded, err := json.Marshal(status.Rules)
	require.NoError(t, err)
	for _, risky := range []string{`"好"`, `"行"`, `"ok"`, `"再见"`} {
		require.NotContains(t, string(encoded), risky, "不得预置容易误杀的短词")
	}
}
