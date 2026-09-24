//go:build unit

package handler

// Ticket 01 — Anthropic Messages 主入口（/v1/messages）的请求内 429 账号上限验收。
//
// 本文件的既有用例只覆盖默认上限 2 的行为；这里补上"管理员改配置值后入口随之改变"
// 这一最高外部行为接缝：上游实际被调用的账号序列与客户端状态码，
// 不直接调用内部计数结构。设置仓储复用 Ticket 02 建立、且 Antigravity/Grok 入口
// 已在用的 newOpenAI429MatrixSettingRepo（见 openai_429_account_limit_matrix_test.go），
// 不再另建重复 stub。

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type gateway429AccountLimitUpstream struct {
	service.HTTPUpstream
	hits []int64
}

type gateway429NoCooldownRepo struct {
	openAIImagesFailoverAccountRepo
}

func (gateway429NoCooldownRepo) SetRateLimited(context.Context, int64, time.Time) error { return nil }

func (u *gateway429AccountLimitUpstream) DoWithTLS(_ *http.Request, _ string, accountID int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.hits = append(u.hits, accountID)
	return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`))}, nil
}

// gateway429ThenSuccessUpstream 让指定账号返回成功，其余账号返回 429，
// 用于验证「A 最终 429、B 成功」这一最高外部行为接缝。
type gateway429ThenSuccessUpstream struct {
	service.HTTPUpstream
	successAccountID int64
	hits             []int64
}

func (u *gateway429ThenSuccessUpstream) DoWithTLS(_ *http.Request, _ string, accountID int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.hits = append(u.hits, accountID)
	if accountID == u.successAccountID {
		const body = `{"id":"msg_429_limit","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(body))}, nil
	}
	return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`))}, nil
}

func gateway429TestAccounts(groupID int64, count int) []*service.Account {
	accounts := make([]*service.Account, 0, count)
	for id := int64(1); id <= int64(count); id++ {
		accounts = append(accounts, &service.Account{ID: id, Name: "upstream", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: int(id), Concurrency: 1, Credentials: map[string]any{"api_key": "sk-upstream"}, AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: groupID}}})
	}
	return accounts
}

// newGateway429TestHandler 组装一个最小可用的 Anthropic 主网关 handler，
// 使 429 账号上限能在真实 Messages 入口（最高外部行为接缝）上被验证。
func newGateway429TestHandler(t *testing.T, upstream service.HTTPUpstream, accounts []*service.Account, group *service.Group) *GatewayHandler {
	t.Helper()
	repoAccounts := make([]service.Account, 0, len(accounts))
	for _, a := range accounts {
		repoAccounts = append(repoAccounts, *a)
	}
	accountRepo := openAIImagesFailoverAccountRepo{accounts: repoAccounts}
	rateLimit := service.NewRateLimitService(gateway429NoCooldownRepo{accountRepo}, nil, &config.Config{}, nil, nil)
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: accounts}, nil, nil, nil, nil)
	gw := service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, nil,
		&config.Config{}, snapshot, nil, nil, rateLimit, nil, nil, upstream,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	return &GatewayHandler{gatewayService: gw, billingCacheService: billing, concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0), maxAccountSwitches: 10, cfg: cfg}
}

func newGateway429TestContext(t *testing.T, group *service.Group) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	groupID := group.ID
	body := `{"model":"claude-sonnet-4-5","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	apiKey := &service.APIKey{ID: 8, UserID: 10, GroupID: &groupID, Status: service.StatusActive, User: &service.User{ID: 10, Concurrency: 10, Balance: 100}, Group: group}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
	return c, rec
}

// TestGatewayMessagesSucceedsOnSecondAccountAfterFirst429 覆盖验收标准 2 的前半段：
// 上限为 2 时，A 账号最终 429、B 账号成功，逻辑请求应返回成功而不是被上限提前终止。
func TestGatewayMessagesSucceedsOnSecondAccountAfterFirst429(t *testing.T) {
	groupID := int64(7772)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	upstream := &gateway429ThenSuccessUpstream{successAccountID: 2}
	h := newGateway429TestHandler(t, upstream, gateway429TestAccounts(groupID, 2), group)
	c, rec := newGateway429TestContext(t, group)
	h.Messages(c)

	require.Equal(t, []int64{1, 2}, upstream.hits)
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestGatewayMessagesNextRequestCountsIndependently 覆盖验收标准 2 的后半段：
// 触顶终止只影响本次逻辑请求，下一次调用重新独立计数并可再次尝试两个账号。
func TestGatewayMessagesNextRequestCountsIndependently(t *testing.T) {
	groupID := int64(7773)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	upstream := &gateway429AccountLimitUpstream{}
	h := newGateway429TestHandler(t, upstream, gateway429TestAccounts(groupID, 3), group)

	for i := 0; i < 2; i++ {
		c, rec := newGateway429TestContext(t, group)
		h.Messages(c)
		require.Equal(t, http.StatusTooManyRequests, rec.Code)
	}

	require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits)
}

func TestGatewayMessagesStopsAfterTwo429Accounts(t *testing.T) {
	groupID := int64(7771)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	accounts := make([]*service.Account, 0, 3)
	for id := int64(1); id <= 3; id++ {
		accounts = append(accounts, &service.Account{ID: id, Name: "upstream", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: int(id), Concurrency: 1, Credentials: map[string]any{"api_key": "sk-upstream"}, AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: groupID}}})
	}
	upstream := &gateway429AccountLimitUpstream{}
	accountRepo := openAIImagesFailoverAccountRepo{accounts: []service.Account{*accounts[0], *accounts[1], *accounts[2]}}
	rateLimit := service.NewRateLimitService(gateway429NoCooldownRepo{accountRepo}, nil, &config.Config{}, nil, nil)
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: accounts}, nil, nil, nil, nil)
	gw := service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, nil,
		&config.Config{}, snapshot, nil, nil, rateLimit, nil, nil, upstream,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	h := &GatewayHandler{gatewayService: gw, billingCacheService: billing, concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0), maxAccountSwitches: 10, cfg: cfg}
	body := `{"model":"claude-sonnet-4-5","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	apiKey := &service.APIKey{ID: 8, UserID: 10, GroupID: &groupID, Status: service.StatusActive, User: &service.User{ID: 10, Concurrency: 10, Balance: 100}, Group: group}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
	h.Messages(c)

	require.Equal(t, []int64{1, 2}, upstream.hits)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// TestGatewayMessagesStopsAtFirstAccountWithConfiguredLimitOne 覆盖验收标准 1：
// 管理员把上限调成 1 后，第一个账号最终 429 即停止，第二个账号不被尝试。
// 与默认值 2 的既有用例对照，证明 Messages 入口读取的是配置值而不是写死的 2。
func TestGatewayMessagesStopsAtFirstAccountWithConfiguredLimitOne(t *testing.T) {
	groupID := int64(7774)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	upstream := &gateway429AccountLimitUpstream{}
	h := newGateway429TestHandler(t, upstream, gateway429TestAccounts(groupID, 3), group)
	h.settingService = service.NewSettingService(newOpenAI429MatrixSettingRepo(1), h.cfg)
	c, rec := newGateway429TestContext(t, group)
	h.Messages(c)

	require.Equal(t, []int64{1}, upstream.hits, "上限为 1 时不应换到第二个账号")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// TestGatewayMessagesSucceedsOnThirdAccountWithConfiguredLimitThree 覆盖验收标准 3：
// 上限调成 3 后，前两个账号各自最终 429、第三个账号成功，逻辑请求对外成功。
// 若入口仍按默认 2 截断，第三个账号不会被尝试且对外变成 429，本用例即失败。
func TestGatewayMessagesSucceedsOnThirdAccountWithConfiguredLimitThree(t *testing.T) {
	groupID := int64(7775)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	upstream := &gateway429ThenSuccessUpstream{successAccountID: 3}
	h := newGateway429TestHandler(t, upstream, gateway429TestAccounts(groupID, 3), group)
	h.settingService = service.NewSettingService(newOpenAI429MatrixSettingRepo(3), h.cfg)
	c, rec := newGateway429TestContext(t, group)
	h.Messages(c)

	require.Equal(t, []int64{1, 2, 3}, upstream.hits, "上限为 3 时第三个账号应被尝试")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "msg_429_limit", "应把第三个账号的上游成功响应回给客户端")
}
