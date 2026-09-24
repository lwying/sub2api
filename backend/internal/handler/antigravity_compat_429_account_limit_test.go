//go:build unit

package handler

// Ticket 03 — Antigravity「OpenAI 兼容」入口（Chat Completions / Responses）的请求内
// 429 账号上限验收。
//
// 与 antigravity_v1beta_429_account_limit_test.go 覆盖的 Gemini 转换入口不同：
// 这里走的是 /v1/chat/completions 与 /v1/responses，选中 Antigravity OAuth 账号后由
// antigravityGatewayService.ForwardAsChatCompletions / ForwardAsResponses 转发
// （gateway_handler_chat_completions.go:298-308、gateway_handler_responses.go:287-296）。
//
// 两条入口的服务层都通过 handleAntigravityCompatTransportError 把账号切换信号转成
// UpstreamFailoverError；handler 的请求内 429 账号上限只统计 StatusCode == 429 的失败
// （request_429_account_limit.go）。若该信号被折叠成 503，上限永不触顶：
// A、B 各自 429 后仍会继续尝试 C。上游 429 使用 Antigravity 能识别的
// RATE_LIMIT_EXCEEDED + RetryInfo.retryDelay(15s)，大于 7s 阈值时服务层立刻返回换号
// 信号、不进入同账号退避，因此测试无需等待。

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const antigravityCompat429TestModel = "claude-sonnet-4-5"

// newAntigravityCompat429TestHandler 复用 ticket 03 的 Antigravity handler 组装，
// 但显式把 maxAccountSwitches 设为正数：Chat Completions / Responses 入口使用
// maxAccountSwitches（Gemini helper 只设置了 maxAccountSwitchesGemini），
// 否则第一次失败就会被恒为 0 的旧换号上限截断，无法区分「429 上限触顶」与
// 「旧换号上限触顶」，从而误证本票结论。
func newAntigravityCompat429TestHandler(
	t *testing.T,
	upstream service.HTTPUpstream,
	accounts []*service.Account,
	group *service.Group,
) *GatewayHandler {
	t.Helper()
	h := newAntigravity429TestHandler(t, upstream, accounts, group)
	h.maxAccountSwitches = 10
	return h
}

// newAntigravityCompat429TestContext 构造与 `/v1/chat/completions` 等价的请求上下文：
// 分组平台为 antigravity，选中账号是 Antigravity OAuth，因此命中 compat 转发分支。
func newAntigravityCompat429TestContext(
	t *testing.T,
	group *service.Group,
	path string,
	body string,
) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	groupID := group.ID
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(req.Context(), ctxkey.Group, group)
	ctx = context.WithValue(ctx, ctxkey.ForcePlatform, service.PlatformAntigravity)
	c.Request = req.WithContext(ctx)
	c.Set(string(middleware.ContextKeyForcePlatform), service.PlatformAntigravity)
	apiKey := &service.APIKey{
		ID: 8, UserID: 10, GroupID: &groupID, Status: service.StatusActive,
		User: &service.User{ID: 10, Concurrency: 10, Balance: 100}, Group: group,
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 10, Concurrency: 10})
	return c, rec
}

// TestAntigravityCompatChatCompletions429Limit_SwitchesToSecondAccountOnRateLimited429
// 覆盖验收 4 前半段：账号 1 收到可识别的限流 429 后进入换号，账号 2 成功 → 返回成功。
func TestAntigravityCompatChatCompletions429Limit_SwitchesToSecondAccountOnRateLimited429(t *testing.T) {
	group := newAntigravity429TestGroup(7911)
	upstream := &antigravity429Upstream{successAccountID: 2}
	h := newAntigravityCompat429TestHandler(t, upstream, antigravity429TestAccounts(group.ID, 3), group)

	c, rec := newAntigravityCompat429TestContext(t, group, "/v1/chat/completions",
		`{"model":"`+antigravityCompat429TestModel+`","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	h.ChatCompletions(c)

	require.Equal(t, []int64{1, 2}, antigravity429SwitchedAccountOrder(upstream.hits),
		"账号 1 限流后应换到账号 2，且不再访问账号 3")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestAntigravityCompatChatCompletions429Limit_StopsAfterTwoRateLimited429Accounts
// 覆盖验收 4 后半段（Chat Completions 入口）：A、B 各自被上游明确 429 之后不尝试 C。
// 账号 3 是成功探针：一旦 429 上限失效，账号 3 会被访问并让下游返回 200，断言与状态码
// 会同时失败。触顶后下游沿用该入口既有错误映射返回 429，而不是 502。
func TestAntigravityCompatChatCompletions429Limit_StopsAfterTwoRateLimited429Accounts(t *testing.T) {
	group := newAntigravity429TestGroup(7912)
	upstream := &antigravity429Upstream{successAccountID: 3}
	h := newAntigravityCompat429TestHandler(t, upstream, antigravity429TestAccounts(group.ID, 3), group)

	c, rec := newAntigravityCompat429TestContext(t, group, "/v1/chat/completions",
		`{"model":"`+antigravityCompat429TestModel+`","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	h.ChatCompletions(c)

	require.Equal(t, []int64{1, 2}, antigravity429SwitchedAccountOrder(upstream.hits),
		"账号 1、2 各自被上游 429 后应触顶请求内 429 账号上限，不再尝试账号 3")
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
}

// TestAntigravityCompatResponses429Limit_StopsAfterTwoRateLimited429Accounts 覆盖
// Responses 入口的同一契约（两条入口共用 compat 传输错误处理，但入参协议不同）。
func TestAntigravityCompatResponses429Limit_StopsAfterTwoRateLimited429Accounts(t *testing.T) {
	group := newAntigravity429TestGroup(7913)
	upstream := &antigravity429Upstream{successAccountID: 3}
	h := newAntigravityCompat429TestHandler(t, upstream, antigravity429TestAccounts(group.ID, 3), group)

	c, rec := newAntigravityCompat429TestContext(t, group, "/v1/responses",
		`{"model":"`+antigravityCompat429TestModel+`","input":"hello","stream":false}`)
	h.Responses(c)

	require.Equal(t, []int64{1, 2}, antigravity429SwitchedAccountOrder(upstream.hits),
		"账号 1、2 各自被上游 429 后应触顶请求内 429 账号上限，不再尝试账号 3")
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
}
