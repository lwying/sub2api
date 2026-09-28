//go:build unit

package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestGatewayClaude429CooldownOfficialB1APIKeyShapes reproduces the relevant
// inbound wire properties of official B1's standard and passthrough API-key
// paths without using an external sample or credential. Both retain the
// client's metadata.user_id; standard syncs a conflicting session header to
// the body, while passthrough forwards the mismatch unchanged.
func TestGatewayClaude429CooldownOfficialB1APIKeyShapes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		passthrough   bool
		headerSession string
		wouldBlock    bool
	}{
		{"standard match", false, claude429SessionA, true},
		{"passthrough match", true, claude429SessionA, true},
		{"standard normalized mismatch", false, claude429SessionB, true},
		{"passthrough mismatch", true, claude429SessionB, false},
		{"standard missing header", false, "", false},
		{"passthrough missing header", true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := &service.Group{ID: 9390, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
			upstream := &gateway429AccountLimitUpstream{}
			h := newGateway429TestHandler(t, upstream, gateway429TestAccounts(group.ID, 3), group)
			settings := newOpenAI429MatrixSettingRepo(2)
			require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, `{"enabled":true,"scope":"session","cooldown_seconds":60}`))
			h.settingService = service.NewSettingService(settings, h.cfg)
			store := newClaude429TestStore()
			h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, store)
			actualHeader := tc.headerSession
			if !tc.passthrough && actualHeader != "" {
				actualHeader = claude429SessionA // Official B1 API-key standard syncs body session to header.
			}
			call := func(device, session, header, requestID string) (int, string) {
				c, rec := claude429TestContext(t, group, 9008, device, session, header, requestID)
				c.Request.Header.Set("User-Agent", claude429ModeClientUA)
				c.Request.Header.Set("X-App", "claude-code")
				c.Request.Header.Set("anthropic-beta", "message-batches-2024-09-24")
				c.Request.Header.Set("anthropic-version", "2023-06-01")
				h.Messages(c)
				return rec.Code, rec.Header().Get("Retry-After")
			}
			code, _ := call(claude429DeviceA, claude429SessionA, actualHeader, "first")
			require.Equal(t, http.StatusTooManyRequests, code)
			require.Equal(t, []int64{1, 2}, upstream.hits)
			_, retry := call(claude429DeviceA, claude429SessionA, actualHeader, "second")
			if tc.wouldBlock {
				require.Equal(t, "60", retry)
				require.Equal(t, []int64{1, 2}, upstream.hits)
			} else {
				require.Empty(t, retry)
				require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits)
				require.Zero(t, store.reads)
				require.Zero(t, store.writes)
			}
			_, retry = call(claude429DeviceB, claude429SessionB, claude429SessionB, "third")
			require.Empty(t, retry, "B on a shared B1 key must never hit A's local cooldown")
		})
	}
}
