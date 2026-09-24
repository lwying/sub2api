//go:build unit

package handler

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

// 本文件覆盖 ticket 03 剩下的「其余可换号入口」：Gemini 原生 API 兼容层
// （`POST /v1beta/models/{model}:{action}`，含 generateContent 与
// streamGenerateContent 两种动作），以及 /antigravity 前缀的同一 handler。
// 这些入口不属于 01 的主网关 Anthropic 路径，也不属于 02 的 OpenAI 兼容路径，
// 但同样持有换号循环，必须遵守同一「请求内 429 账号上限」。
//
// 测试只使用外部行为接缝：真实 handler + 真实请求上下文，上游用 stub 决定
// 每个账号返回 429 还是成功，断言「实际被访问的上游账号序列」和「下游响应码」，
// 不读内部计数结构。

const gemini429TestModel = "gemini-2.5-pro"

// gemini429AccountLimitUpstream 让除 successAccountID 之外的账号一律返回 429，
// 用于验证「A 最终 429、B 成功」以及「A、B 各自最终 429 后不尝试 C」。
type gemini429AccountLimitUpstream struct {
	service.HTTPUpstream
	successAccountID int64
	hits             []int64
}

func (u *gemini429AccountLimitUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	return u.respond(accountID), nil
}

func (u *gemini429AccountLimitUpstream) DoWithTLS(_ *http.Request, _ string, accountID int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.respond(accountID), nil
}

func (u *gemini429AccountLimitUpstream) respond(accountID int64) *http.Response {
	u.hits = append(u.hits, accountID)
	if accountID == u.successAccountID {
		const body = `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(body))}
	}
	const body = `{"error":{"code":429,"message":"rate limited","status":"RESOURCE_EXHAUSTED"}}`
	return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(body))}
}

// gemini429NoCooldownRepo 屏蔽账号级 429 冷却写入，使测试只观察换号行为；
// 429 账号上限不得依赖既有账号级冷却，因此这里不能让它反过来影响选号。
type gemini429NoCooldownRepo struct {
	openAIImagesFailoverAccountRepo
}

func (gemini429NoCooldownRepo) SetRateLimited(context.Context, int64, time.Time) error { return nil }

func gemini429TestAccounts(groupID int64, count int) []*service.Account {
	accounts := make([]*service.Account, 0, count)
	for id := int64(1); id <= int64(count); id++ {
		accounts = append(accounts, &service.Account{
			ID:            id,
			Name:          "gemini-upstream",
			Platform:      service.PlatformGemini,
			Type:          service.AccountTypeAPIKey,
			Status:        service.StatusActive,
			Schedulable:   true,
			Priority:      int(id),
			Concurrency:   1,
			Credentials:   map[string]any{"api_key": "sk-gemini"},
			AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: groupID}},
		})
	}
	return accounts
}

// newGemini429TestHandler 组装一个最小可用的 Gemini 原生 handler：
// 选号走真实 GatewayService，转发走注入的 stub 上游。
func newGemini429TestHandler(t *testing.T, upstream service.HTTPUpstream, accounts []*service.Account, group *service.Group) *GatewayHandler {
	t.Helper()
	repoAccounts := make([]service.Account, 0, len(accounts))
	for _, a := range accounts {
		repoAccounts = append(repoAccounts, *a)
	}
	accountRepo := gemini429NoCooldownRepo{openAIImagesFailoverAccountRepo{accounts: repoAccounts}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	rateLimit := service.NewRateLimitService(accountRepo, nil, cfg, nil, nil)
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: accounts}, nil, nil, nil, nil)
	gw := service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, nil,
		cfg, snapshot, nil, nil, rateLimit, nil, nil, upstream,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	return &GatewayHandler{
		gatewayService:           gw,
		geminiCompatService:      service.NewGeminiMessagesCompatService(accountRepo, nil, nil, snapshot, nil, rateLimit, upstream, nil, cfg),
		billingCacheService:      billing,
		concurrencyHelper:        NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatNone, 0),
		maxAccountSwitchesGemini: 10,
		cfg:                      cfg,
	}
}

// newGemini429TestContext 构造与路由 `/v1beta/models/*modelAction` 等价的请求上下文。
// action 形如 generateContent / streamGenerateContent，路由参数保留前导 "/"。
func newGemini429TestContext(t *testing.T, group *service.Group, action string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	groupID := group.ID
	body := `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1beta/models/"+gemini429TestModel+":"+action,
		bytes.NewBufferString(body),
	)
	req.Header.Set("Content-Type", "application/json")
	c.Request = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	c.Params = gin.Params{{Key: "modelAction", Value: "/" + gemini429TestModel + ":" + action}}
	apiKey := &service.APIKey{
		ID: 8, UserID: 10, GroupID: &groupID, Status: service.StatusActive,
		User: &service.User{ID: 10, Concurrency: 10, Balance: 100}, Group: group,
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
	return c, rec
}

func newGemini429TestGroup(groupID int64) *service.Group {
	return &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformGemini, Status: service.StatusActive}
}

// gemini429SwitchedAccountOrder 把上游命中序列压成「实际换号顺序」：Gemini 上游
// 服务对 429 有既有的同账号短重试（geminiMaxRetries），同一账号会被多次访问。
// 429 账号上限只按**不同账号**计数，因此断言口径是折叠后的账号序列，而不是
// 每次 upstream attempt；同账号重试的具体次数不属于本票的约束。
func gemini429SwitchedAccountOrder(hits []int64) []int64 {
	order := make([]int64, 0, len(hits))
	for _, id := range hits {
		if len(order) == 0 || order[len(order)-1] != id {
			order = append(order, id)
		}
	}
	return order
}

// TestGeminiV1BetaGenerateContentSucceedsOnSecondAccountAfterFirst429 覆盖验收 4 前半段：
// 上限为 2 时，A 账号最终 429、B 账号成功，逻辑请求返回成功，不被上限提前终止。
func TestGeminiV1BetaGenerateContentSucceedsOnSecondAccountAfterFirst429(t *testing.T) {
	group := newGemini429TestGroup(7801)
	upstream := &gemini429AccountLimitUpstream{successAccountID: 2}
	h := newGemini429TestHandler(t, upstream, gemini429TestAccounts(group.ID, 3), group)

	c, rec := newGemini429TestContext(t, group, "generateContent")
	h.GeminiV1BetaModels(c)

	require.Equal(t, []int64{1, 2}, gemini429SwitchedAccountOrder(upstream.hits))
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestGeminiV1BetaStreamGenerateContentStopsAfterTwo429Accounts 覆盖验收 4 后半段：
// A、B 各自最终 429 后不再尝试 C，并按该入口现有错误映射返回 429。
// streamGenerateContent 与 generateContent 共用同一换号循环，因此上限对流式同样生效。
func TestGeminiV1BetaStreamGenerateContentStopsAfterTwo429Accounts(t *testing.T) {
	group := newGemini429TestGroup(7802)
	upstream := &gemini429AccountLimitUpstream{}
	h := newGemini429TestHandler(t, upstream, gemini429TestAccounts(group.ID, 3), group)

	c, rec := newGemini429TestContext(t, group, "streamGenerateContent")
	h.GeminiV1BetaModels(c)

	require.Equal(t, []int64{1, 2}, gemini429SwitchedAccountOrder(upstream.hits))
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}
