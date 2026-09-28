//go:build unit

package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	claude429SessionA = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	claude429SessionB = "11111111-2222-4333-8444-555555555555"
	claude429DeviceA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	claude429DeviceB  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type claude429TestStore struct {
	mu       sync.Mutex
	entries  map[string]time.Time
	reads    int
	writes   int
	readErr  error
	writeErr error
}

func newClaude429TestStore() *claude429TestStore {
	return &claude429TestStore{entries: make(map[string]time.Time)}
}

func (s *claude429TestStore) SetClaude429Cooldown(_ context.Context, key string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.writeErr != nil {
		return s.writeErr
	}
	s.entries[key] = time.Now().Add(ttl)
	return nil
}

func (s *claude429TestStore) Claude429CooldownTTL(_ context.Context, key string) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.readErr != nil {
		return 0, s.readErr
	}
	if until := time.Until(s.entries[key]); until > 0 {
		return until, nil
	}
	return 0, nil
}

func claude429TestContext(t *testing.T, group *service.Group, keyID int64, deviceID, sessionID, headerSession, requestID string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	userID := "user_" + deviceID + "_account__session_" + sessionID
	body := `{"model":"claude-sonnet-4-5","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"` + userID + `"}}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", headerSession)
	req.Header.Set("X-Request-Id", requestID)
	c.Request = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	groupID := group.ID
	apiKey := &service.APIKey{ID: keyID, UserID: 10, GroupID: &groupID, Status: service.StatusActive, User: &service.User{ID: 10, Concurrency: 10, Balance: 100}, Group: group}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
	return c, rec
}

func TestGatewayClaude429CooldownMessagesSameSessionNewRequestID(t *testing.T) {
	group := &service.Group{ID: 9029, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	upstream := &gateway429AccountLimitUpstream{}
	h := newGateway429TestHandler(t, upstream, gateway429TestAccounts(group.ID, 3), group)
	settings := newOpenAI429MatrixSettingRepo(2)
	require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, `{"enabled":true,"scope":"session","cooldown_seconds":60}`))
	h.settingService = service.NewSettingService(settings, &config.Config{RunMode: config.RunModeSimple})
	store := newClaude429TestStore()
	h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, store)

	c1, rec1 := claude429TestContext(t, group, 9008, claude429DeviceA, claude429SessionA, claude429SessionA, "turn-1")
	h.Messages(c1)
	require.Equal(t, http.StatusTooManyRequests, rec1.Code)
	require.Equal(t, []int64{1, 2}, upstream.hits)

	c2, rec2 := claude429TestContext(t, group, 9008, claude429DeviceA, claude429SessionA, claude429SessionA, "turn-2")
	h.Messages(c2)
	require.Equal(t, http.StatusTooManyRequests, rec2.Code)
	require.Equal(t, []int64{1, 2}, upstream.hits, "a new request ID must not cause more upstream attempts in the cooled session")
	require.Equal(t, "60", rec2.Header().Get("Retry-After"))
	require.Contains(t, rec2.Body.String(), claude429CooldownCode)
	require.NotContains(t, rec2.Body.String(), claude429DeviceA)
	require.NotContains(t, rec2.Body.String(), claude429SessionA)

	c3, rec3 := claude429TestContext(t, group, 9008, claude429DeviceB, claude429SessionB, claude429SessionB, "turn-3")
	h.Messages(c3)
	require.Equal(t, http.StatusTooManyRequests, rec3.Code)
	require.Empty(t, rec3.Header().Get("Retry-After"))
	require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits, "a different device sharing the API key must not be locally cooled")
}

func TestGatewayClaude429CooldownMessagesScopesAndKeys(t *testing.T) {
	for _, tc := range []struct {
		name    string
		scope   string
		device  string
		session string
		keyID   int64
		blocked bool
	}{
		{"same session", "session", claude429DeviceA, claude429SessionA, 9008, true},
		{"new session", "session", claude429DeviceA, claude429SessionB, 9008, false},
		{"new session device scope", "device", claude429DeviceA, claude429SessionB, 9008, true},
		{"different device", "device", claude429DeviceB, claude429SessionB, 9008, false},
		{"different authenticated key", "session", claude429DeviceA, claude429SessionA, 9009, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := &service.Group{ID: 9030, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
			upstream := &gateway429AccountLimitUpstream{}
			h := newGateway429TestHandler(t, upstream, gateway429TestAccounts(group.ID, 3), group)
			settings := newOpenAI429MatrixSettingRepo(2)
			require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, `{"enabled":true,"scope":"`+tc.scope+`","cooldown_seconds":60}`))
			h.settingService = service.NewSettingService(settings, h.cfg)
			store := newClaude429TestStore()
			h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, store)
			c1, _ := claude429TestContext(t, group, 9008, claude429DeviceA, claude429SessionA, claude429SessionA, "first")
			h.Messages(c1)
			require.Equal(t, []int64{1, 2}, upstream.hits)
			c2, rec2 := claude429TestContext(t, group, tc.keyID, tc.device, tc.session, tc.session, "second")
			h.Messages(c2)
			require.Equal(t, http.StatusTooManyRequests, rec2.Code)
			if tc.blocked {
				require.Equal(t, []int64{1, 2}, upstream.hits)
				require.NotEmpty(t, rec2.Header().Get("Retry-After"))
			} else {
				require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits)
				require.Empty(t, rec2.Header().Get("Retry-After"))
			}
		})
	}
}

func TestGatewayClaude429CooldownMessagesDuplicateSessionHeadersSkip(t *testing.T) {
	group := &service.Group{ID: 9032, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	upstream := &gateway429AccountLimitUpstream{}
	h := newGateway429TestHandler(t, upstream, gateway429TestAccounts(group.ID, 3), group)
	settings := newOpenAI429MatrixSettingRepo(2)
	require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, `{"enabled":true,"scope":"session","cooldown_seconds":60}`))
	h.settingService = service.NewSettingService(settings, h.cfg)
	store := newClaude429TestStore()
	h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, store)
	for i := 0; i < 2; i++ {
		c, rec := claude429TestContext(t, group, 9008, claude429DeviceA, claude429SessionA, claude429SessionA, strconv.Itoa(i))
		c.Request.Header.Add("X-Claude-Code-Session-Id", claude429SessionB)
		h.Messages(c)
		require.Equal(t, http.StatusTooManyRequests, rec.Code)
	}
	require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits)
	require.Zero(t, store.reads)
	require.Zero(t, store.writes)
}

func TestGatewayClaude429CooldownMessagesIdentityAndStorageFailOpen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		device  string
		session string
		header  string
		broken  bool
	}{
		{"missing metadata", "", claude429SessionA, claude429SessionA, false},
		{"malformed metadata", "invalid-device", claude429SessionA, claude429SessionA, false},
		{"missing header", claude429DeviceA, claude429SessionA, "", false},
		{"conflicting header", claude429DeviceA, claude429SessionA, claude429SessionB, false},
		{"whitespace header", claude429DeviceA, claude429SessionA, " " + claude429SessionA, false},
		{"redis read error", claude429DeviceA, claude429SessionA, claude429SessionA, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := &service.Group{ID: 9031, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
			upstream := &gateway429AccountLimitUpstream{}
			h := newGateway429TestHandler(t, upstream, gateway429TestAccounts(group.ID, 3), group)
			settings := newOpenAI429MatrixSettingRepo(2)
			require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, `{"enabled":true,"scope":"session","cooldown_seconds":60}`))
			h.settingService = service.NewSettingService(settings, h.cfg)
			store := newClaude429TestStore()
			h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, store)
			if tc.broken {
				store.readErr = errors.New("synthetic redis unavailable")
			}
			for i := 0; i < 2; i++ {
				c, rec := claude429TestContext(t, group, 9008, tc.device, tc.session, tc.header, strconv.Itoa(i))
				h.Messages(c)
				require.Equal(t, http.StatusTooManyRequests, rec.Code)
			}
			require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits, "invalid identity or Redis failure must keep N but skip new cooldown")
			if !tc.broken {
				require.Zero(t, store.writes)
				require.Zero(t, store.reads)
			}
		})
	}
}
