//go:build unit

package handler

// Ticket 03 — Antigravity「Gemini 转换」入口的请求内 429 账号上限验收。
//
// 入口：`POST /v1beta/models/{model}:{action}` 走 /antigravity 强制平台，命中
// gemini_v1beta_handler.go:645-659 的分支——当选中账号是 Antigravity 且非 API-key 时，
// 由 antigravityGatewayService.ForwardGemini 把 Gemini 请求转换成 Antigravity
// v1internal 形态转发，而不是走 Gemini 原生兼容层。
//
// 这是与 Gemini 原生入口不同的上游协议，因此必须独立验证其「429 后是否换号、
// 换到第几个账号停下、下游返回什么」。
//
// 上游 429 使用 Antigravity 能识别的 RATE_LIMIT_EXCEEDED + RetryInfo.retryDelay(15s)：
//           > antigravityRateLimitThreshold(7s) 时服务层直接判定模型限流并立刻返回
//           换号信号，不会进入同账号指数退避重试，因此测试无需等待。

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
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const antigravity429TestModel = "gemini-2.5-pro"

// antigravity429Upstream 让除 successAccountID 之外的账号返回可识别的限流 429，
// 成功账号返回 Antigravity 流式事件（该入口上游只支持 streamGenerateContent）。
type antigravity429Upstream struct {
	service.HTTPUpstream
	successAccountID int64
	hits             []int64
}

func (u *antigravity429Upstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	return u.respond(accountID), nil
}

func (u *antigravity429Upstream) DoWithTLS(_ *http.Request, _ string, accountID int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.respond(accountID), nil
}

func (u *antigravity429Upstream) respond(accountID int64) *http.Response {
	u.hits = append(u.hits, accountID)
	if u.successAccountID != 0 && accountID == u.successAccountID {
		const body = "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2,\"totalTokenCount\":5}}}\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(bytes.NewBufferString(body)),
		}
	}
	// 可识别的账号级限流：reason=RATE_LIMIT_EXCEEDED + metadata.model + retryDelay 15s
	// （> 7s 阈值）→ 服务层不重试、立刻换号。
	const body = `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"rate limited","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"gemini-2.5-pro"}},{"@type":"google.rpc.RetryInfo","retryDelay":"15s"}]}}`
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

// antigravity429TestAccounts 构造 Antigravity OAuth 账号。凭据自带未过期的
// access_token 与 project_id，配合 (nil,nil,nil) 的 token provider 时不会触发
// 刷新网络调用，也不会走 credits/overages 分支。
func antigravity429TestAccounts(groupID int64, count int) []*service.Account {
	accounts := make([]*service.Account, 0, count)
	for id := int64(1); id <= int64(count); id++ {
		accounts = append(accounts, &service.Account{
			ID:          id,
			Name:        "antigravity-429-upstream",
			Platform:    service.PlatformAntigravity,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    int(id),
			Credentials: map[string]any{
				"access_token": "test-token-" + strconv.FormatInt(id, 10),
				"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				"project_id":   "test-project",
			},
			GroupIDs:      []int64{groupID},
			AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: groupID}},
		})
	}
	return accounts
}

// newAntigravity429TestHandler 复用 ticket 03 的 Gemini handler 组装，仅替换
// antigravityGatewayService 为注入 stub 上游的真实服务。
func newAntigravity429TestHandler(t *testing.T, upstream service.HTTPUpstream, accounts []*service.Account, group *service.Group) *GatewayHandler {
	t.Helper()
	h := newGemini429TestHandler(t, upstream, accounts, group)
	// settingService 必须非 nil 且 cfg 非 nil：Antigravity 流式合并路径会直接读
	// settingService.cfg（antigravity_gateway_streaming.go:338）。
	h.antigravityGatewayService = service.NewAntigravityGatewayService(
		nil,
		nil,
		nil,
		service.NewAntigravityTokenProvider(nil, nil, nil),
		nil,
		upstream,
		service.NewSettingService(newOpenAI429MatrixSettingRepo(0), &config.Config{}),
		nil,
	)
	return h
}

// newAntigravity429TestContext 构造与 `/antigravity/v1beta/models/*modelAction` 等价的
// 请求上下文：强制平台为 antigravity，分组平台为 antigravity。
func newAntigravity429TestContext(t *testing.T, group *service.Group, action string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	groupID := group.ID
	body := `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(
		http.MethodPost,
		"/antigravity/v1beta/models/"+antigravity429TestModel+":"+action,
		bytes.NewBufferString(body),
	)
	req.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(req.Context(), ctxkey.Group, group)
	ctx = context.WithValue(ctx, ctxkey.ForcePlatform, service.PlatformAntigravity)
	c.Request = req.WithContext(ctx)
	c.Set(string(middleware.ContextKeyForcePlatform), service.PlatformAntigravity)
	c.Params = gin.Params{{Key: "modelAction", Value: "/" + antigravity429TestModel + ":" + action}}
	apiKey := &service.APIKey{
		ID: 8, UserID: 10, GroupID: &groupID, Status: service.StatusActive,
		User: &service.User{ID: 10, Concurrency: 10, Balance: 100}, Group: group,
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 10, Concurrency: 10})
	return c, rec
}

func newAntigravity429TestGroup(groupID int64) *service.Group {
	return &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAntigravity, Status: service.StatusActive}
}

// antigravity429SwitchedAccountOrder 折叠同账号重复访问，只保留「不同账号」的出现顺序。
// Antigravity 服务层的同账号重试具体次数不属于本票约束。
func antigravity429SwitchedAccountOrder(hits []int64) []int64 {
	return gemini429SwitchedAccountOrder(hits)
}

// TestAntigravityV1Beta429Limit_SwitchesToSecondAccountOnRateLimited429 覆盖验收 4 前半段：
// 账号 1 收到可识别的限流 429 后立刻换号，账号 2 成功 → 逻辑请求返回成功。
// 同时断言耗时远小于任何同账号退避（1s 起），证明该 429 没有进入等待路径。
func TestAntigravityV1Beta429Limit_SwitchesToSecondAccountOnRateLimited429(t *testing.T) {
	group := newAntigravity429TestGroup(7901)
	upstream := &antigravity429Upstream{successAccountID: 2}
	h := newAntigravity429TestHandler(t, upstream, antigravity429TestAccounts(group.ID, 3), group)

	c, rec := newAntigravity429TestContext(t, group, "generateContent")
	started := time.Now()
	h.GeminiV1BetaModels(c)
	elapsed := time.Since(started)

	require.Equal(t, []int64{1, 2}, antigravity429SwitchedAccountOrder(upstream.hits),
		"账号 1 限流后应换到账号 2，且不再访问账号 3")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Less(t, elapsed, 2*time.Second, "可识别的长 retryDelay 限流不应进入同账号退避等待")
}

// TestAntigravityV1Beta429Limit_StopsAfterTwoRateLimited429Accounts 覆盖验收 4 后半段：
// A、B 各自被上游明确 429 之后不尝试 C。
//
// 该入口的服务层把「上游 429 → 换号」折叠成账号切换信号，再由
// antigravity_gateway_gemini.go 转成 UpstreamFailoverError。信号必须保留触发本次
// 切换的上游状态码：只有真实的上游 429 才透传 429，让 handler 的请求内 429 账号
// 上限（只统计 StatusCode == 429）计入 A 与 B 并在触顶时终止；非 429 的切换
// （503 容量、调度前模型限流预检查等）仍按原有 503 语义处理。
//
// 账号 3 被设为成功账号作为探针：一旦上限再次失效，账号 3 会被访问并让下游返回
// 200，断言与响应码会同时失败。触顶后下游按该入口既有映射返回 429
// （mapGeminiUpstreamError：上游 429 → 429），而不是 502。
func TestAntigravityV1Beta429Limit_StopsAfterTwoRateLimited429Accounts(t *testing.T) {
	group := newAntigravity429TestGroup(7902)
	upstream := &antigravity429Upstream{successAccountID: 3}
	h := newAntigravity429TestHandler(t, upstream, antigravity429TestAccounts(group.ID, 3), group)

	c, rec := newAntigravity429TestContext(t, group, "generateContent")
	h.GeminiV1BetaModels(c)

	require.Equal(t, []int64{1, 2}, antigravity429SwitchedAccountOrder(upstream.hits),
		"账号 1、2 各自被上游 429 后应触顶请求内 429 账号上限，不再尝试账号 3")
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
}
