//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGatewayClaude429CooldownGeminiCountTokensDoesNotReadOrWrite(t *testing.T) {
	group := newAntigravity429TestGroup(9380)
	upstream := &antigravity429Upstream{}
	h := newAntigravity429TestHandler(t, upstream, antigravity429TestAccounts(group.ID, 3), group)
	settings := newOpenAI429MatrixSettingRepo(2)
	require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, `{"enabled":true,"scope":"session","cooldown_seconds":60}`))
	h.settingService = service.NewSettingService(settings, h.cfg)
	store := newClaude429TestStore()
	h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, store)
	for i := 0; i < 2; i++ {
		c, rec := newAntigravity429TestContext(t, group, "countTokens")
		body := `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"metadata":{"user_id":"user_` + claude429DeviceA + `_account__session_` + claude429SessionA + `"}}`
		c.Request.Body = io.NopCloser(bytes.NewBufferString(body))
		c.Request.ContentLength = int64(len(body))
		c.Request.Header.Set("X-Claude-Code-Session-Id", claude429SessionA)
		h.GeminiV1BetaModels(c)
		require.Empty(t, rec.Header().Get("Retry-After"))
	}
	require.Zero(t, store.reads, "token-counting action must not read the cooldown")
	require.Zero(t, store.writes, "token-counting action must not write the cooldown")
}

func TestGatewayClaude429CooldownResponsesInputTokensDoesNotReadOrWrite(t *testing.T) {
	group := &service.Group{ID: 9381, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	upstream := &gateway429AccountLimitUpstream{}
	h := newGateway429TestHandler(t, upstream, gateway429TestAccounts(group.ID, 3), group)
	settings := newOpenAI429MatrixSettingRepo(2)
	require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, `{"enabled":true,"scope":"session","cooldown_seconds":60}`))
	h.settingService = service.NewSettingService(settings, h.cfg)
	store := newClaude429TestStore()
	h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, store)
	body := `{"model":"claude-sonnet-4-5","input":"hello","stream":false,"metadata":{"user_id":"user_` + claude429DeviceA + `_account__session_` + claude429SessionA + `"}}`
	for i := 0; i < 2; i++ {
		c, rec, cancel := claude429BridgeContext(t, group, 9008, "/v1/responses/input_tokens", body, claude429SessionA, "turn")
		h.Responses(c)
		cancel()
		require.Empty(t, rec.Header().Get("Retry-After"))
	}
	require.Zero(t, store.reads)
	require.Zero(t, store.writes)
}
