//go:build unit

package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGatewayClaude429CooldownMessagesToggleAndTTL(t *testing.T) {
	group := &service.Group{ID: 9302, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	upstream := &gateway429AccountLimitUpstream{}
	h := newGateway429TestHandler(t, upstream, gateway429TestAccounts(group.ID, 3), group)
	settings := newOpenAI429MatrixSettingRepo(2)
	h.settingService = service.NewSettingService(settings, h.cfg)
	store := newClaude429TestStore()
	h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, store)
	ctx := context.Background()
	call := func(session string) (int, string) {
		c, rec := claude429TestContext(t, group, 9008, claude429DeviceA, session, session, "new-id")
		h.Messages(c)
		return rec.Code, rec.Header().Get("Retry-After")
	}

	// Default off: even with complete identity, original N is applied independently.
	for i := 0; i < 2; i++ {
		code, retry := call(claude429SessionA)
		require.Equal(t, http.StatusTooManyRequests, code)
		require.Empty(t, retry)
	}
	require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits)
	require.Zero(t, store.reads)
	require.Zero(t, store.writes)

	require.NoError(t, h.settingService.SetRateLimit429AccountLimitCooldown(ctx, service.RateLimit429AccountLimitCooldown{Enabled: true, Scope: service.RateLimit429CooldownScopeSession, CooldownSeconds: 60}))
	code, retry := call(claude429SessionA)
	require.Equal(t, http.StatusTooManyRequests, code)
	require.Empty(t, retry)
	require.Equal(t, 1, store.writes)
	_, retry = call(claude429SessionA)
	require.NotEmpty(t, retry)
	require.Equal(t, []int64{1, 2, 1, 2, 1, 2}, upstream.hits)

	// Turning off neither deletes nor refreshes an existing TTL.
	require.NoError(t, h.settingService.SetRateLimit429AccountLimitCooldown(ctx, service.RateLimit429AccountLimitCooldown{Enabled: false, Scope: service.RateLimit429CooldownScopeSession, CooldownSeconds: 60}))
	reads, writes := store.reads, store.writes
	_, retry = call(claude429SessionA)
	require.Empty(t, retry)
	require.Equal(t, reads, store.reads)
	require.Equal(t, writes, store.writes)
	require.Equal(t, []int64{1, 2, 1, 2, 1, 2, 1, 2}, upstream.hits)

	require.NoError(t, h.settingService.SetRateLimit429AccountLimitCooldown(ctx, service.RateLimit429AccountLimitCooldown{Enabled: true, Scope: service.RateLimit429CooldownScopeSession, CooldownSeconds: 60}))
	_, retry = call(claude429SessionA)
	require.NotEmpty(t, retry, "same-scope unexpired key resumes after re-enable")
	require.Len(t, upstream.hits, 8)

	// Old scope is ignored after a scope change; remaining seconds come from TTL.
	require.NoError(t, h.settingService.SetRateLimit429AccountLimitCooldown(ctx, service.RateLimit429AccountLimitCooldown{Enabled: true, Scope: service.RateLimit429CooldownScopeDevice, CooldownSeconds: 10}))
	_, retry = call(claude429SessionA)
	require.Empty(t, retry)
	require.Len(t, upstream.hits, 10)
	store.mu.Lock()
	for key := range store.entries {
		if store.entries[key].Before(time.Now().Add(20 * time.Second)) {
			store.entries[key] = time.Now().Add(3400 * time.Millisecond)
		}
	}
	store.mu.Unlock()
	_, retry = call(claude429SessionB)
	require.Equal(t, "4", retry, "device scope uses remaining TTL rounded upward, not configured seconds")
	require.Len(t, upstream.hits, 10)

	// Changing the configured duration must not refresh an existing key's TTL.
	require.NoError(t, h.settingService.SetRateLimit429AccountLimitCooldown(ctx, service.RateLimit429AccountLimitCooldown{Enabled: true, Scope: service.RateLimit429CooldownScopeDevice, CooldownSeconds: 7200}))
	_, retry = call(claude429SessionA)
	require.Equal(t, "4", retry)
	require.Len(t, upstream.hits, 10)

	// Once the prior key expires, the next N cap uses the newly configured duration.
	store.mu.Lock()
	for key := range store.entries {
		store.entries[key] = time.Now().Add(-time.Second)
	}
	store.mu.Unlock()
	_, retry = call(claude429SessionA)
	require.Empty(t, retry)
	require.Len(t, upstream.hits, 12)
	_, retry = call(claude429SessionB)
	require.Equal(t, "7200", retry)
	require.Len(t, upstream.hits, 12)
}
