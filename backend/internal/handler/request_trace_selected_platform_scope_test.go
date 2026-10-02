//go:build unit

package handler

// GatewayHandler.ChatCompletions / GatewayHandler.Responses 在"已选中账号、但拿不到
// 并发槽位"时的平台范围事实。
//
// 规格（`docs/gateway-mock-trace-scope-export-account-grants-spec-20260929.md` 2.2）
// 要求：**已实际选中具体账号的平台是已知事实，即使在向上游发出前就失败**；平台范围
// 按首个可确定的实际选中平台复核整条逻辑请求。因此"记录选中平台"必须紧跟在选号
// 成功之后——与并发准入、利润终检、下游测试请求 mock 是否通过无关。
//
// 若处理器把这一步推迟到准入/veto 之后（例如只挂在"即将发出上游"的那段），那么
// 账号已选中、槽位等待却超时的请求会把已经拿到的平台事实丢掉：仅指定/排除指定平台
// 的复核只能按"平台未知"处理，于是这条没有使用记录的错误 Trace 根本不会落库——
// 正是规格要求"按任一次实际选中平台可检索"的那类失败链路。
//
// 本文件用真实处理器 + 真实 service + 真实 HTTPUpstream（账号槽位全部占满，上游
// 永远不会被调用）复现该路径：鉴权、请求体解析与选号照常完成，然后在账号并发槽位
// 处超时失败。只断言 Trace 采集结论，不访问任何生产上游。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
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

const requestTraceSelectedPlatformScopeModel = "claude-sonnet-4-5"

// requestTraceNoAccountSlotCache 让账号槽位永远抢不到（返回未获取而不是错误，
// 与真实满载一致），同时保留用户槽位与等待计数的可用性：请求能通过用户级准入，
// 卡在账号级槽位上超时失败。
type requestTraceNoAccountSlotCache struct {
	*fakeConcurrencyCache
}

func (requestTraceNoAccountSlotCache) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	return false, nil
}

// newRequestTraceSelectedPlatformScopeHandler 装配真实 Anthropic 网关：账号可调度、
// 但并发槽位全部占满，因此选号会成功返回一个等待计划，随后槽位等待超时。
func newRequestTraceSelectedPlatformScopeHandler(t *testing.T, group *service.Group, account *service.Account, cfg *config.Config) *GatewayHandler {
	t.Helper()
	accountRepo := openAIImagesFailoverAccountRepo{accounts: []service.Account{*account}}
	rateLimit := service.NewRateLimitService(gateway429NoCooldownRepo{accountRepo}, nil, &config.Config{}, nil, nil)
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account}}, nil, nil, nil, nil)
	concurrency := service.NewConcurrencyService(requestTraceNoAccountSlotCache{&fakeConcurrencyCache{}})
	gw := service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, nil,
		cfg, snapshot, concurrency, nil, rateLimit, nil, nil,
		// 真实 HTTPUpstream：这条路径的 trace 接缝与其它用例完全相同。
		repository.NewHTTPUpstream(cfg),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	return &GatewayHandler{
		gatewayService:      gw,
		billingCacheService: billing,
		concurrencyHelper:   NewConcurrencyHelper(concurrency, SSEPingFormatClaude, 0),
		maxAccountSwitches:  10,
		cfg:                 cfg,
	}
}

// requestTraceGateFromLegacyScope 用读取侧缺省补齐手工构造的范围：这些夹具只关心
// 分组/模型/平台范围，新采集内容字段按缺省（正文开启、200 开启、两类 100%、1 MiB），
// 避免零值被读成"正文关闭 + 0% 采样"而让夹具意外丢掉全部 Trace。
func requestTraceGateFromLegacyScope(scope service.RequestTraceSettings) service.RequestTraceGate {
	scope.Enabled = true
	scope.RiskAcknowledged = true
	scope.CaptureBody = true
	scope.CaptureHTTP200 = true
	if scope.SampleRateHTTP200 == 0 {
		scope.SampleRateHTTP200 = 100
	}
	if scope.SampleRateOther == 0 {
		scope.SampleRateOther = 100
	}
	if scope.BodyMaxBytes == 0 {
		scope.BodyMaxBytes = service.RequestTraceBodyLimit
	}
	if scope.ModelScope == "" {
		scope.ModelScope = service.RequestTraceScopeAll
	}
	if scope.PlatformScope == "" {
		scope.PlatformScope = service.RequestTraceScopeAll
	}
	return service.RequestTraceGate{CaptureAllowed: true, Scope: scope}
}

// serveChatCompletionsOrResponsesWithScope 复刻生产中间件链：Trace 采集（带显式采集
// 范围）-> 鉴权注入 -> Trace 绑定 -> 真实入口。
func serveSelectedPlatformScopeRequest(
	t *testing.T,
	handler *GatewayHandler,
	group *service.Group,
	scope service.RequestTraceSettings,
	path string,
	body string,
	entry gin.HandlerFunc,
) (*httptest.ResponseRecorder, *finalizingRequestTraceRepoStub, *service.RequestTraceCaptureQueue) {
	t.Helper()

	gate := func(context.Context) service.RequestTraceGate {
		return requestTraceGateFromLegacyScope(scope)
	}

	repo := &finalizingRequestTraceRepoStub{finalized: make(chan service.RequestTraceCaptureState, 1)}
	queue := service.NewRequestTraceCaptureQueue(repo)
	t.Cleanup(queue.Stop)

	groupID := group.ID
	r := gin.New()
	r.POST(path,
		RequestTraceCaptureMiddleware(gate, repo, queue),
		func(c *gin.Context) {
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, group))
			apiKey := &service.APIKey{
				ID: 61, UserID: 71, GroupID: &groupID, Status: service.StatusActive,
				User: &service.User{ID: 71, Concurrency: 10, Balance: 100}, Group: group,
			}
			c.Set(string(middleware.ContextKeyAPIKey), apiKey)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 71, Concurrency: 10})
			c.Next()
		},
		BindRequestTraceAfterAuth(),
		entry,
	)

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w, repo, queue
}

// requireSlotWaitFailure 断言这次请求真的走到了"账号已选中、账号并发槽位等待超时"
// 这一步：失败原因必须是账号槽位，而不是更早的鉴权、解析或选号失败，否则这条用例
// 证明不了平台事实在选号之后已经被拿到。
func requireSlotWaitFailure(t *testing.T, w *httptest.ResponseRecorder, entry string) {
	t.Helper()
	require.Equal(t, http.StatusTooManyRequests, w.Code,
		"%s: the selected account never got a slot, so the request must fail on the account concurrency limit: %s",
		entry, w.Body.String())
	require.Contains(t, w.Body.String(), gatewayConcurrencyLimitCode,
		"%s: the failure must be the account slot wait, not an earlier rejection: %s", entry, w.Body.String())
}

// requireSelectedPlatformScopeTraceCaptured 等到这次请求的 Trace 真的落库，并断言它
// 恰好是一条、没有伪造任何上游尝试（这次请求从未发出上游），且信封上的请求时平台历史
// 就是 expectedPlatforms（去重、按首次观察顺序）。
//
// expectedPlatforms 是本次修复的核心断言点：只把平台写进上下文供采集门控用是不够的，
// 平台还必须作为信封事实落库，否则列表/导出按"任一实际选中平台"检索时这条没有上游尝试
// 的 Trace 既匹配不上具体平台、又会被 platform_unknown 当成未知（规格 §2.3）。
func requireSelectedPlatformScopeTraceCaptured(
	t *testing.T,
	w *httptest.ResponseRecorder,
	repo *finalizingRequestTraceRepoStub,
	queue *service.RequestTraceCaptureQueue,
	family service.RequestTraceRouteFamily,
	endpoint string,
	expectedPlatforms []string,
) service.RequestTrace {
	t.Helper()

	// 队列先写信封、再逐条写阶段：等到阶段也写完，才能断言"没有伪造尝试"。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats := queue.Stats()
		repo.mu.Lock()
		count, stageCount := len(repo.traces), len(repo.stages)
		repo.mu.Unlock()
		if (count >= 1 && stageCount >= 3) || stats.WriteFailed+stats.Dropped+stats.Rejected > 0 {
			break
		}
		runtime.Gosched()
	}

	repo.mu.Lock()
	traces := append([]service.RequestTrace(nil), repo.traces...)
	stages := append([]service.RequestTraceStage(nil), repo.stages...)
	repo.mu.Unlock()

	require.Len(t, traces, 1,
		"the account was really selected before the slot failure, so its platform is a known fact "+
			"and an in-scope error trace must still be captured (queue stats=%+v)", queue.Stats())
	trace := traces[0]
	require.Equal(t, family, trace.RouteFamily)
	require.Equal(t, endpoint, trace.InboundEndpoint)
	require.Equal(t, w.Code, trace.ClientStatus)
	require.Nil(t, trace.UsageLogID, "a slot failure has no usage record")
	require.NotEqual(t, service.RequestTraceStored, trace.CaptureState,
		"no upstream attempt was observed, so the trace must not claim full coverage")
	require.Equal(t, expectedPlatforms, trace.ObservedPlatforms,
		"the envelope must carry every platform that was actually selected, including one whose "+
			"account was selected but never sent to upstream: it is what the platform filter matches and "+
			"what keeps the trace out of platform_unknown")

	var fabricated, genericGap bool
	for _, stage := range stages {
		if stage.Stage != "wire_attempt" {
			continue
		}
		if stage.Metadata != nil && stage.Metadata.Platform != "" {
			fabricated = true
		}
		if stage.State == service.RequestTraceNotObserved && stage.Reason == "attempt_not_observed" {
			genericGap = true
		}
	}
	require.False(t, fabricated,
		"no upstream attempt was sent, so no wire_attempt platform fact may be fabricated")
	require.True(t, genericGap,
		"a request that never sent an upstream attempt keeps the generic attempt_not_observed gap")
	return trace
}

// requireSelectedPlatformScopeNotCaptured 断言这次请求完全没有进入采集。
func requireSelectedPlatformScopeNotCaptured(
	t *testing.T,
	repo *finalizingRequestTraceRepoStub,
	queue *service.RequestTraceCaptureQueue,
) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		stats := queue.Stats()
		if stats.Stored+stats.WriteFailed+stats.Dropped+stats.Rejected > 0 {
			repo.mu.Lock()
			defer repo.mu.Unlock()
			require.FailNowf(t, "unexpected trace capture",
				"a request outside the configured platform scope must not be queued (stats=%+v, traces=%d)",
				stats, len(repo.traces))
		}
		runtime.Gosched()
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Empty(t, repo.traces, "no trace may be persisted for an out-of-scope request")
}

// newSelectedPlatformScopeEnv 组装 Anthropic 分组的账号与处理器：账号可调度但槽位满载。
func newSelectedPlatformScopeEnv(t *testing.T, groupID, accountID int64) (*service.Group, *GatewayHandler) {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	group := &service.Group{
		ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive,
	}
	account := &service.Account{
		ID: accountID, Name: "selected-but-slotless", Platform: service.PlatformAnthropic,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Priority: 1, Concurrency: 1,
		Credentials:   map[string]any{"api_key": "sk-selected-but-slotless", "base_url": "http://127.0.0.1:1"},
		AccountGroups: []service.AccountGroup{{AccountID: accountID, GroupID: groupID}},
	}
	return group, newRequestTraceSelectedPlatformScopeHandler(t, group, account, cfg)
}

// TestRequestTraceSelectedPlatformScopeChatCompletionsKeepsSlotFailure 是本次修复的核心：
// Chat Completions 入口在"已选中账号、槽位等待超时"失败时，选中账号的平台仍须参与
// 采集范围复核，使"仅指定平台"的范围能命中这条无使用记录的错误 Trace。
func TestRequestTraceSelectedPlatformScopeChatCompletionsKeepsSlotFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	group, handler := newSelectedPlatformScopeEnv(t, 7841, 9501)

	body := `{"model":"` + requestTraceSelectedPlatformScopeModel + `","stream":false,` +
		`"messages":[{"role":"user","content":"hello"}]}`

	w, repo, queue := serveSelectedPlatformScopeRequest(t, handler, group, service.RequestTraceSettings{
		AllGroups:     true,
		ModelScope:    service.RequestTraceScopeAll,
		PlatformScope: service.RequestTraceScopeInclude,
		Platforms:     []string{service.PlatformAnthropic},
	}, "/v1/chat/completions", body, handler.ChatCompletions)

	requireSlotWaitFailure(t, w, "chat completions")

	requireSelectedPlatformScopeTraceCaptured(t, w, repo, queue,
		service.RequestTraceChatCompletions, "/v1/chat/completions",
		[]string{service.PlatformAnthropic})
}

// TestRequestTraceSelectedPlatformScopeResponsesKeepsSlotFailure 覆盖 /v1/responses
// 上的同一接缝：槽位失败同样发生在选号之后、发出上游之前。
func TestRequestTraceSelectedPlatformScopeResponsesKeepsSlotFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	group, handler := newSelectedPlatformScopeEnv(t, 7842, 9502)

	body := `{"model":"` + requestTraceSelectedPlatformScopeModel + `","stream":false,"input":"hello"}`

	w, repo, queue := serveSelectedPlatformScopeRequest(t, handler, group, service.RequestTraceSettings{
		AllGroups:     true,
		ModelScope:    service.RequestTraceScopeAll,
		PlatformScope: service.RequestTraceScopeInclude,
		Platforms:     []string{service.PlatformAnthropic},
	}, "/v1/responses", body, handler.Responses)

	requireSlotWaitFailure(t, w, "responses")

	requireSelectedPlatformScopeTraceCaptured(t, w, repo, queue,
		service.RequestTraceResponses, "/v1/responses",
		[]string{service.PlatformAnthropic})
}

// TestRequestTraceSelectedPlatformScopeKeepsFirstPlatformAcrossFailover 固定"首次可确定
// 平台"在换号后不回退：同一个入口先选中一个平台、该账号失败后换号又选中另一个平台时，
// 采集结论仍按第一次选中的平台判定。这正是把记录点提前到选号成功之后必须保持的
// 不变式——第二次标记不得覆盖第一次，否则"仅指定平台"的范围会按最后一次尝试判定。
func TestRequestTraceSelectedPlatformScopeKeepsFirstPlatformAcrossFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)

	group := &service.Group{
		ID: 7851, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive,
	}
	body := `{"model":"` + requestTraceSelectedPlatformScopeModel + `","stream":false,` +
		`"messages":[{"role":"user","content":"hello"}]}`

	// 入口不复用真实处理器：这里只驱动"选到账号 -> 记录平台 -> 换号 -> 又选到账号"
	// 这一串标记，处理器本身已由上面两条用例覆盖。
	failoverEntry := func(c *gin.Context) {
		markRequestTraceSelectedPlatform(c, service.PlatformAntigravity)
		markRequestTraceSelectedPlatform(c, service.PlatformGemini)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "api_error", "message": "no available accounts"}})
	}

	t.Run("首次平台在范围内则采集", func(t *testing.T) {
		w, repo, queue := serveSelectedPlatformScopeRequest(t, nil, group, service.RequestTraceSettings{
			AllGroups:     true,
			ModelScope:    service.RequestTraceScopeAll,
			PlatformScope: service.RequestTraceScopeInclude,
			Platforms:     []string{service.PlatformAntigravity},
		}, "/v1/chat/completions", body, failoverEntry)

		requireSelectedPlatformScopeTraceCaptured(t, w, repo, queue,
			service.RequestTraceChatCompletions, "/v1/chat/completions",
			[]string{service.PlatformAntigravity, service.PlatformGemini})
	})

	t.Run("换号后的平台不参与判定", func(t *testing.T) {
		w, repo, queue := serveSelectedPlatformScopeRequest(t, nil, group, service.RequestTraceSettings{
			AllGroups:     true,
			ModelScope:    service.RequestTraceScopeAll,
			PlatformScope: service.RequestTraceScopeInclude,
			Platforms:     []string{service.PlatformGemini},
		}, "/v1/chat/completions", body, failoverEntry)

		require.Equal(t, http.StatusServiceUnavailable, w.Code, "%s", w.Body.String())

		requireSelectedPlatformScopeNotCaptured(t, repo, queue)
	})
}

// TestRequestTraceSelectedPlatformScopeChatCompletionsFiltersOtherPlatform 是上一条的
// 正向对照：范围只包含另一个平台时，同一条槽位失败请求不得被采集——结论确实按
// 实际选中账号的平台判定，而不是"选到账号就一律采集"。
func TestRequestTraceSelectedPlatformScopeChatCompletionsFiltersOtherPlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)

	group, handler := newSelectedPlatformScopeEnv(t, 7843, 9503)

	body := `{"model":"` + requestTraceSelectedPlatformScopeModel + `","stream":false,` +
		`"messages":[{"role":"user","content":"hello"}]}`

	w, repo, queue := serveSelectedPlatformScopeRequest(t, handler, group, service.RequestTraceSettings{
		AllGroups:     true,
		ModelScope:    service.RequestTraceScopeAll,
		PlatformScope: service.RequestTraceScopeInclude,
		Platforms:     []string{service.PlatformOpenAI},
	}, "/v1/chat/completions", body, handler.ChatCompletions)

	requireSlotWaitFailure(t, w, "chat completions")

	requireSelectedPlatformScopeNotCaptured(t, repo, queue)
}
