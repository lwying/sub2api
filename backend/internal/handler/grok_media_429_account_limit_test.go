//go:build unit

package handler

// Ticket 03 — Grok 媒体入口（图片生成 / 视频生成）的请求内 429 账号上限验收。
//
// Grok 媒体不属于 01 的主网关 Anthropic 路径，也不属于 02 的 OpenAI 兼容路径，
// 而是 OpenAIGatewayHandler.handleGrokMedia 里独立的换号循环。它与主网关共享
// 同一个「请求内 429 账号上限」语义，但有自己的同账号重试、视频计费延迟和错误映射，
// 必须逐入口验证。
//
// 测试只使用外部行为接缝：真实 handler + 真实换号循环，上游 stub 决定每个账号
// 返回 429 还是 200，断言「实际被访问的上游账号序列」与「下游响应码」，
// 不读内部计数结构。
//
// 账号一律使用 API-key（非池模式）：Grok 媒体路径只在池模式下做同账号重试，
// API-key 429 会直接换号，从而既不引入等待也不依赖既有账号级冷却。

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const grokMedia429TestModel = "grok-imagine"

// grokMedia429UpstreamMarker 只出现在上游 429 正文里，用于验证触顶响应不回传上游正文。
const grokMedia429UpstreamMarker = "grok-upstream-429-marker"

// grokMedia429Upstream 让除 successAccountID 之外的账号一律返回 429，
// 并记录被调用的账号序列（含同账号重试）。
type grokMedia429Upstream struct {
	service.HTTPUpstream
	successAccountID int64
	hits             []int64
}

func (u *grokMedia429Upstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.hits = append(u.hits, accountID)
	if u.successAccountID != 0 && accountID == u.successAccountID {
		// Grok 图片生成的既有成功载荷形态（b64_json）。
		return grokMedia429Response(http.StatusOK, `{"created":1700000000,"data":[{"b64_json":"aGVsbG8=","revised_prompt":"a cat"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`), nil
	}
	// 普通限流：措辞刻意避开 capacity/overloaded/server_busy，避免被分类为模型容量类
	// 错误而触发同账号有界重放（那会引入 500ms 等待），使本入口测试节奏变慢。
	// marker 是只属于上游正文的独特串，用来验证触顶响应不回传上游错误正文。
	return grokMedia429Response(http.StatusTooManyRequests, `{"error":{"code":429,"message":"rate limit exceeded; marker=`+grokMedia429UpstreamMarker+`","type":"rate_limit_error"}}`), nil
}

func grokMedia429Response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

func grokMedia429Accounts(count int) []service.Account {
	accounts := make([]service.Account, count)
	for i := range accounts {
		accounts[i] = service.Account{
			ID:          int64(i + 1),
			Name:        "grok-media-429-" + strconv.Itoa(i+1),
			Platform:    service.PlatformGrok,
			Type:        service.AccountTypeAPIKey,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 50,
			Priority:    i,
			GroupIDs:    []int64{24},
			Credentials: map[string]any{"api_key": "sk-grok-upstream", "access_token": "test-token"},
		}
	}
	return accounts
}

// grokMedia429AccountRepo 屏蔽账号级写入。Grok 的 429 会触发速率快照写回
// （UpdateExtra）与账号级冷却/限流写入，这些都属于既有行为，不是本票观察对象：
// 429 账号上限不得依赖它们，因此这里让它们成为无副作用的空实现。
type grokMedia429AccountRepo struct {
	openAIImagesFailoverAccountRepo
}

func (grokMedia429AccountRepo) UpdateExtra(context.Context, int64, map[string]any) error { return nil }

func (grokMedia429AccountRepo) SetRateLimited(context.Context, int64, time.Time) error { return nil }

// newGrokMedia429Env 构造一个接入了全站 429 账号上限的 Grok 媒体 handler。
// configuredLimit <= 0 表示不写设置（入口应回退到默认 2）。
func newGrokMedia429Env(t *testing.T, upstream service.HTTPUpstream, accountCount, configuredLimit int) *OpenAIGatewayHandler {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.Scheduling.StickySessionWaitTimeout = 20 * time.Millisecond
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 3

	accounts := grokMedia429Accounts(accountCount)
	repo := grokMedia429AccountRepo{openAIImagesFailoverAccountRepo{accounts: accounts}}
	slots := &grokMediaSlotsCache{accounts: map[string]int64{}, users: map[string]int64{}}
	concurrency := service.NewConcurrencyService(slots)
	bindings := &grokMediaSlotBindings{owner: 1}
	rateLimit := service.NewRateLimitService(repo, nil, cfg, nil, nil)

	var settingService *service.SettingService
	if configuredLimit > 0 {
		settingService = service.NewSettingService(newOpenAI429MatrixSettingRepo(configuredLimit), cfg)
	}

	gateway := service.NewOpenAIGatewayService(
		repo, nil, nil, nil, nil, nil, bindings, cfg, nil, concurrency,
		nil, rateLimit, nil, upstream, nil, nil, service.NewGrokTokenProvider(repo, nil),
		nil, nil, nil, settingService, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)

	handler := NewOpenAIGatewayHandler(
		gateway,
		concurrency,
		billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil, cfg,
	)
	// 换号总次数刻意放宽，使本文件观察到的停止点归因于 429 账号上限，
	// 而不是既有换号总次数；需要验证更严格限制的用例会单独收紧它。
	handler.maxAccountSwitches = 10
	return handler
}

// grokMedia429ImageContext 构造与 `/v1/images/generations` 等价的请求上下文。
func grokMedia429ImageContext() (*gin.Context, *httptest.ResponseRecorder) {
	groupID := int64(24)
	body := `{"model":"` + grokMedia429TestModel + `","prompt":"a cat"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID: 20, UserID: 10, GroupID: &groupID,
		Group: &service.Group{ID: groupID, Platform: service.PlatformGrok, AllowImageGeneration: true},
		User:  &service.User{ID: 10},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 10, Concurrency: 5})
	return c, rec
}

// TestGrokMedia429AccountLimit_ImageSucceedsOnSecondAccountAfterFirst429 覆盖验收 4 前半段：
// 上限为默认 2 时，账号 1 最终 429、账号 2 成功，图片生成返回成功，不被上限提前终止。
func TestGrokMedia429AccountLimit_ImageSucceedsOnSecondAccountAfterFirst429(t *testing.T) {
	upstream := &grokMedia429Upstream{successAccountID: 2}
	h := newGrokMedia429Env(t, upstream, 3, 0)

	c, rec := grokMedia429ImageContext()
	h.GrokImages(c)

	require.Equal(t, []int64{1, 2}, upstream.hits,
		"API-key 账号的 429 不做同账号重试，每个账号只应被访问一次")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestGrokMedia429AccountLimit_ImageStopsBeforeThirdAccount 覆盖验收 4 后半段：
// 账号 1、2 各自最终 429 后不再尝试账号 3，并按该入口既有错误映射返回 429。
func TestGrokMedia429AccountLimit_ImageStopsBeforeThirdAccount(t *testing.T) {
	upstream := &grokMedia429Upstream{}
	h := newGrokMedia429Env(t, upstream, 3, 0)

	c, rec := grokMedia429ImageContext()
	h.GrokImages(c)

	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	require.Equal(t, []int64{1, 2}, upstream.hits, "第二个账号最终 429 后不应尝试账号 3")
}

// TestGrokMedia429AccountLimit_ImageReadsConfiguredLimit 证明该入口读取的是管理员配置值，
// 而不是写死的默认 2：上限调成 1 后，第一个账号最终 429 即停止。
func TestGrokMedia429AccountLimit_ImageReadsConfiguredLimit(t *testing.T) {
	upstream := &grokMedia429Upstream{}
	h := newGrokMedia429Env(t, upstream, 3, 1)

	c, rec := grokMedia429ImageContext()
	h.GrokImages(c)

	require.Equal(t, []int64{1}, upstream.hits, "上限为 1 时不应换到第二个账号")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// TestGrokMedia429AccountLimit_ImageHigherConfiguredLimitAllowsThirdAccount 证明上限可被调高：
// 调成 3 后第三个账号也应被尝试，而第四个不试。
func TestGrokMedia429AccountLimit_ImageHigherConfiguredLimitAllowsThirdAccount(t *testing.T) {
	upstream := &grokMedia429Upstream{}
	h := newGrokMedia429Env(t, upstream, 4, 3)

	c, rec := grokMedia429ImageContext()
	h.GrokImages(c)

	require.Equal(t, []int64{1, 2, 3}, upstream.hits, "上限为 3 时应允许尝试第三个账号")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// TestGrokMedia429AccountLimit_ImageStricterSwitchLimitStillBinds 覆盖「已有更严格的限制不被放宽」：
// 全站上限调到 100，但本入口既有换号总次数限制为 1 时，账号 2 之后仍必须停止。
func TestGrokMedia429AccountLimit_ImageStricterSwitchLimitStillBinds(t *testing.T) {
	upstream := &grokMedia429Upstream{}
	h := newGrokMedia429Env(t, upstream, 4, 100)
	h.maxAccountSwitches = 1

	c, rec := grokMedia429ImageContext()
	h.GrokImages(c)

	require.Equal(t, []int64{1, 2}, upstream.hits,
		"既有更严格的换号限制（1 次切换）必须先于全站上限（100）生效")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// TestGrokMedia429AccountLimit_VideoGenerationStopsBeforeThirdAccount 覆盖视频生成分支：
// 它与图片生成共用同一个 handleGrokMedia 换号循环，因此同样受 429 账号上限约束。
func TestGrokMedia429AccountLimit_VideoGenerationStopsBeforeThirdAccount(t *testing.T) {
	upstream := &grokMedia429Upstream{}
	h := newGrokMedia429Env(t, upstream, 3, 0)

	c, rec := grokMediaSlotContext(t.Context(), true)
	h.GrokVideoGeneration(c)

	require.Equal(t, []int64{1, 2}, upstream.hits, "第二个账号最终 429 后不应尝试账号 3")
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
}

// TestGrokMedia429AccountLimit_NextRequestStartsFresh 覆盖「这些账号只在本次调用内计数」：
// 同一 handler 连续两次调用各自都会重新尝试账号 1 和 2。
func TestGrokMedia429AccountLimit_NextRequestStartsFresh(t *testing.T) {
	upstream := &grokMedia429Upstream{}
	h := newGrokMedia429Env(t, upstream, 3, 0)

	c, rec := grokMedia429ImageContext()
	h.GrokImages(c)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	first := append([]int64(nil), upstream.hits...)

	c, rec = grokMedia429ImageContext()
	h.GrokImages(c)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	second := upstream.hits[len(first):]

	require.Equal(t, []int64{1, 2}, first)
	require.Equal(t, []int64{1, 2}, second, "新调用必须重新开始计数")
}

// TestGrokMedia429AccountLimit_ExhaustedResponseDoesNotLeakUpstreamDetail 覆盖触顶后的响应卫生：
// 沿用既有 429 映射，且不回传上游正文、凭据或账号标识。
func TestGrokMedia429AccountLimit_ExhaustedResponseDoesNotLeakUpstreamDetail(t *testing.T) {
	upstream := &grokMedia429Upstream{}
	h := newGrokMedia429Env(t, upstream, 3, 0)

	c, rec := grokMedia429ImageContext()
	h.GrokImages(c)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	body := rec.Body.String()
	require.NotContains(t, body, grokMedia429UpstreamMarker, "不应回传上游错误正文")
	require.NotContains(t, body, "sk-grok-upstream", "不应回传上游凭据")
	require.NotContains(t, body, "grok-media-429", "不应回传上游账号标识")
}
