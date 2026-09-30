//go:build unit

package handler

// Chat Completions 入口在选号失败时的客户端模型范围事实。
//
// 采集范围按**客户端请求模型**复核，而这个事实在解析请求体成功后就已经可确定，
// 与是否选到账号无关。若处理器把「记录客户端模型」推迟到选到账号之后，那么
// OpenAIGatewayHandler.ChatCompletions 在选号失败（无可用账号）时就会把已经拿到
// 的模型名丢掉：仅指定/排除指定模型的范围复核会按"模型未知"处理，于是这条
// 没有使用记录的错误 Trace 根本不会落库——正是规格要求要能按客户端模型检索的
// 那类失败链路。
//
// 本文件用真实处理器 + 真实 service + 本地 httptest 上游（实际上永远不被调用）
// 复现该路径：分组里没有任何可调度账号，鉴权与请求体解析照常完成，然后在选号处
// 失败。只断言 Trace 采集结论，不访问任何生产上游。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const requestTraceCCNoAccountModel = "gpt-5.1"

// requestTraceCCNoAccountRepo 是分组内没有任何可调度账号的账号仓储。
// 真实选号失败后处理器还会走模型可用性诊断，因此这里必须覆盖该查询
// （Embedded AccountRepository 只提供其余未被真实路径触达的方法）。
type requestTraceCCNoAccountRepo struct {
	openAIImagesFailoverAccountRepo
}

func (requestTraceCCNoAccountRepo) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]service.Account, error) {
	return nil, nil
}

// newRequestTraceCCNoAccountHandler 装配真实 OpenAI 网关：账号池为空，上游不可达
// 也无所谓——这条路径在向上游发出前就失败。
func newRequestTraceCCNoAccountHandler(t *testing.T, group *service.Group, cfg *config.Config) *OpenAIGatewayHandler {
	t.Helper()
	gatewayService := service.NewOpenAIGatewayService(
		requestTraceCCNoAccountRepo{},
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		service.NewConcurrencyService(&fakeConcurrencyCache{}),
		billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil, cfg,
	)
	handler.maxAccountSwitches = 10
	return handler
}

// serveChatCompletionsWithScope 复刻生产中间件链：Trace 采集（带显式采集范围）-> 鉴权
// 注入 -> Trace 绑定 -> 真实 Chat Completions 入口。
func serveChatCompletionsWithScope(
	t *testing.T,
	handler *OpenAIGatewayHandler,
	group *service.Group,
	scope service.RequestTraceSettings,
	body string,
) (*httptest.ResponseRecorder, *finalizingRequestTraceRepoStub, *service.RequestTraceCaptureQueue) {
	t.Helper()

	gate := func(context.Context) service.RequestTraceGate {
		resolved := scope
		resolved.Enabled = true
		resolved.RiskAcknowledged = true
		return service.RequestTraceGate{CaptureAllowed: true, Scope: resolved}
	}

	repo := &finalizingRequestTraceRepoStub{finalized: make(chan service.RequestTraceCaptureState, 1)}
	queue := service.NewRequestTraceCaptureQueue(repo)
	t.Cleanup(queue.Stop)

	groupID := group.ID
	r := gin.New()
	r.POST("/v1/chat/completions",
		RequestTraceCaptureMiddleware(gate, repo, queue),
		func(c *gin.Context) {
			apiKey := &service.APIKey{
				ID: 41, UserID: 51, GroupID: &groupID, Status: service.StatusActive,
				User: &service.User{ID: 51, Concurrency: 10, Balance: 100}, Group: group,
			}
			c.Set(string(middleware.ContextKeyAPIKey), apiKey)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 51, Concurrency: 10})
			c.Next()
		},
		BindRequestTraceAfterAuth(),
		handler.ChatCompletions,
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w, repo, queue
}

// requireRequestTraceNotCaptured 断言这次请求完全没有进入采集：范围复核在中间件
// 里同步完成，因此这里看守的是"队列上永远不会出现任何写入"，而不是"此刻恰好还没写完"。
func requireRequestTraceNotCaptured(t *testing.T, repo *finalizingRequestTraceRepoStub, queue *service.RequestTraceCaptureQueue) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		stats := queue.Stats()
		if stats.Stored+stats.WriteFailed+stats.Dropped+stats.Rejected > 0 {
			repo.mu.Lock()
			defer repo.mu.Unlock()
			require.FailNowf(t, "unexpected trace capture",
				"a request outside the configured scope must not be queued (stats=%+v, traces=%d)",
				stats, len(repo.traces))
		}
		runtime.Gosched()
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Empty(t, repo.traces, "no trace may be persisted for an out-of-scope request")
}

// TestRequestTraceChatCompletionsNoAccountKeepsClientModelInScope 是本次修复的核心：
// 选号失败时，已解析出的客户端模型仍须参与采集范围复核，使"仅指定模型"的范围能
// 命中这条无使用记录的错误 Trace，并把该模型作为请求时事实落库。
func TestRequestTraceChatCompletionsNoAccountKeepsClientModelInScope(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{RunMode: config.RunModeSimple}
	const groupID int64 = 7831
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformOpenAI, Status: service.StatusActive}
	handler := newRequestTraceCCNoAccountHandler(t, group, cfg)

	body := `{"model":"` + requestTraceCCNoAccountModel + `","stream":false,` +
		`"messages":[{"role":"user","content":"hello"}]}`

	// 仅指定模型：范围包含客户端实际请求的模型。
	w, repo, queue := serveChatCompletionsWithScope(t, handler, group, service.RequestTraceSettings{
		AllGroups:     true,
		ModelScope:    service.RequestTraceScopeInclude,
		Models:        []string{requestTraceCCNoAccountModel},
		PlatformScope: service.RequestTraceScopeAll,
	}, body)

	require.GreaterOrEqual(t, w.Code, http.StatusBadRequest,
		"a request with no available account must fail: %s", w.Body.String())

	requestTraceMatrixWaitQueueDrained(t, queue)

	repo.mu.Lock()
	traces := append([]service.RequestTrace(nil), repo.traces...)
	repo.mu.Unlock()

	require.Len(t, traces, 1,
		"the client model is already determinable when selection fails, so a model-scoped trace must still be captured")
	trace := traces[0]
	require.Equal(t, requestTraceCCNoAccountModel, trace.RequestedModel,
		"the determinable client model must be stored as a request-time fact, not dropped")
	require.Equal(t, service.RequestTraceChatCompletions, trace.RouteFamily)
	require.Equal(t, "/v1/chat/completions", trace.InboundEndpoint)
	require.Equal(t, w.Code, trace.ClientStatus)
	require.NotEqual(t, service.RequestTraceStored, trace.CaptureState,
		"no upstream attempt was observed, so the trace must not claim full coverage")
}

// TestRequestTraceChatCompletionsNoAccountModelScopeStillFilters 是上一条的正向对照：
// 范围只包含另一个模型时，同一个失败请求不得被采集——采集结论确实按客户端模型
// 判定，而不是"鉴权后一律采集"。
func TestRequestTraceChatCompletionsNoAccountModelScopeStillFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{RunMode: config.RunModeSimple}
	const groupID int64 = 7832
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformOpenAI, Status: service.StatusActive}
	handler := newRequestTraceCCNoAccountHandler(t, group, cfg)

	body := `{"model":"` + requestTraceCCNoAccountModel + `","stream":false,` +
		`"messages":[{"role":"user","content":"hello"}]}`

	w, repo, queue := serveChatCompletionsWithScope(t, handler, group, service.RequestTraceSettings{
		AllGroups:     true,
		ModelScope:    service.RequestTraceScopeInclude,
		Models:        []string{"some-other-model"},
		PlatformScope: service.RequestTraceScopeAll,
	}, body)

	require.GreaterOrEqual(t, w.Code, http.StatusBadRequest, "the request must still fail: %s", w.Body.String())

	requireRequestTraceNotCaptured(t, repo, queue)
}

// TestRequestTraceChatCompletionsNoAccountAllModelsKeepsClientModel 覆盖"所有模型"范围：
// 该范围本就覆盖模型未知的请求，但已经确定的客户端模型仍必须作为请求时事实落库，
// 否则 Trace 页面无法按原请求模型检索这条无使用记录的错误。
func TestRequestTraceChatCompletionsNoAccountAllModelsKeepsClientModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{RunMode: config.RunModeSimple}
	const groupID int64 = 7833
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformOpenAI, Status: service.StatusActive}
	handler := newRequestTraceCCNoAccountHandler(t, group, cfg)

	body := `{"model":"` + requestTraceCCNoAccountModel + `","stream":false,` +
		`"messages":[{"role":"user","content":"hello"}]}`

	w, repo, queue := serveChatCompletionsWithScope(t, handler, group, service.RequestTraceSettings{
		AllGroups:     true,
		ModelScope:    service.RequestTraceScopeAll,
		PlatformScope: service.RequestTraceScopeAll,
	}, body)

	require.GreaterOrEqual(t, w.Code, http.StatusBadRequest, "the request must still fail: %s", w.Body.String())

	requestTraceMatrixWaitQueueDrained(t, queue)

	repo.mu.Lock()
	traces := append([]service.RequestTrace(nil), repo.traces...)
	repo.mu.Unlock()

	require.Len(t, traces, 1, "an all-models scope keeps the early failure trace")
	require.Equal(t, requestTraceCCNoAccountModel, traces[0].RequestedModel,
		"the client model must be kept even though the scope covers unknown models")
}
