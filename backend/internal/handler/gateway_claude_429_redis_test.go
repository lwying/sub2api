//go:build unit

package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// TestGatewayClaude429CooldownMessagesSharedRedisAcrossNodes drives two real
// Messages handlers against the same Redis-backed TTL store. The second B2
// instance must observe A's cooldown without sharing an in-process counter.
func TestGatewayClaude429CooldownMessagesSharedRedisAcrossNodes(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, ok := repository.NewGatewayCache(client).(service.Claude429CooldownStore)
	require.True(t, ok)
	group := &service.Group{ID: 9391, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	upstream := &gateway429AccountLimitUpstream{}
	settings := newOpenAI429MatrixSettingRepo(2)
	require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, `{"enabled":true,"scope":"session","cooldown_seconds":60}`))
	makeHandler := func() *GatewayHandler {
		h := newGateway429TestHandler(t, upstream, gateway429TestAccounts(group.ID, 3), group)
		h.settingService = service.NewSettingService(settings, h.cfg)
		h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, store)
		return h
	}
	a, b := makeHandler(), makeHandler()

	c1, rec1 := claude429TestContext(t, group, 9008, claude429DeviceA, claude429SessionA, claude429SessionA, "request-one")
	a.Messages(c1)
	require.Equal(t, http.StatusTooManyRequests, rec1.Code)
	require.Equal(t, []int64{1, 2}, upstream.hits)

	c2, rec2 := claude429TestContext(t, group, 9008, claude429DeviceA, claude429SessionA, claude429SessionA, "request-two")
	b.Messages(c2)
	require.Equal(t, http.StatusTooManyRequests, rec2.Code)
	require.Equal(t, "60", rec2.Header().Get("Retry-After"))
	require.Equal(t, []int64{1, 2}, upstream.hits, "second node must reject before upstream attempt")

	c3, rec3 := claude429TestContext(t, group, 9008, claude429DeviceB, claude429SessionB, claude429SessionB, "request-three")
	b.Messages(c3)
	require.Equal(t, http.StatusTooManyRequests, rec3.Code)
	require.Empty(t, rec3.Header().Get("Retry-After"), "same key, different device not locally blocked")
	require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits)

	server.FastForward(61 * time.Second)
	c4, rec4 := claude429TestContext(t, group, 9008, claude429DeviceA, claude429SessionA, claude429SessionA, "request-four")
	b.Messages(c4)
	require.Equal(t, http.StatusTooManyRequests, rec4.Code)
	require.Empty(t, rec4.Header().Get("Retry-After"), "expired key must release A")
	require.Equal(t, []int64{1, 2, 1, 2, 1, 2}, upstream.hits)
}
