package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// gatewayCacheClaude429CooldownStore 通过类型断言接入可选能力：不修改共享
// GatewayCache 接口，未实现该能力的实现（测试 stub / 装饰器）自动降级。
func newGatewayCacheClaude429CooldownStore(t *testing.T) (*miniredis.Miniredis, service.Claude429CooldownStore) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, ok := NewGatewayCache(client).(service.Claude429CooldownStore)
	require.True(t, ok, "gatewayCache must implement the optional Claude 429 cooldown store")
	return server, store
}

// claude429CooldownCommandRecorder 记录真实下发的 Redis 命令，用于断言
// "写值与 TTL 同一条 SET"（不是 SET + EXPIRE 两条命令）。
type claude429CooldownCommandRecorder struct {
	mu    sync.Mutex
	names []string
}

// tracked 只记录本功能关心的键操作，跳过 go-redis 建立连接时的握手命令
// （hello/client setinfo），否则它们会被误当作额外命令。
var claude429CooldownTrackedCommands = map[string]bool{
	"set": true, "setex": true, "psetex": true,
	"expire": true, "pexpire": true,
	"get": true, "pttl": true, "del": true,
}

func (h *claude429CooldownCommandRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *claude429CooldownCommandRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if claude429CooldownTrackedCommands[cmd.Name()] {
			h.mu.Lock()
			h.names = append(h.names, cmd.Name())
			h.mu.Unlock()
		}
		return next(ctx, cmd)
	}
}

func (h *claude429CooldownCommandRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.mu.Lock()
		for _, cmd := range cmds {
			if claude429CooldownTrackedCommands[cmd.Name()] {
				h.names = append(h.names, cmd.Name())
			}
		}
		h.mu.Unlock()
		return next(ctx, cmds)
	}
}

func (h *claude429CooldownCommandRecorder) recorded() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.names...)
}

func TestGatewayCacheClaude429CooldownWriteUsesAtomicSetAndReportsRemainingTTL(t *testing.T) {
	server, store := newGatewayCacheClaude429CooldownStore(t)
	ctx := context.Background()

	require.NoError(t, store.SetClaude429Cooldown(ctx, "digest-a", 90*time.Second))
	require.Equal(t, 90*time.Second, server.TTL(claude429CooldownPrefix+"digest-a"))

	ttl, err := store.Claude429CooldownTTL(ctx, "digest-a")
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0))
	require.LessOrEqual(t, ttl, 90*time.Second)
}

func TestGatewayCacheClaude429CooldownSetCarriesTTLInOneCommand(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	recorder := &claude429CooldownCommandRecorder{}
	client.AddHook(recorder)
	store, ok := NewGatewayCache(client).(service.Claude429CooldownStore)
	require.True(t, ok)

	require.NoError(t, store.SetClaude429Cooldown(context.Background(), "digest-atomic", time.Minute))

	names := recorder.recorded()
	require.Equal(t, []string{"set"}, names, "write must be a single SET carrying the TTL")
	require.Equal(t, time.Minute, server.TTL(claude429CooldownPrefix+"digest-atomic"))
}

func TestGatewayCacheClaude429CooldownMissAndExpiryReportNoCooldown(t *testing.T) {
	server, store := newGatewayCacheClaude429CooldownStore(t)
	ctx := context.Background()

	ttl, err := store.Claude429CooldownTTL(ctx, "missing-digest")
	require.NoError(t, err)
	require.Zero(t, ttl, "a missing key is a miss, not an error")

	require.NoError(t, store.SetClaude429Cooldown(ctx, "digest-expiring", 30*time.Second))
	server.FastForward(31 * time.Second)
	ttl, err = store.Claude429CooldownTTL(ctx, "digest-expiring")
	require.NoError(t, err)
	require.Zero(t, ttl, "an expired key must report no remaining cooldown")
}

func TestGatewayCacheClaude429CooldownRejectsInvalidInput(t *testing.T) {
	_, store := newGatewayCacheClaude429CooldownStore(t)
	ctx := context.Background()

	require.Error(t, store.SetClaude429Cooldown(ctx, "   ", time.Minute))
	require.Error(t, store.SetClaude429Cooldown(ctx, "digest-c", 0))
	require.Error(t, store.SetClaude429Cooldown(ctx, "digest-c", -time.Second))
	_, err := store.Claude429CooldownTTL(ctx, "   ")
	require.Error(t, err)
	require.NoError(t, store.SetClaude429Cooldown(ctx, "digest-c", time.Minute))
}

func TestGatewayCacheClaude429CooldownUnavailableRedisFailsFast(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	store, ok := NewGatewayCache(client).(service.Claude429CooldownStore)
	require.True(t, ok)
	server.Close()

	require.Error(t, store.SetClaude429Cooldown(context.Background(), "digest-e", time.Minute))
	_, err := store.Claude429CooldownTTL(context.Background(), "digest-e")
	require.Error(t, err)
}

func TestGatewayCacheClaude429CooldownKeyContainsOnlyPrefixAndOpaqueDigest(t *testing.T) {
	server, store := newGatewayCacheClaude429CooldownStore(t)
	const digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

	require.NoError(t, store.SetClaude429Cooldown(context.Background(), digest, time.Minute))

	keys := server.Keys()
	require.Equal(t, []string{claude429CooldownPrefix + digest}, keys)
	require.NotContains(t, keys[0], "api_key")
	require.NotContains(t, keys[0], "device")
	require.NotContains(t, keys[0], "session")
}
