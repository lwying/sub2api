//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGeminiV1BetaClaude429CooldownOnlyWithOriginalIdentity(t *testing.T) {
	group := newAntigravity429TestGroup(9310)
	upstream := &antigravity429Upstream{}
	h := newAntigravity429TestHandler(t, upstream, antigravity429TestAccounts(group.ID, 3), group)
	settings := newOpenAI429MatrixSettingRepo(2)
	require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, `{"enabled":true,"scope":"session","cooldown_seconds":60}`))
	h.settingService = service.NewSettingService(settings, h.cfg)
	store := newClaude429TestStore()
	h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, store)
	call := func(device, session, header string) (int, string) {
		c, rec := newAntigravity429TestContext(t, group, "generateContent")
		body := `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]`
		if device != "" {
			body += `,"metadata":{"user_id":"user_` + device + `_account__session_` + session + `"}`
		}
		body += `}`
		c.Request.Body = io.NopCloser(bytes.NewBufferString(body))
		c.Request.ContentLength = int64(len(body))
		c.Request.Header.Set("X-Claude-Code-Session-Id", header)
		h.GeminiV1BetaModels(c)
		return rec.Code, rec.Header().Get("Retry-After")
	}
	code, retry := call(claude429DeviceA, claude429SessionA, claude429SessionA)
	require.Equal(t, http.StatusTooManyRequests, code)
	require.Empty(t, retry)
	firstHits := len(upstream.hits)
	require.Positive(t, firstHits)
	code, retry = call(claude429DeviceA, claude429SessionA, claude429SessionA)
	require.Equal(t, http.StatusTooManyRequests, code)
	require.Equal(t, "60", retry)
	require.Len(t, upstream.hits, firstHits, "same inbound identity must make zero upstream attempts")
	_, retry = call(claude429DeviceB, claude429SessionB, claude429SessionB)
	require.Empty(t, retry)
	require.Greater(t, len(upstream.hits), firstHits, "B sharing key must not be locally cooled")
	hits := len(upstream.hits)
	_, retry = call("", claude429SessionA, claude429SessionA)
	require.Empty(t, retry, "plain Gemini without original identity must skip new cooldown")
	require.Greater(t, len(upstream.hits), hits)
}
