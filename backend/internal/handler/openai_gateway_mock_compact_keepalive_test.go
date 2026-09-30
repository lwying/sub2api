//go:build unit

package handler

// 下游测试请求 mock 与 body-signal compact 下游心跳（#3887）的交互。
//
// compact 心跳会在上游 unary 等待期间向下游写出 SSE 注释行并提交 200，但它
// 不会设置 response_committed（那个 key 由 handleErrorResponse 系列设置）。而
// compact 请求的白名单归一化会删掉 stream 字段，于是 mock 按"非流式"判定构造
// 裸 JSON。若 mock 在这条已提交的 SSE 流上接管写回，客户端会收到
// ": keepalive\n\n{...json...}"——注释行后面的裸 JSON 不是合法 SSE 事件，解析
// 直接失败。
//
// 反向的约束同样成立：mock 判定不得反过来摘下这条心跳。真实 body-signal compact
// 请求的 input 必带 compaction_trigger，永不可能被判为下游测试请求，因此判定必须
// 先于停拍——先无条件停拍等于给每个真实 compact 请求在上游等待期间摘掉唯一的空闲
// 保护，下游重新变成零字节静默。两个方向合起来即：规则快照只读一次，命中才停拍
// 接管；不命中时心跳必须继续跳动。
//
// 真实链路里这两件事不会同时发生（body-signal 提升要求 input 带
// compaction_trigger，而带该 item 的请求不会被判定为下游测试请求），因此这里用
// 处理器可用的接缝显式复现该状态：真实路由 -> 真实 handler -> 真实 service ->
// 本地 httptest 上游；只把「客户端流式标记」按 body-signal 提升的效果直接写入
// 上下文，并用慢一拍的并发缓存让心跳在 mock 判定之前完成首拍。
//
// 只使用本地 httptest 作为上游，不访问任何生产上游。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const compactMockUpstreamModel = "gpt-5.1"

// compactMockDelayedUserSlotCache 在用户槽位获取上刻意延迟，让 compact 心跳
// 在 mock 判定之前完成首拍；其余并发能力沿用标准 fake。
type compactMockDelayedUserSlotCache struct {
	*fakeConcurrencyCache
	userSlotDelay time.Duration
}

func (c *compactMockDelayedUserSlotCache) AcquireUserSlot(ctx context.Context, _ int64, _ int, _ string) (bool, error) {
	if c.userSlotDelay > 0 {
		timer := time.NewTimer(c.userSlotDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return true, nil
}

type compactMockKeepaliveEnv struct {
	handler *OpenAIGatewayHandler
	capture *requestTraceRealUpstream
	group   *service.Group
}

// newCompactMockKeepaliveEnv 装配真实上游 client 的最小 OpenAI 网关：账号
// base_url 指向本地 httptest 上游，compact 心跳间隔 1s。
//
// upstreamDelay 让上游在收到请求后保持静默：这正是 compact 心跳存在的场景
// （上游 unary 等待期间零字节），回调里所有路径都不得在此之前摘掉心跳。
func newCompactMockKeepaliveEnv(t *testing.T, userSlotDelay, upstreamDelay time.Duration) *compactMockKeepaliveEnv {
	t.Helper()

	capture := &requestTraceRealUpstream{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		capture.record(req, body)
		if upstreamDelay > 0 {
			timer := time.NewTimer(upstreamDelay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-req.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(requestTraceRealOpenAIJSON))
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	// config 校验里的 5..30s 下限不适用于这里的直接构造；1s 让心跳在测试的
	// 用户槽位等待（1.5s）内完成首拍。
	cfg.Gateway.StreamKeepaliveInterval = 1

	const groupID int64 = 7921
	const accountID int64 = 9421
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformOpenAI, Status: service.StatusActive}
	accounts := []service.Account{{
		ID: accountID, Name: "compact-mock-upstream", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Priority: 1, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-compact-mock", "base_url": upstream.URL},
	}}

	gatewayService := service.NewOpenAIGatewayService(
		openAIImagesFailoverAccountRepo{accounts: accounts},
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil,
		repository.NewHTTPUpstream(cfg),
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)

	handler := NewOpenAIGatewayHandler(
		gatewayService,
		service.NewConcurrencyService(&compactMockDelayedUserSlotCache{
			fakeConcurrencyCache: &fakeConcurrencyCache{},
			userSlotDelay:        userSlotDelay,
		}),
		billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil, cfg,
	)
	handler.maxAccountSwitches = 10

	settingRepo := &gatewayMockSettingsRepo{values: map[string]string{}}
	startGatewayMockRules(t, settingRepo, "hi", "本地回复")
	handler.SetSettingService(gatewayMockSettingService(t, settingRepo))

	return &compactMockKeepaliveEnv{handler: handler, capture: capture, group: group}
}

// compactMockEligibleBody 是一条单轮纯文本请求体：命中 mock 关键词「hi」。
const compactMockEligibleBody = `{"model":"` + compactMockUpstreamModel + `","input":[{"type":"message","role":"user","content":"hi"}]}`

// compactMockRealCompactBody 是一条真实 body-signal compact 请求体：input 除
// 单轮文本外还带 compaction_trigger。带该 item 的调用按定义不属于下游测试请求
// （extractResponsesSingleUserText 见到 compaction_trigger 即判定有实质任务），
// 所以哪怕正文里出现的正是 mock 关键词「hi」，也不得被本地 mock 接管。
const compactMockRealCompactBody = `{"model":"` + compactMockUpstreamModel + `","stream":true,` +
	`"input":[{"type":"message","role":"user","content":"hi"},{"type":"compaction_trigger"}]}`

// servePathBasedCompactMock 发送一条 /v1/responses/compact 请求，并显式写入
// body-signal 提升后的客户端流式标记（归一化 body 已丢掉 stream：reqStream=false）。
func (e *compactMockKeepaliveEnv) servePathBasedCompactMock(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	group := e.group
	r := gin.New()
	r.POST("/v1/responses/compact", func(c *gin.Context) {
		keyGroupID := group.ID
		apiKey := &service.APIKey{
			ID: 21, UserID: 31, GroupID: &keyGroupID, Status: service.StatusActive,
			User: &service.User{ID: 31, Concurrency: 10, Balance: 100}, Group: group,
		}
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 31, Concurrency: 10})
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, group))
		service.MarkOpenAICompactClientStream(c)
		c.Next()
	}, e.handler.Responses)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// requireNoRawJSONOnCommittedStream 断言响应体要么还没提交、要么整条都是合法的
// SSE 帧（注释行 / event 名 / data 行）；裸 JSON 行意味着下游解析必然失败。
func requireNoRawJSONOnCommittedStream(t *testing.T, body string) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		ok := strings.HasPrefix(trimmed, ":") ||
			strings.HasPrefix(trimmed, "event: ") ||
			strings.HasPrefix(trimmed, "data: ")
		require.True(t, ok, "committed stream contains a non-SSE line: %q", trimmed)
	}
}

// TestOpenAICompactMock_NotServedAfterCompactKeepaliveCommitted 是本文件的核心
// 复现：compact 心跳已经把响应头提交为 200 的 SSE 流之后，本地 mock 不得再按
// 裸 JSON 接管写回。
func TestOpenAICompactMock_NotServedAfterCompactKeepaliveCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newCompactMockKeepaliveEnv(t, 1500*time.Millisecond, 0)

	w := env.servePathBasedCompactMock(t, compactMockEligibleBody)

	// 心跳必须真的先提交了（否则本用例没有复现目标状态）。
	require.True(t, strings.HasPrefix(w.Body.String(), ": keepalive\n\n"),
		"compact keepalive must have committed the stream before the mock decision: %s", w.Body.String())
	require.Equal(t, http.StatusOK, w.Code)

	// 已提交的 SSE 流上不得出现本地 mock 的裸 JSON 接管。
	require.Empty(t, w.Header().Get(service.GatewayMockReplyHeader),
		"local mock must not take over an already committed stream")
	require.NotContains(t, w.Body.String(), "本地回复")
	requireNoRawJSONOnCommittedStream(t, w.Body.String())

	// 标准 compact 写回接管：上游被真实调用，响应以 SSE 终止事件收尾。
	require.Equal(t, []string{"/v1/responses/compact"}, realUpstreamPaths(env.capture))
	require.Contains(t, w.Body.String(), "event: response.completed")
}

// TestOpenAICompactMock_ServedWhenKeepaliveNotCommitted 保证修复不改变标准路径：
// 心跳尚未提交（没有发生首拍）时，命中规则的下游测试请求仍由本地 mock 接管，
// 且不发出上游请求。
func TestOpenAICompactMock_ServedWhenKeepaliveNotCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newCompactMockKeepaliveEnv(t, 0, 0)

	w := env.servePathBasedCompactMock(t, compactMockEligibleBody)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, service.GatewayMockReplyHeaderValue, w.Header().Get(service.GatewayMockReplyHeader),
		"an uncommitted response must still be served by the local mock")
	require.Contains(t, w.Body.String(), "本地回复")
	require.Empty(t, realUpstreamPaths(env.capture), "命中本地 mock 时不得向上游发出任何请求")
}

// TestOpenAICompactMock_KeepaliveContinuesForRealCompactRequest 是本次修复的核心
// 复现：#3887 的下游心跳是 body-signal compact 在上游 unary 等待期间唯一的空闲
// 保护，但 mock 判定曾无条件先停拍——真实 compact 请求的 input 必带
// compaction_trigger，永远不可能被判为下游测试请求，于是每个真实 compact 请求
// 一走到 mock 判定就被摘掉心跳，上游仍在等待（大上下文可达数分钟）时下游重新
// 变成零字节静默，反代空闲超时照旧掐断连接。
//
// 这里用真实路由复现：上游静默 3s（= 心跳的 3 个间隔），mock 规则启用且关键词
// 「hi」就出现在请求正文里，但请求带 compaction_trigger，因此不得命中；判定之后
// 心跳必须继续跳动到上游应答为止。
func TestOpenAICompactMock_KeepaliveContinuesForRealCompactRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newCompactMockKeepaliveEnv(t, 0, 3*time.Second)

	w := env.servePathBasedCompactMock(t, compactMockRealCompactBody)

	require.Equal(t, http.StatusOK, w.Code)
	// 带 compaction_trigger 的请求不是下游测试请求：不得被本地 mock 接管。
	require.Empty(t, w.Header().Get(service.GatewayMockReplyHeader),
		"a request carrying compaction_trigger is not a downstream test request")
	require.NotContains(t, w.Body.String(), "本地回复")
	// 关键断言：mock 判定通过之后，心跳仍在上游等待期间继续写注释行。
	require.GreaterOrEqual(t, strings.Count(w.Body.String(), ": keepalive\n\n"), 2,
		"compact keepalive must survive the mock eligibility check: %s", w.Body.String())
	// 心跳与真实 SSE 写回不得互相污染，且上游被真实调用。
	requireNoRawJSONOnCommittedStream(t, w.Body.String())
	require.Equal(t, []string{"/v1/responses/compact"}, realUpstreamPaths(env.capture))
	require.Contains(t, w.Body.String(), "event: response.completed")
}
