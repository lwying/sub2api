package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// rateLimit429CooldownSettingRepo 记录每次写入，用来断言「同一功能」的配置是原子写入。
type rateLimit429CooldownSettingRepo struct {
	mu         sync.Mutex
	values     map[string]string
	getErr     error
	writeErr   error
	writeCalls []map[string]string
}

func (r *rateLimit429CooldownSettingRepo) Get(context.Context, string) (*Setting, error) {
	panic("unexpected Get call")
}

func (r *rateLimit429CooldownSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return "", r.getErr
	}
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", ErrSettingNotFound
}

func (r *rateLimit429CooldownSettingRepo) Set(context.Context, string, string) error {
	panic("unexpected Set call")
}

func (r *rateLimit429CooldownSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	panic("unexpected GetMultiple call")
}

func (r *rateLimit429CooldownSettingRepo) SetMultiple(_ context.Context, settings map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.writeErr != nil {
		return r.writeErr
	}
	call := make(map[string]string, len(settings))
	for key, value := range settings {
		call[key] = value
		if r.values == nil {
			r.values = map[string]string{}
		}
		r.values[key] = value
	}
	r.writeCalls = append(r.writeCalls, call)
	return nil
}

func (r *rateLimit429CooldownSettingRepo) GetAll(context.Context) (map[string]string, error) {
	panic("unexpected GetAll call")
}

func (r *rateLimit429CooldownSettingRepo) Delete(context.Context, string) error {
	panic("unexpected Delete call")
}

func newRateLimit429CooldownService(stored map[string]string) (*SettingService, *rateLimit429CooldownSettingRepo) {
	repo := &rateLimit429CooldownSettingRepo{values: stored}
	return NewSettingService(repo, nil), repo
}

// TestGetRateLimit429AccountLimitCooldownDefaultsOffSessionSixty 覆盖验收标准 1/7：
// 未配置时读回「关闭 / 会话级 / 60 秒」，并且不产生任何写入。
func TestGetRateLimit429AccountLimitCooldownDefaultsOffSessionSixty(t *testing.T) {
	svc, repo := newRateLimit429CooldownService(map[string]string{})

	cfg, err := svc.GetRateLimit429AccountLimitCooldown(context.Background())

	require.NoError(t, err)
	require.Equal(t, RateLimit429AccountLimitCooldown{
		Enabled:         false,
		Scope:           RateLimit429CooldownScopeSession,
		CooldownSeconds: 60,
	}, cfg)
	require.Empty(t, repo.writeCalls, "读取默认值不得落库")
}

// TestGetRateLimit429AccountLimitCooldownDefaults 校验导出的默认值函数与读取路径一致。
func TestGetRateLimit429AccountLimitCooldownDefaults(t *testing.T) {
	require.Equal(t, RateLimit429AccountLimitCooldown{
		Enabled:         false,
		Scope:           RateLimit429CooldownScopeSession,
		CooldownSeconds: 60,
	}, DefaultRateLimit429AccountLimitCooldown())
}

// TestGetRateLimit429AccountLimitCooldownFallsBackOnStoredGarbage 覆盖存量脏数据：
// 缺失/非 JSON/字段越界按字段回落到默认值，不报错也不放大取值。
func TestGetRateLimit429AccountLimitCooldownFallsBackOnStoredGarbage(t *testing.T) {
	for _, stored := range []string{"", "abc", "[]", "{}", `{"scope":"key","cooldown_seconds":0}`, `{"cooldown_seconds":7201}`} {
		svc, _ := newRateLimit429CooldownService(map[string]string{
			SettingKeyRateLimit429AccountLimitCooldown: stored,
		})

		cfg, err := svc.GetRateLimit429AccountLimitCooldown(context.Background())

		require.NoError(t, err, "stored=%q", stored)
		require.Equal(t, RateLimit429AccountLimitCooldown{
			Enabled:         false,
			Scope:           RateLimit429CooldownScopeSession,
			CooldownSeconds: 60,
		}, cfg, "stored=%q", stored)
	}
}

// TestGetRateLimit429AccountLimitCooldownSurfacesRepositoryErrors 读取失败必须上抛。
func TestGetRateLimit429AccountLimitCooldownSurfacesRepositoryErrors(t *testing.T) {
	svc, repo := newRateLimit429CooldownService(map[string]string{})
	repo.getErr = errors.New("boom")

	_, err := svc.GetRateLimit429AccountLimitCooldown(context.Background())

	require.ErrorContains(t, err, "429 account limit cooldown")
}

// TestSetRateLimit429AccountLimitWithCooldownWritesBothKeysOnce 覆盖验收标准 7：
// N 与新配置一次写入（单个 SetMultiple），读回一致。
func TestSetRateLimit429AccountLimitWithCooldownWritesBothKeysOnce(t *testing.T) {
	svc, repo := newRateLimit429CooldownService(map[string]string{})

	err := svc.SetRateLimit429AccountLimitWithCooldown(context.Background(), 5, &RateLimit429AccountLimitCooldown{
		Enabled:         true,
		Scope:           RateLimit429CooldownScopeDevice,
		CooldownSeconds: 120,
	})

	require.NoError(t, err)
	require.Len(t, repo.writeCalls, 1)
	require.Equal(t, map[string]string{
		SettingKeyRateLimit429AccountLimit:         "5",
		SettingKeyRateLimit429AccountLimitCooldown: `{"enabled":true,"scope":"device","cooldown_seconds":120}`,
	}, repo.writeCalls[0])

	limit, err := svc.GetRateLimit429AccountLimit(context.Background())
	require.NoError(t, err)
	require.Equal(t, 5, limit)
	cfg, err := svc.GetRateLimit429AccountLimitCooldown(context.Background())
	require.NoError(t, err)
	require.Equal(t, RateLimit429AccountLimitCooldown{
		Enabled:         true,
		Scope:           RateLimit429CooldownScopeDevice,
		CooldownSeconds: 120,
	}, cfg)
}

// TestSetRateLimit429AccountLimitWithCooldownNilKeepsCooldownKeyUntouched 覆盖向后兼容：
// 未提供新配置时只写 N，不重建/清空冷却键。
func TestSetRateLimit429AccountLimitWithCooldownNilKeepsCooldownKeyUntouched(t *testing.T) {
	stored := `{"enabled":true,"scope":"device","cooldown_seconds":120}`
	svc, repo := newRateLimit429CooldownService(map[string]string{
		SettingKeyRateLimit429AccountLimitCooldown: stored,
	})

	require.NoError(t, svc.SetRateLimit429AccountLimitWithCooldown(context.Background(), 3, nil))

	require.Len(t, repo.writeCalls, 1)
	require.Equal(t, map[string]string{SettingKeyRateLimit429AccountLimit: "3"}, repo.writeCalls[0])
	require.Equal(t, stored, repo.values[SettingKeyRateLimit429AccountLimitCooldown])
}

// TestSetRateLimit429AccountLimitWithCooldownRejectsInvalid 覆盖验收标准 1/7：
// 0/7201 秒即使开关关闭也拒绝，非法粒度拒绝，且拒绝时两个键都不落库。
func TestSetRateLimit429AccountLimitWithCooldownRejectsInvalid(t *testing.T) {
	cases := []struct {
		name     string
		limit    int
		cooldown RateLimit429AccountLimitCooldown
	}{
		{"seconds zero while disabled", 2, RateLimit429AccountLimitCooldown{Enabled: false, Scope: RateLimit429CooldownScopeSession, CooldownSeconds: 0}},
		{"seconds 7201", 2, RateLimit429AccountLimitCooldown{Enabled: true, Scope: RateLimit429CooldownScopeSession, CooldownSeconds: 7201}},
		{"seconds negative", 2, RateLimit429AccountLimitCooldown{Enabled: false, Scope: RateLimit429CooldownScopeDevice, CooldownSeconds: -1}},
		{"unknown scope", 2, RateLimit429AccountLimitCooldown{Enabled: true, Scope: "key", CooldownSeconds: 60}},
		{"empty scope", 2, RateLimit429AccountLimitCooldown{Enabled: true, Scope: "", CooldownSeconds: 60}},
		{"limit out of range", 101, RateLimit429AccountLimitCooldown{Enabled: true, Scope: RateLimit429CooldownScopeSession, CooldownSeconds: 60}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo := newRateLimit429CooldownService(map[string]string{})
			cooldown := tc.cooldown

			err := svc.SetRateLimit429AccountLimitWithCooldown(context.Background(), tc.limit, &cooldown)

			require.Error(t, err)
			require.Empty(t, repo.writeCalls)
		})
	}
}

// TestSetRateLimit429AccountLimitCooldownAcceptsBoundaries 覆盖秒数边界 1/7200 与两种粒度。
func TestSetRateLimit429AccountLimitCooldownAcceptsBoundaries(t *testing.T) {
	for _, scope := range []string{RateLimit429CooldownScopeSession, RateLimit429CooldownScopeDevice} {
		for _, seconds := range []int{1, 7200} {
			svc, repo := newRateLimit429CooldownService(map[string]string{})

			require.NoError(t, svc.SetRateLimit429AccountLimitCooldown(context.Background(), RateLimit429AccountLimitCooldown{
				Enabled:         true,
				Scope:           scope,
				CooldownSeconds: seconds,
			}), "scope=%s seconds=%d", scope, seconds)

			require.Len(t, repo.writeCalls, 1, "只写冷却键")
			_, wroteLimit := repo.writeCalls[0][SettingKeyRateLimit429AccountLimit]
			require.False(t, wroteLimit, "独立设置器不得改动 N")

			cfg, err := svc.GetRateLimit429AccountLimitCooldown(context.Background())
			require.NoError(t, err)
			require.Equal(t, RateLimit429AccountLimitCooldown{
				Enabled:         true,
				Scope:           scope,
				CooldownSeconds: seconds,
			}, cfg)
		}
	}
}
