//go:build unit

package service

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// valueDetailSettingRepoStub 是 SettingRepository 的最小实现：只保存键值，
// 足以验证门控与书面确认的读写路径。
type valueDetailSettingRepoStub struct {
	values map[string]string
}

func (r *valueDetailSettingRepoStub) Get(_ context.Context, key string) (*Setting, error) {
	if value, ok := r.values[key]; ok {
		return &Setting{Key: key, Value: value}, nil
	}
	return nil, ErrSettingNotFound
}

func (r *valueDetailSettingRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	setting, err := r.Get(ctx, key)
	if err != nil {
		return "", err
	}
	return setting.Value, nil
}

func (r *valueDetailSettingRepoStub) Set(ctx context.Context, key, value string) error {
	return r.SetMultiple(ctx, map[string]string{key: value})
}

func (r *valueDetailSettingRepoStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *valueDetailSettingRepoStub) SetMultiple(_ context.Context, settings map[string]string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	for key, value := range settings {
		r.values[key] = value
	}
	return nil
}

func (r *valueDetailSettingRepoStub) GetAll(_ context.Context) (map[string]string, error) {
	out := map[string]string{}
	for key, value := range r.values {
		out[key] = value
	}
	return out, nil
}

func (r *valueDetailSettingRepoStub) Delete(_ context.Context, key string) error {
	delete(r.values, key)
	return nil
}

func valueDetailConfigWithStableKey(t *testing.T) *config.Config {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return &config.Config{Totp: config.TotpConfig{
		EncryptionKey:           hex.EncodeToString(key),
		EncryptionKeyConfigured: true,
	}}
}

// 默认关闭：没有任何设置时门控全关，且不读取确认键。
func TestRequestAuditValueDetailGateDefaultsClosed(t *testing.T) {
	repo := &valueDetailSettingRepoStub{}
	svc := NewSettingService(repo, valueDetailConfigWithStableKey(t))

	gate := svc.RequestAuditValueDetailGate(context.Background())
	require.False(t, gate.CaptureAllowed)
	require.False(t, gate.EncryptionAvailable)

	status, err := svc.GetRequestAuditValueDetailOperatorStatus(context.Background())
	require.NoError(t, err)
	require.False(t, status.Enabled)
	require.False(t, status.RiskAcknowledged)
	require.False(t, status.CaptureAllowed)
	require.True(t, status.EncryptionKeyAvailable)
	require.Empty(t, status.RiskAcknowledgement)
	require.False(t, status.RiskAcknowledgementCurrent)
}

// 直接改库把布尔值写成真不足以打开采集：没有覆盖当前语句的书面确认就没有采集。
func TestRequestAuditValueDetailGateRequiresWrittenAcknowledgement(t *testing.T) {
	payload, err := json.Marshal(RequestAuditValueDetailSettings{Enabled: true, RiskAcknowledged: true})
	require.NoError(t, err)
	repo := &valueDetailSettingRepoStub{values: map[string]string{
		SettingKeyRequestAuditValueDetail: string(payload),
	}}
	svc := NewSettingService(repo, valueDetailConfigWithStableKey(t))

	gate := svc.RequestAuditValueDetailGate(context.Background())
	require.False(t, gate.CaptureAllowed, "没有书面确认时不得采集")

	status, err := svc.GetRequestAuditValueDetailOperatorStatus(context.Background())
	require.NoError(t, err)
	require.True(t, status.Enabled, "存量值必须如实回显")
	require.True(t, status.RiskAcknowledged)
	require.False(t, status.CaptureAllowed, "校验结论必须与采集侧一致")
	require.False(t, status.RiskAcknowledgementCurrent)
}

// 版本正确但原文任意的记录不构成有效确认（伪造记录不能打开采集）。
func TestRequestAuditValueDetailRiskAcknowledgementRejectsArbitraryPhrase(t *testing.T) {
	ack := RequestAuditValueDetailRiskAcknowledgement{
		Version:     RequestAuditValueDetailRiskAcknowledgementVersion,
		Phrase:      "yes I accept",
		AdminUserID: 1,
		AcceptedAt:  time.Now().UTC(),
	}
	require.False(t, ack.CoversCurrentStatement())

	ack.Phrase = RequestAuditValueDetailRiskAcknowledgementPhraseEN
	require.True(t, ack.CoversCurrentStatement())

	ack.Phrase = RequestAuditValueDetailRiskAcknowledgementPhraseZH
	require.True(t, ack.CoversCurrentStatement())

	ack.Version = "v0"
	require.False(t, ack.CoversCurrentStatement())
}

// 开启需要管理员身份、逐字确认与可用密钥；关闭永远允许且不需要任何前置条件。
func TestUpdateRequestAuditValueDetailOperatorSettings(t *testing.T) {
	repo := &valueDetailSettingRepoStub{}
	svc := NewSettingService(repo, valueDetailConfigWithStableKey(t))
	ctx := context.Background()

	// 没有管理员身份：不能退化成匿名开启。
	_, err := svc.UpdateRequestAuditValueDetailOperatorSettings(ctx, RequestAuditValueDetailOperatorUpdateInput{Enabled: true})
	require.ErrorIs(t, err, ErrRequestAuditValueDetailOperatorIdentityRequired)

	_, err = svc.UpdateRequestAuditValueDetailOperatorSettings(ctx, RequestAuditValueDetailOperatorUpdateInput{
		Enabled: true, AdminUserID: 7,
	})
	require.ErrorIs(t, err, ErrRequestAuditValueDetailRiskAcknowledgementRequired)

	_, err = svc.UpdateRequestAuditValueDetailOperatorSettings(ctx, RequestAuditValueDetailOperatorUpdateInput{
		Enabled: true, AdminUserID: 7, Phrase: "wrong",
	})
	require.ErrorIs(t, err, ErrRequestAuditValueDetailRiskAcknowledgementInvalid)

	status, err := svc.UpdateRequestAuditValueDetailOperatorSettings(ctx, RequestAuditValueDetailOperatorUpdateInput{
		Enabled: true, AdminUserID: 7, Phrase: RequestAuditValueDetailRiskAcknowledgementPhraseEN,
		IPAddress: "127.0.0.1", UserAgent: "test",
	})
	require.NoError(t, err)
	require.True(t, status.CaptureAllowed)
	require.True(t, status.RiskAcknowledgementCurrent)
	require.NotNil(t, status.RiskAcknowledgement)
	require.Equal(t, int64(7), status.RiskAcknowledgement.AdminUserID)

	gate := svc.RequestAuditValueDetailGate(ctx)
	require.True(t, gate.CaptureAllowed)
	require.True(t, gate.EncryptionAvailable)

	// 关闭不需要确认或身份，且不能被任何前置校验挡住。
	status, err = svc.UpdateRequestAuditValueDetailOperatorSettings(ctx, RequestAuditValueDetailOperatorUpdateInput{})
	require.NoError(t, err)
	require.False(t, status.CaptureAllowed)
	require.False(t, svc.RequestAuditValueDetailGate(ctx).CaptureAllowed)
}

// 没有稳定配置密钥时不允许打开：每次采集都注定被判为缺密钥，那是把配置故障显示成正常运行。
func TestUpdateRequestAuditValueDetailOperatorSettingsRequiresStableKey(t *testing.T) {
	svc := NewSettingService(&valueDetailSettingRepoStub{}, &config.Config{})
	_, err := svc.UpdateRequestAuditValueDetailOperatorSettings(context.Background(), RequestAuditValueDetailOperatorUpdateInput{
		Enabled: true, AdminUserID: 7, Phrase: RequestAuditValueDetailRiskAcknowledgementPhraseEN,
	})
	require.ErrorIs(t, err, ErrRequestAuditValueDetailKeyUnavailable)

	// 自动生成的进程级密钥同样不可用：换进程即变，密文会永久不可解。
	auto := &config.Config{Totp: config.TotpConfig{EncryptionKey: hex.EncodeToString(make([]byte, 32))}}
	svc = NewSettingService(&valueDetailSettingRepoStub{}, auto)
	require.False(t, svc.RequestAuditValueDetailEncryptionKeyAvailable())

	// 长度不对的密钥也不可用。
	short := &config.Config{Totp: config.TotpConfig{EncryptionKey: "abcd", EncryptionKeyConfigured: true}}
	svc = NewSettingService(&valueDetailSettingRepoStub{}, short)
	require.False(t, svc.RequestAuditValueDetailEncryptionKeyAvailable())
}

// 中文语句按 zh* 归一确认：两种语言各自按自己的原文确认。
func TestUpdateRequestAuditValueDetailOperatorSettingsAcceptsChinesePhrase(t *testing.T) {
	svc := NewSettingService(&valueDetailSettingRepoStub{}, valueDetailConfigWithStableKey(t))
	status, err := svc.UpdateRequestAuditValueDetailOperatorSettings(context.Background(), RequestAuditValueDetailOperatorUpdateInput{
		Enabled: true, AdminUserID: 9, Language: "zh-CN", Phrase: RequestAuditValueDetailRiskAcknowledgementPhraseZH,
	})
	require.NoError(t, err)
	require.True(t, status.CaptureAllowed)
	require.Equal(t, RequestAuditValueDetailRiskAcknowledgementPhraseZH, status.RiskAcknowledgement.Phrase)

	// 语言归一：用中文语言提交英文原文不通过。
	_, err = svc.UpdateRequestAuditValueDetailOperatorSettings(context.Background(), RequestAuditValueDetailOperatorUpdateInput{
		Enabled: true, AdminUserID: 9, Language: "zh", Phrase: RequestAuditValueDetailRiskAcknowledgementPhraseEN,
	})
	require.ErrorIs(t, err, ErrRequestAuditValueDetailRiskAcknowledgementInvalid)
}

// 确认键损坏时不得被当成「已确认」。
func TestRequestAuditValueDetailGateRejectsCorruptAcknowledgement(t *testing.T) {
	payload, err := json.Marshal(RequestAuditValueDetailSettings{Enabled: true, RiskAcknowledged: true})
	require.NoError(t, err)
	repo := &valueDetailSettingRepoStub{values: map[string]string{
		SettingKeyRequestAuditValueDetail:                    string(payload),
		SettingKeyRequestAuditValueDetailRiskAcknowledgement: "{not json",
	}}
	svc := NewSettingService(repo, valueDetailConfigWithStableKey(t))
	require.False(t, svc.RequestAuditValueDetailGate(context.Background()).CaptureAllowed)

	// 形状残缺（缺管理员 ID / 确认时间）同样不是证据。
	partial, err := json.Marshal(RequestAuditValueDetailRiskAcknowledgement{
		Version: RequestAuditValueDetailRiskAcknowledgementVersion,
		Phrase:  RequestAuditValueDetailRiskAcknowledgementPhraseEN,
	})
	require.NoError(t, err)
	repo.values[SettingKeyRequestAuditValueDetailRiskAcknowledgement] = string(partial)
	require.False(t, svc.RequestAuditValueDetailGate(context.Background()).CaptureAllowed)

	// 设置存储整体不可用时按全关处理，不抛错到采集热路径。
	require.False(t, (*SettingService)(nil).RequestAuditValueDetailGate(context.Background()).CaptureAllowed)
	require.False(t, NewSettingService(nil, nil).RequestAuditValueDetailGate(context.Background()).CaptureAllowed)
}
