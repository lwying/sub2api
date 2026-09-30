//go:build unit

package handler

// 下游测试请求 mock 的真实链路测试：真实路由 -> 真实 handler -> 真实 service ->
// repository.NewHTTPUpstream -> 本地 httptest 上游。
//
// 关键断言是"命中时上游零请求、零使用记录、Trace 无 wire 尝试"：本地 mock 回复
// 不得冒充上游响应，也不得产生计费事实。只使用本地 httptest 作为上游。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// gatewayMockSettingsRepo 是只覆盖设置读写的内存仓库。
type gatewayMockSettingsRepo struct {
	service.SettingRepository
	values map[string]string
}

func (r *gatewayMockSettingsRepo) GetValue(ctx context.Context, key string) (string, error) {
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", service.ErrSettingNotFound
}

func (r *gatewayMockSettingsRepo) Set(ctx context.Context, key, value string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}

// gatewayMockSettingService 构造读取该仓库的设置服务，供 OpenAI 侧处理器注入。
func gatewayMockSettingService(t *testing.T, repo *gatewayMockSettingsRepo) *service.SettingService {
	t.Helper()
	return service.NewSettingService(repo, &config.Config{})
}

// startGatewayMockRules 在给定设置仓库里启用「关键词 -> 固定回复」。
func startGatewayMockRules(t *testing.T, repo *gatewayMockSettingsRepo, keyword, reply string) {
	t.Helper()
	encoded, err := json.Marshal(service.GatewayMockSettings{
		Enabled: true,
		Rules:   []service.GatewayMockRule{{ID: "rule-1", Keyword: keyword, Reply: reply, Enabled: true}},
	})
	require.NoError(t, err)
	require.NoError(t, repo.Set(context.Background(), service.SettingKeyGatewayMock, string(encoded)))
}

// upstreamPaths 取本地上游真实收到的请求路径快照。
func upstreamPaths(capture *requestTraceMatrixUpstream) []string {
	paths, _ := capture.snapshot()
	return paths
}

// realUpstreamPaths 取"真实链路"夹具（requestTraceRealUpstream）的路径快照。
func realUpstreamPaths(capture *requestTraceRealUpstream) []string {
	paths, _ := capture.snapshot()
	return paths
}

// gatewayMockMessagesBody 构造一条单轮单文本的 Anthropic Messages 请求。
func gatewayMockMessagesBody(text string, stream bool) string {
	encoded, _ := json.Marshal(map[string]any{
		"model":      requestTraceMatrixModel,
		"max_tokens": 256,
		"stream":     stream,
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}},
		},
	})
	return string(encoded)
}

// gatewayMockEnv 是一次真实链路运行所需的接线：真实 GatewayHandler + 本地可控上游。
type gatewayMockEnv struct {
	handler     *GatewayHandler
	capture     *requestTraceMatrixUpstream
	settingRepo *gatewayMockSettingsRepo
	settings    *service.SettingService
	group       *service.Group
	groupID     int64
	apiKeyID    int64
	userID      int64
	// slots 与 usagePool 只在需要观察「槽位归还」和「是否提交使用记录」的用例里注入；
	// 未注入的用例保持既有最小接线不变。
	slots     *gatewayMockSlotCountingCache
	usagePool *service.UsageRecordWorkerPool
}

// newGatewayMockEnv 装配 Anthropic 分组的真实网关，账号 base_url 指向本地上游。
func newGatewayMockEnv(t *testing.T) *gatewayMockEnv {
	t.Helper()

	capture := &requestTraceMatrixUpstream{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		capture.recordRequest(req, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_upstream","type":"message","role":"assistant","model":"` +
			requestTraceMatrixModel + `","content":[{"type":"text","text":"upstream"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true

	const groupID int64 = 7901
	const accountID int64 = 9401
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	account := &service.Account{
		ID: accountID, Name: "mock-upstream", Platform: service.PlatformAnthropic,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Priority: 1, Concurrency: 1,
		Credentials:   map[string]any{"api_key": "sk-mock-credential", "base_url": upstream.URL},
		AccountGroups: []service.AccountGroup{{AccountID: accountID, GroupID: groupID}},
	}

	settingRepo := &gatewayMockSettingsRepo{values: map[string]string{}}
	settingService := service.NewSettingService(settingRepo, cfg)

	accountRepo := openAIImagesFailoverAccountRepo{accounts: []service.Account{*account}}
	rateLimit := service.NewRateLimitService(gateway429NoCooldownRepo{accountRepo}, nil, &config.Config{}, nil, nil)
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account}}, nil, nil, nil, nil)
	gw := service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, nil,
		cfg, snapshot, nil, nil, rateLimit, nil, nil,
		repository.NewHTTPUpstream(cfg),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)

	handler := NewGatewayHandler(
		gw, nil, nil, nil, nil,
		service.NewConcurrencyService(&fakeConcurrencyCache{}),
		billing, nil, nil, nil, nil, nil, nil,
		cfg, settingService,
	)
	return &gatewayMockEnv{
		handler: handler, capture: capture, settingRepo: settingRepo, settings: settingService,
		group: group, groupID: groupID, apiKeyID: 31, userID: 41,
	}
}

func (e *gatewayMockEnv) serve(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	group := e.group
	groupID := e.groupID
	apiKeyID := e.apiKeyID
	userID := e.userID

	r := gin.New()
	r.POST(path, func(c *gin.Context) {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, group))
		apiKey := &service.APIKey{
			ID: apiKeyID, UserID: userID, GroupID: &groupID, Status: service.StatusActive,
			User: &service.User{ID: userID, Concurrency: 10, Balance: 100}, Group: group,
		}
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: userID, Concurrency: 10})
		c.Next()
	}, e.handler.Messages)

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestGatewayMock_OpenAISideEntriesReturnLocalReplyWithoutUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("chat completions", func(t *testing.T) {
		env := newRequestTraceRealOpenAIEnv(t, false)
		settingRepo := &gatewayMockSettingsRepo{values: map[string]string{}}
		startGatewayMockRules(t, settingRepo, "hi", "本地回复")
		env.handler.SetSettingService(gatewayMockSettingService(t, settingRepo))

		body, _ := json.Marshal(map[string]any{
			"model":    "gpt-5.1",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		w := env.serve(t, "/v1/chat/completions", string(body), env.handler.ChatCompletions)

		require.Equal(t, http.StatusOK, w.Code)
		require.Empty(t, realUpstreamPaths(env.capture), "命中本地 mock 时不得向上游发出任何请求")
		require.Equal(t, service.GatewayMockReplyHeaderValue, w.Header().Get(service.GatewayMockReplyHeader))
		require.Contains(t, w.Body.String(), "本地回复")
		require.Contains(t, w.Body.String(), `"total_tokens":0`, "本地 mock 不得编造用量")
	})

	t.Run("responses", func(t *testing.T) {
		env := newRequestTraceRealOpenAIEnv(t, false)
		settingRepo := &gatewayMockSettingsRepo{values: map[string]string{}}
		startGatewayMockRules(t, settingRepo, "hi", "本地回复")
		env.handler.SetSettingService(gatewayMockSettingService(t, settingRepo))

		body, _ := json.Marshal(map[string]any{"model": "gpt-5.1", "input": "hi"})
		w := env.serve(t, "/v1/responses", string(body), env.handler.Responses)

		require.Equal(t, http.StatusOK, w.Code)
		require.Empty(t, realUpstreamPaths(env.capture), "命中本地 mock 时不得向上游发出任何请求")
		require.Equal(t, service.GatewayMockReplyHeaderValue, w.Header().Get(service.GatewayMockReplyHeader))
		require.Contains(t, w.Body.String(), "本地回复")
	})

	t.Run("messages via the openai handler", func(t *testing.T) {
		env := newRequestTraceMatrixOpenAIEnv(t)
		settingRepo := &gatewayMockSettingsRepo{values: map[string]string{}}
		startGatewayMockRules(t, settingRepo, "hi", "本地回复")
		env.openai.SetSettingService(gatewayMockSettingService(t, settingRepo))

		body := gatewayMockMessagesBody("hi", false)
		w := env.serve(t, "/v1/messages", body, env.openai.Messages)

		require.Equal(t, http.StatusOK, w.Code)
		require.Empty(t, upstreamPaths(env.capture), "命中本地 mock 时不得向上游发出任何请求")
		require.Equal(t, service.GatewayMockReplyHeaderValue, w.Header().Get(service.GatewayMockReplyHeader))
	})
}

func TestGatewayMock_MessagesNonStreamReturnsLocalReplyWithoutUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newGatewayMockEnv(t)
	startGatewayMockRules(t, env.settingRepo, "hi", "你好！这是本地测试回复。")

	w := env.serve(t, "/v1/messages", gatewayMockMessagesBody("  HI  ", false))

	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, upstreamPaths(env.capture), "命中本地 mock 时不得向上游发出任何请求")
	require.Equal(t, service.GatewayMockReplyHeaderValue, w.Header().Get(service.GatewayMockReplyHeader))

	var response map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, "assistant", response["role"])

	content, ok := response["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)
	block, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "你好！这是本地测试回复。", block["text"])

	usage, ok := response["usage"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(0), usage["input_tokens"], "本地 mock 不得编造输入用量")
	require.Equal(t, float64(0), usage["output_tokens"], "本地 mock 不得编造输出用量")
}

func TestGatewayMock_UnmatchedRequestStillReachesUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newGatewayMockEnv(t)
	startGatewayMockRules(t, env.settingRepo, "hi", "你好！")

	w := env.serve(t, "/v1/messages", gatewayMockMessagesBody("你好今天天气怎么样", false))

	require.Equal(t, http.StatusOK, w.Code)
	require.NotEmpty(t, upstreamPaths(env.capture), "带实质任务的请求必须继续访问上游")
	require.Empty(t, w.Header().Get(service.GatewayMockReplyHeader))
}

func TestGatewayMock_DisabledSwitchNeverIntercepts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newGatewayMockEnv(t)
	encoded, err := json.Marshal(service.GatewayMockSettings{
		Enabled: false,
		Rules:   []service.GatewayMockRule{{ID: "rule-1", Keyword: "hi", Reply: "你好！", Enabled: true}},
	})
	require.NoError(t, err)
	require.NoError(t, env.settingRepo.Set(context.Background(), service.SettingKeyGatewayMock, string(encoded)))

	w := env.serve(t, "/v1/messages", gatewayMockMessagesBody("hi", false))

	require.Equal(t, http.StatusOK, w.Code)
	require.NotEmpty(t, upstreamPaths(env.capture), "总开关关闭时规则不得生效")
	require.Empty(t, w.Header().Get(service.GatewayMockReplyHeader))
}

// 命中时不仅不发上游，也必须不在计费/用量路径上留下任何痕迹：
// 用流式请求验证——真实转发路径一定会写出 SSE 字节，"零响应字节"即为证明量。
func TestGatewayMock_StreamRequestWritesOnlyMockBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newGatewayMockEnv(t)
	startGatewayMockRules(t, env.settingRepo, "hi", "本地回复")

	w := env.serve(t, "/v1/messages", gatewayMockMessagesBody("hi", true))

	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, upstreamPaths(env.capture), "命中 mock 时上游零请求")
	require.Equal(t, service.GatewayMockReplyHeaderValue, w.Header().Get(service.GatewayMockReplyHeader))
	require.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))

	body := w.Body.String()
	require.Contains(t, body, "event: message_stop", "流式 mock 必须是完整终止的事件流")
	require.NotContains(t, body, "upstream", "不得把上游内容混进本地回复")
}

// ---------------------------------------------------------------------------
// 分组平台是 Gemini 的 /v1/messages
//
// 路由 /v1/messages 按分组平台分派：Gemini 分组不会走 OpenAI 承接路径，而是进
// GatewayHandler.Messages 的 Gemini 分支（选号后交给 geminiCompatService /
// ForwardGemini）。该入口承接的下游协议仍然是 Anthropic Messages，mock 判定只能
// 依据下游协议，不能因为选中的账号平台不是 Anthropic 就漏拦——否则同一份下游
// 请求会因为分组平台不同而时而命中、时而打到上游。
// ---------------------------------------------------------------------------

// gatewayMockSlotCountingCache 在既有假并发缓存之上记录账号槽位的获取/释放次数，
// 用来证明命中 mock 的返回路径真的归还了它占用的账号槽位（并发额度不泄漏）。
type gatewayMockSlotCountingCache struct {
	fakeConcurrencyCache

	mu       sync.Mutex
	acquired int
	released int
}

func (c *gatewayMockSlotCountingCache) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	c.mu.Lock()
	c.acquired++
	c.mu.Unlock()
	return true, nil
}

func (c *gatewayMockSlotCountingCache) ReleaseAccountSlot(context.Context, int64, string) error {
	c.mu.Lock()
	c.released++
	c.mu.Unlock()
	return nil
}

// slotCounts 返回账号槽位的（获取次数, 释放次数）。
func (c *gatewayMockSlotCountingCache) slotCounts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acquired, c.released
}

// newGatewayMockGeminiEnv 装配 Gemini 分组的真实网关：账号 base_url 指向本地上游，
// 真实转发路径（geminiCompatService + repository.NewHTTPUpstream 的真实 RoundTrip）
// 也在链路上——未命中规则时请求会真的打到本地上游，命中时则一个请求都不发。
// 槽位计数与使用记录任务池同样注入，使「归还槽位、不提交使用记录」成为可观测事实。
func newGatewayMockGeminiEnv(t *testing.T) *gatewayMockEnv {
	t.Helper()

	capture := &requestTraceMatrixUpstream{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		capture.recordRequest(req, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"upstream"}]},` +
			`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true

	const groupID int64 = 7902
	const accountID int64 = 9402
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformGemini, Status: service.StatusActive}
	account := &service.Account{
		ID: accountID, Name: "mock-gemini-upstream", Platform: service.PlatformGemini,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Priority: 1, Concurrency: 1,
		Credentials:   map[string]any{"api_key": "sk-mock-gemini-credential", "base_url": upstream.URL},
		AccountGroups: []service.AccountGroup{{AccountID: accountID, GroupID: groupID}},
	}

	settingRepo := &gatewayMockSettingsRepo{values: map[string]string{}}
	settingService := service.NewSettingService(settingRepo, cfg)

	slots := &gatewayMockSlotCountingCache{}
	concurrencyService := service.NewConcurrencyService(slots)
	accountRepo := openAIImagesFailoverAccountRepo{accounts: []service.Account{*account}}
	rateLimit := service.NewRateLimitService(gateway429NoCooldownRepo{accountRepo}, nil, &config.Config{}, nil, nil)
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account}}, nil, nil, nil, nil)
	httpUpstream := repository.NewHTTPUpstream(cfg)
	gw := service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, nil,
		cfg, snapshot, concurrencyService, nil, rateLimit, nil, nil,
		httpUpstream,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	usagePool := service.NewUsageRecordWorkerPool(cfg)
	t.Cleanup(usagePool.Stop)

	handler := &GatewayHandler{
		gatewayService:           gw,
		geminiCompatService:      service.NewGeminiMessagesCompatService(accountRepo, nil, nil, snapshot, nil, rateLimit, httpUpstream, nil, cfg),
		billingCacheService:      billing,
		concurrencyHelper:        NewConcurrencyHelper(concurrencyService, SSEPingFormatClaude, 0),
		maxAccountSwitchesGemini: 10,
		cfg:                      cfg,
		settingService:           settingService,
		usageRecordWorkerPool:    usagePool,
	}
	return &gatewayMockEnv{
		handler: handler, capture: capture, settingRepo: settingRepo, settings: settingService,
		group: group, groupID: groupID, apiKeyID: 32, userID: 42,
		slots: slots, usagePool: usagePool,
	}
}

// TestGatewayMock_MessagesOnGeminiGroupPlatformReturnsLocalReply 是本切片的回归：
// 分组平台为 Gemini 的 /v1/messages 与其它分组平台上的同一下游请求行为一致——
// 命中管理员规则时返回本地 mock、上游零请求、归还账号槽位、不提交使用记录。
func TestGatewayMock_MessagesOnGeminiGroupPlatformReturnsLocalReply(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newGatewayMockGeminiEnv(t)
	startGatewayMockRules(t, env.settingRepo, "hi", "你好！这是本地测试回复。")

	w := env.serve(t, "/v1/messages", gatewayMockMessagesBody("  HI  ", false))

	require.Equal(t, service.GatewayMockReplyHeaderValue, w.Header().Get(service.GatewayMockReplyHeader),
		"Gemini 分组平台上的 Messages 命中规则时必须由本地 mock 应答")
	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, upstreamPaths(env.capture), "命中本地 mock 时不得向上游发出任何请求")

	var response map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, "assistant", response["role"])
	content, ok := response["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)
	block, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "你好！这是本地测试回复。", block["text"])
	usage, ok := response["usage"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(0), usage["input_tokens"], "本地 mock 不得编造输入用量")
	require.Equal(t, float64(0), usage["output_tokens"], "本地 mock 不得编造输出用量")

	// 账号槽位必须在返回前归还：获取与释放必须成对，不能把并发额度留给下一次请求。
	acquired, released := env.slots.slotCounts()
	require.Equal(t, 1, acquired, "真实选号路径必须经并发缓存获取账号槽位，槽位计数才有意义")
	require.Equal(t, acquired, released, "命中 mock 返回前必须安全归还账号槽位")

	// 本地 mock 不得进入计量路径：没有上游尝试，就不能有使用记录任务。
	require.Zero(t, env.usagePool.Stats().SubmittedTasks, "本地 mock 不得提交任何使用记录任务")
}

// TestGatewayMock_UnmatchedGeminiGroupRequestStillReachesUpstream 是上一用例的对照：
// 同一 Gemini 分组的真实链路上，未命中规则的请求必须照常转发到上游，并进入计量
// 路径。没有这条对照，"命中时上游零请求 / 无使用记录"完全可能只是因为这条链路上
// 根本发不出请求。
func TestGatewayMock_UnmatchedGeminiGroupRequestStillReachesUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newGatewayMockGeminiEnv(t)
	startGatewayMockRules(t, env.settingRepo, "hi", "你好！")

	w := env.serve(t, "/v1/messages", gatewayMockMessagesBody("你好今天天气怎么样", false))

	require.Equal(t, http.StatusOK, w.Code, "未命中的请求必须由真实上游正常应答: %s", w.Body.String())
	require.NotEmpty(t, upstreamPaths(env.capture), "带实质任务的请求必须继续访问上游")
	require.Empty(t, w.Header().Get(service.GatewayMockReplyHeader))

	acquired, released := env.slots.slotCounts()
	require.Equal(t, acquired, released, "真实转发返回后同样不得泄漏账号槽位")
	require.NotZero(t, env.usagePool.Stats().SubmittedTasks, "真实转发的请求必须进入计量路径")
}
