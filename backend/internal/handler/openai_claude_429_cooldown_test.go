//go:build unit

package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIMessagesClaude429CooldownSameSessionAndSharedKey(t *testing.T) {
	upstream := &openAI429MatrixUpstream{statusByAccount: map[int64]int{}}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, 2)
	settings := newOpenAI429MatrixSettingRepo(2)
	require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, `{"enabled":true,"scope":"session","cooldown_seconds":60}`))
	store := newClaude429TestStore()
	env.handler.claude429Cooldown = service.NewClaude429CooldownGate(service.NewSettingService(settings, &config.Config{RunMode: config.RunModeSimple}), store)
	group := openAI429MatrixGroup(4293)
	group.AllowMessagesDispatch = true
	key := openAI429MatrixAPIKey(group)
	call := func(device, session, header string) (int, string) {
		body := []byte(`{"model":"gpt-5.1","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"user_` + device + `_account__session_` + session + `"}}`)
		c, rec := openAI429MatrixContext(t, key, "/v1/messages", body, nil)
		c.Request.Header.Set("X-Claude-Code-Session-Id", header)
		env.handler.Messages(c)
		return rec.Code, rec.Header().Get("Retry-After")
	}
	code, _ := call(claude429DeviceA, claude429SessionA, claude429SessionA)
	require.Equal(t, http.StatusTooManyRequests, code)
	require.Equal(t, []int64{1, 2}, upstream.calls())
	code, retry := call(claude429DeviceA, claude429SessionA, claude429SessionA)
	require.Equal(t, http.StatusTooManyRequests, code)
	require.Equal(t, "60", retry)
	require.Equal(t, []int64{1, 2}, upstream.calls(), "a second request in the same session must make zero upstream attempts")
	code, retry = call(claude429DeviceB, claude429SessionB, claude429SessionB)
	require.Equal(t, http.StatusTooManyRequests, code)
	require.Empty(t, retry)
	require.Equal(t, []int64{1, 2, 1, 2}, upstream.calls(), "a different device on the same key must not be locally cooled")
}
