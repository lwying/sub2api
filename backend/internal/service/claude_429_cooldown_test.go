package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// claude429CooldownSettingRepo 是只暴露 GetValue 的设置仓储桩：读取失败可用 err 注入，
// 其余方法 panic，避免测试意外依赖未覆盖的设置读写。
type claude429CooldownSettingRepo struct {
	values map[string]string
	err    error
}

func (r *claude429CooldownSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (r *claude429CooldownSettingRepo) Get(_ context.Context, _ string) (*Setting, error) {
	panic("claude429CooldownSettingRepo.Get not implemented")
}
func (r *claude429CooldownSettingRepo) Set(_ context.Context, _, _ string) error {
	panic("claude429CooldownSettingRepo.Set not implemented")
}
func (r *claude429CooldownSettingRepo) GetMultiple(_ context.Context, _ []string) (map[string]string, error) {
	panic("claude429CooldownSettingRepo.GetMultiple not implemented")
}
func (r *claude429CooldownSettingRepo) SetMultiple(_ context.Context, _ map[string]string) error {
	panic("claude429CooldownSettingRepo.SetMultiple not implemented")
}
func (r *claude429CooldownSettingRepo) GetAll(_ context.Context) (map[string]string, error) {
	panic("claude429CooldownSettingRepo.GetAll not implemented")
}
func (r *claude429CooldownSettingRepo) Delete(_ context.Context, _ string) error {
	panic("claude429CooldownSettingRepo.Delete not implemented")
}

var _ SettingRepository = (*claude429CooldownSettingRepo)(nil)

// fakeClaude429CooldownStore 记录读写并可注入故障，用于验证 fail-open 与键的稳定性。
type fakeClaude429CooldownStore struct {
	mu      sync.Mutex
	ttls    map[string]time.Duration
	writes  []string
	reads   []string
	setErr  error
	readErr error
}

func newFakeClaude429CooldownStore() *fakeClaude429CooldownStore {
	return &fakeClaude429CooldownStore{ttls: map[string]time.Duration{}}
}

func (s *fakeClaude429CooldownStore) SetClaude429Cooldown(_ context.Context, key string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return s.setErr
	}
	s.writes = append(s.writes, key)
	s.ttls[key] = ttl
	return nil
}

func (s *fakeClaude429CooldownStore) Claude429CooldownTTL(_ context.Context, key string) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, key)
	if s.readErr != nil {
		return 0, s.readErr
	}
	return s.ttls[key], nil
}

func (s *fakeClaude429CooldownStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

func (s *fakeClaude429CooldownStore) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reads)
}

func (s *fakeClaude429CooldownStore) lastWrite() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.writes) == 0 {
		return ""
	}
	return s.writes[len(s.writes)-1]
}

func newClaude429CooldownTestGate(cooldownJSON string, store Claude429CooldownStore) *Claude429CooldownGate {
	values := map[string]string{}
	if cooldownJSON != "" {
		values[SettingKeyRateLimit429AccountLimitCooldown] = cooldownJSON
	}
	return NewClaude429CooldownGate(&SettingService{settingRepo: &claude429CooldownSettingRepo{values: values}}, store)
}

func enabledClaude429Cooldown(scope string, seconds int) RateLimit429AccountLimitCooldown {
	return RateLimit429AccountLimitCooldown{Enabled: true, Scope: scope, CooldownSeconds: seconds}
}

func TestClaude429CooldownRuntimeDefaultsToDisabledAndFailsOpen(t *testing.T) {
	ctx := context.Background()
	store := newFakeClaude429CooldownStore()

	var nilGate *Claude429CooldownGate
	defaults := nilGate.Runtime(ctx)
	require.False(t, defaults.Enabled)
	require.Equal(t, RateLimit429CooldownScopeSession, defaults.Scope)
	require.Equal(t, 60, defaults.CooldownSeconds)
	remaining, cooled := nilGate.Remaining(ctx, 1, enabledClaude429Cooldown(RateLimit429CooldownScopeSession, 60), "dev", "sess")
	require.Zero(t, remaining)
	require.False(t, cooled)
	require.NotPanics(t, func() { nilGate.Mark(ctx, 1, defaults, "dev", "sess") })

	noSettings := NewClaude429CooldownGate(nil, store)
	require.False(t, noSettings.Runtime(ctx).Enabled)

	// 设置键缺失：回落到默认（关闭），且不读不写存储。
	missing := newClaude429CooldownTestGate("", store)
	rt := missing.Runtime(ctx)
	require.False(t, rt.Enabled)
	remaining, cooled = missing.Remaining(ctx, 9008, rt, "dev", "sess")
	require.Zero(t, remaining)
	require.False(t, cooled)
	missing.Mark(ctx, 9008, rt, "dev", "sess")
	require.Zero(t, store.writeCount())
	require.Zero(t, store.readCount())
}

func TestClaude429CooldownRuntimeReadsConfiguredScopeAndSeconds(t *testing.T) {
	ctx := context.Background()
	gate := newClaude429CooldownTestGate(`{"enabled":true,"scope":"device","cooldown_seconds":90}`, newFakeClaude429CooldownStore())

	rt := gate.Runtime(ctx)
	require.True(t, rt.Enabled)
	require.Equal(t, RateLimit429CooldownScopeDevice, rt.Scope)
	require.Equal(t, 90, rt.CooldownSeconds)

	// 读取失败：仍返回默认（关闭），不把错误暴露给热路径。
	failing := NewClaude429CooldownGate(&SettingService{settingRepo: &claude429CooldownSettingRepo{err: errors.New("setting db down")}}, newFakeClaude429CooldownStore())
	failedRT := failing.Runtime(ctx)
	require.False(t, failedRT.Enabled)
	remaining, cooled := failing.Remaining(ctx, 1, failedRT, "dev", "sess")
	require.Zero(t, remaining)
	require.False(t, cooled)
}

func TestClaude429CooldownSessionScopeKeysDeviceAndSession(t *testing.T) {
	ctx := context.Background()
	store := newFakeClaude429CooldownStore()
	gate := newClaude429CooldownTestGate(`{"enabled":true,"scope":"session","cooldown_seconds":60}`, store)
	rt := gate.Runtime(ctx)
	require.Equal(t, RateLimit429CooldownScopeSession, rt.Scope)

	gate.Mark(ctx, 9008, rt, "device-alpha", "session-alpha")
	require.Equal(t, 1, store.writeCount())

	remaining, cooled := gate.Remaining(ctx, 9008, rt, "device-alpha", "session-alpha")
	require.True(t, cooled)
	require.Equal(t, 60, remaining)

	// 同一设备新开会话：会话级不命中（允许 A 新会话）。
	remaining, cooled = gate.Remaining(ctx, 9008, rt, "device-alpha", "session-beta")
	require.Zero(t, remaining)
	require.False(t, cooled)

	// 同一把 Key 下的其他设备：不命中（B 不被误冷却）。
	remaining, cooled = gate.Remaining(ctx, 9008, rt, "device-beta", "session-alpha")
	require.Zero(t, remaining)
	require.False(t, cooled)

	// 另一把 B1 Key 的相同身份：不命中（Key 间隔离）。
	remaining, cooled = gate.Remaining(ctx, 9009, rt, "device-alpha", "session-alpha")
	require.Zero(t, remaining)
	require.False(t, cooled)
}

func TestClaude429CooldownDeviceScopeSurvivesNewSession(t *testing.T) {
	ctx := context.Background()
	store := newFakeClaude429CooldownStore()
	gate := newClaude429CooldownTestGate(`{"enabled":true,"scope":"device","cooldown_seconds":30}`, store)
	rt := gate.Runtime(ctx)

	gate.Mark(ctx, 9008, rt, "device-alpha", "session-alpha")
	remaining, cooled := gate.Remaining(ctx, 9008, rt, "device-alpha", "session-beta")
	require.True(t, cooled, "device scope must still cool the device when a new session starts")
	require.Equal(t, 30, remaining)

	remaining, cooled = gate.Remaining(ctx, 9008, rt, "device-beta", "session-alpha")
	require.Zero(t, remaining)
	require.False(t, cooled)
}

func TestClaude429CooldownSkipsIncompleteInboundIdentity(t *testing.T) {
	ctx := context.Background()
	store := newFakeClaude429CooldownStore()
	gate := newClaude429CooldownTestGate(`{"enabled":true,"scope":"session","cooldown_seconds":60}`, store)
	rt := gate.Runtime(ctx)

	gate.Mark(ctx, 9008, rt, "", "session-alpha")
	gate.Mark(ctx, 9008, rt, "   ", "session-alpha")
	gate.Mark(ctx, 9008, rt, "device-alpha", "")
	gate.Mark(ctx, 9008, rt, "device-alpha", "   ")
	gate.Mark(ctx, 0, rt, "device-alpha", "session-alpha")
	gate.Mark(ctx, -1, rt, "device-alpha", "session-alpha")
	require.Zero(t, store.writeCount(), "incomplete inbound identity must skip the new cooldown, never widen the key")

	remaining, cooled := gate.Remaining(ctx, 9008, rt, "device-alpha", "session-alpha")
	require.Zero(t, remaining)
	require.False(t, cooled)

	// 设备级也必须先有完整会话身份：不退化成仅 Key/仅 device 的宽范围键。
	writesBefore, readsBefore := store.writeCount(), store.readCount()
	deviceGate := newClaude429CooldownTestGate(`{"enabled":true,"scope":"device","cooldown_seconds":60}`, store)
	deviceRT := deviceGate.Runtime(ctx)
	deviceGate.Mark(ctx, 9008, deviceRT, "device-alpha", "")
	remaining, cooled = deviceGate.Remaining(ctx, 9008, deviceRT, "device-alpha", "")
	require.Zero(t, remaining)
	require.False(t, cooled)
	require.Equal(t, writesBefore, store.writeCount())
	require.Equal(t, readsBefore, store.readCount())
}

func TestClaude429CooldownDisabledNeverTouchesStore(t *testing.T) {
	ctx := context.Background()
	store := newFakeClaude429CooldownStore()
	store.readErr = errors.New("redis down")
	store.setErr = errors.New("redis down")
	gate := newClaude429CooldownTestGate("", store)

	rt := gate.Runtime(ctx)
	require.False(t, rt.Enabled)
	require.NotPanics(t, func() { gate.Mark(ctx, 9008, rt, "device-alpha", "session-alpha") })
	remaining, cooled := gate.Remaining(ctx, 9008, rt, "device-alpha", "session-alpha")
	require.Zero(t, remaining)
	require.False(t, cooled)
	require.Zero(t, store.writeCount())
	require.Zero(t, store.readCount(), "a disabled gate must not even read the store")

	// 即便调用方硬塞一个 Enabled=true 的手工配置，越界秒数也必须跳过而不是写坏键。
	manual := RateLimit429AccountLimitCooldown{Enabled: true, Scope: "session", CooldownSeconds: 0}
	gate.Mark(ctx, 9008, manual, "device-alpha", "session-alpha")
	require.Zero(t, store.writeCount())
}

func TestClaude429CooldownStoreFailuresFailOpen(t *testing.T) {
	ctx := context.Background()
	store := newFakeClaude429CooldownStore()
	gate := newClaude429CooldownTestGate(`{"enabled":true,"scope":"session","cooldown_seconds":60}`, store)
	rt := gate.Runtime(ctx)

	store.readErr = errors.New("redis read down")
	remaining, cooled := gate.Remaining(ctx, 9008, rt, "device-alpha", "session-alpha")
	require.Zero(t, remaining)
	require.False(t, cooled, "storage failure must not block the request")

	store.readErr = nil
	store.setErr = errors.New("redis write down")
	require.NotPanics(t, func() { gate.Mark(ctx, 9008, rt, "device-alpha", "session-alpha") })
	require.Zero(t, store.writeCount())

	store.setErr = nil
	gate.Mark(ctx, 9008, rt, "device-alpha", "session-alpha")
	require.Equal(t, 1, store.writeCount())
	require.Equal(t, 60*time.Second, store.ttls[store.lastWrite()])
	remaining, cooled = gate.Remaining(ctx, 9008, rt, "device-alpha", "session-alpha")
	require.True(t, cooled)
	require.Equal(t, 60, remaining)
}

func TestClaude429CooldownKeyHidesRawIdentityAndSeparatesScopes(t *testing.T) {
	ctx := context.Background()
	store := newFakeClaude429CooldownStore()
	const device = "device-alpha-sentinel"
	const session = "session-beta-sentinel"

	sessionGate := newClaude429CooldownTestGate(`{"enabled":true,"scope":"session","cooldown_seconds":60}`, store)
	sessionRT := sessionGate.Runtime(ctx)
	sessionGate.Mark(ctx, 9008, sessionRT, device, session)
	sessionKey := store.lastWrite()

	sessionGate.Mark(ctx, 9008, sessionRT, device, session)
	require.Equal(t, sessionKey, store.lastWrite(), "the digest must be stable for the same identity")

	require.Len(t, sessionKey, 64)
	require.NotContains(t, sessionKey, device)
	require.NotContains(t, sessionKey, session)
	require.NotContains(t, sessionKey, strconv.FormatInt(9008, 10))
	require.Equal(t, sessionKey, strings.ToLower(sessionKey))
	require.NotContains(t, sessionKey, ":")

	deviceGate := newClaude429CooldownTestGate(`{"enabled":true,"scope":"device","cooldown_seconds":60}`, store)
	deviceRT := deviceGate.Runtime(ctx)

	// 切换粒度后旧模式的键不匹配（自然过期）：已写入的会话级键绝不能命中设备级查询。
	remaining, cooled := deviceGate.Remaining(ctx, 9008, deviceRT, device, session)
	require.Zero(t, remaining)
	require.False(t, cooled, "a session-scope key must never satisfy a device-scope lookup")

	deviceGate.Mark(ctx, 9008, deviceRT, device, session)
	deviceKey := store.lastWrite()
	require.NotEqual(t, sessionKey, deviceKey, "the scope must be domain-separated in the digest")
	require.NotEqual(t, deviceKey, claude429CooldownDigest(9008, RateLimit429CooldownScopeSession, device, session))

	// 设备级记录在新会话下仍命中；会话级记录则不命中新会话。
	remaining, cooled = deviceGate.Remaining(ctx, 9008, deviceRT, device, "session-gamma")
	require.True(t, cooled)
	require.Equal(t, 60, remaining)

	remaining, cooled = sessionGate.Remaining(ctx, 9008, sessionRT, device, "session-gamma")
	require.Zero(t, remaining)
	require.False(t, cooled)
}

// TestClaude429CooldownKeyIsNotAmbiguousAcrossIdentityFieldSplits 锁定键编码的回归：
// 设备与会话都是客户端自报的任意字符串，(device="x", session="y|session=z") 与
// (device="x|session=y", session="z") 在朴素拼接下会得到同一原文（见下方前置断言），
// 从而让一个身份命中另一个身份的冷却。字段长度前缀必须让两者得到不同的键。
func TestClaude429CooldownKeyIsNotAmbiguousAcrossIdentityFieldSplits(t *testing.T) {
	ctx := context.Background()
	store := newFakeClaude429CooldownStore()
	gate := newClaude429CooldownTestGate(`{"enabled":true,"scope":"session","cooldown_seconds":60}`, store)
	rt := gate.Runtime(ctx)

	const deviceOne, sessionOne = "x", "y|session=z"
	const deviceTwo, sessionTwo = "x|session=y", "z"

	// 前置条件：朴素分隔符拼接对这两组不同身份产生完全相同的原文，
	// 因此任何"直接拼接后哈希"的实现都会把两者映射到同一个键。
	naive := func(device, session string) string { return "device=" + device + "|session=" + session }
	require.Equal(t, naive(deviceOne, sessionOne), naive(deviceTwo, sessionTwo))

	first := claude429CooldownDigest(9008, rt.Scope, deviceOne, sessionOne)
	second := claude429CooldownDigest(9008, rt.Scope, deviceTwo, sessionTwo)
	require.NotEqual(t, first, second, "distinct inbound identities must never share a cooldown key")

	gate.Mark(ctx, 9008, rt, deviceOne, sessionOne)
	remaining, cooled := gate.Remaining(ctx, 9008, rt, deviceOne, sessionOne)
	require.True(t, cooled)
	require.Equal(t, 60, remaining)

	remaining, cooled = gate.Remaining(ctx, 9008, rt, deviceTwo, sessionTwo)
	require.Zero(t, remaining)
	require.False(t, cooled, "a split identity must not inherit the other identity's cooldown")
}

// TestClaude429CooldownIdentityIsNotNormalized 锁定"拒绝而不归一化"：
// 首尾空白/控制字符的标识整条跳过（不读不写），绝不被修剪后与干净标识共用键。
func TestClaude429CooldownIdentityIsNotNormalized(t *testing.T) {
	ctx := context.Background()
	store := newFakeClaude429CooldownStore()
	gate := newClaude429CooldownTestGate(`{"enabled":true,"scope":"session","cooldown_seconds":60}`, store)
	rt := gate.Runtime(ctx)

	gate.Mark(ctx, 9008, rt, "device-alpha", "session-alpha")
	require.Equal(t, 1, store.writeCount())
	writesBefore, readsBefore := store.writeCount(), store.readCount()

	for _, identity := range []struct{ device, session string }{
		{" device-alpha", "session-alpha"},
		{"device-alpha ", "session-alpha"},
		{"device-alpha", " session-alpha"},
		{"device-alpha", "session-alpha\n"},
		{"device-\x00alpha", "session-alpha"},
		{"device-alpha", "session-\x1falpha"},
	} {
		gate.Mark(ctx, 9008, rt, identity.device, identity.session)
		remaining, cooled := gate.Remaining(ctx, 9008, rt, identity.device, identity.session)
		require.Zero(t, remaining)
		require.False(t, cooled)
	}
	require.Equal(t, writesBefore, store.writeCount(), "invalid identities must not be normalized into a shared key")
	require.Equal(t, readsBefore, store.readCount())
}

func TestClaude429CooldownRemainingRoundsUpPartialSeconds(t *testing.T) {
	ctx := context.Background()
	store := newFakeClaude429CooldownStore()
	gate := newClaude429CooldownTestGate(`{"enabled":true,"scope":"session","cooldown_seconds":60}`, store)
	rt := gate.Runtime(ctx)

	key := claude429CooldownDigest(9008, rt.Scope, "device-alpha", "session-alpha")
	store.ttls[key] = 1500 * time.Millisecond
	remaining, cooled := gate.Remaining(ctx, 9008, rt, "device-alpha", "session-alpha")
	require.True(t, cooled)
	require.Equal(t, 2, remaining, "a partial second must round up so Retry-After never says 0")

	store.ttls[key] = 200 * time.Millisecond
	remaining, cooled = gate.Remaining(ctx, 9008, rt, "device-alpha", "session-alpha")
	require.True(t, cooled)
	require.Equal(t, 1, remaining)

	store.ttls[key] = 0
	remaining, cooled = gate.Remaining(ctx, 9008, rt, "device-alpha", "session-alpha")
	require.Zero(t, remaining)
	require.False(t, cooled)

	store.ttls[key] = -time.Second
	remaining, cooled = gate.Remaining(ctx, 9008, rt, "device-alpha", "session-alpha")
	require.Zero(t, remaining)
	require.False(t, cooled)
}

func TestClaude429CooldownStoreFromCacheRequiresOptionalCapability(t *testing.T) {
	require.Nil(t, Claude429CooldownStoreFromCache(nil))

	var plainCache fakeClaude429CooldownPlainCache
	require.Nil(t, Claude429CooldownStoreFromCache(plainCache), "a cache without the optional capability must degrade to nil")

	store := newFakeClaude429CooldownStore()
	withStore := &fakeClaude429CooldownCacheWithStore{GatewayCache: plainCache, store: store}
	got := Claude429CooldownStoreFromCache(withStore)
	require.Equal(t, Claude429CooldownStore(withStore), got)

	// 探测到的存储确实可用：写入门禁后能读回剩余冷却。
	gate := newClaude429CooldownTestGate(`{"enabled":true,"scope":"session","cooldown_seconds":60}`, got)
	rt := gate.Runtime(context.Background())
	gate.Mark(context.Background(), 9008, rt, "device-alpha", "session-alpha")
	require.Equal(t, 1, store.writeCount())
}

// fakeClaude429CooldownPlainCache 只实现共享 GatewayCache，不实现可选的冷却存储能力。
type fakeClaude429CooldownPlainCache struct{ GatewayCache }

// fakeClaude429CooldownCacheWithStore 同时实现共享接口与可选能力，模拟 gatewayCache。
type fakeClaude429CooldownCacheWithStore struct {
	GatewayCache
	store Claude429CooldownStore
}

func (c *fakeClaude429CooldownCacheWithStore) SetClaude429Cooldown(ctx context.Context, key string, ttl time.Duration) error {
	return c.store.SetClaude429Cooldown(ctx, key, ttl)
}

func (c *fakeClaude429CooldownCacheWithStore) Claude429CooldownTTL(ctx context.Context, key string) (time.Duration, error) {
	return c.store.Claude429CooldownTTL(ctx, key)
}
