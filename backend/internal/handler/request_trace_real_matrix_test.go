//go:build unit

package handler

// 真实处理器请求 Trace 覆盖矩阵（real-handler request Trace matrix）。
//
// 既有 request_trace_real_gateway_test.go 只覆盖了三条真实链路：
// GatewayHandler.Messages、OpenAIGatewayHandler.Responses 与
// OpenAIGatewayHandler.ChatCompletions。本文件补齐矩阵里剩下的三个真实处理器组合，
// 每条都走真实 handler -> 真实 service -> repository.NewHTTPUpstream 的真实
// RoundTrip -> 本地 httptest 上游：
//
//  1. GatewayHandler.ChatCompletions —— Anthropic 分组上的 /v1/chat/completions，
//     客户端 OpenAI Chat Completions 形状，出站是 Anthropic Messages。
//  2. GatewayHandler.Responses —— Anthropic 分组上的 /v1/responses，
//     客户端 OpenAI Responses 形状，出站是 Anthropic Messages。
//  3. OpenAIGatewayHandler.Messages —— OpenAI 分组上的 /v1/messages，
//     客户端 Anthropic Messages 形状，出站是 OpenAI Responses。
//
// 每个组合都断言同一组事实：恰好一条 Trace；RouteFamily/InboundEndpoint 与入站
// 路由一致；至少一个 wire_attempt 阶段，且它的协议是这条路径真实发出的协议；上游
// 响应体与最终下游体都被观察到；任何阶段（正文、URL、请求/响应头）都不得留下凭据
// canary。矩阵之外另有一条反例：真实处理器路径没有观察到任何真实 wire 尝试时，
// Trace 必须显式报告该缺口，不得声称已完整覆盖（不得终结为 stored）。
//
// 只使用本地 httptest 作为上游，不访问任何生产上游。所有 canary 都是本文件自造的常量。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
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

const (
	requestTraceMatrixModel       = "claude-sonnet-4-5"
	requestTraceMatrixOpenAIModel = "gpt-5.1"
)

// 阶段哨兵：分别证明入站正文、上游响应与最终下游响应被真的观察到。
const (
	requestTraceMatrixPromptCanary   = "TRACE_MATRIX_PROMPT_CANARY"
	requestTraceMatrixResponseCanary = "TRACE_MATRIX_RESPONSE_CANARY"

	requestTraceMatrixInboundHeaderCanary  = "matrix-inbound-visible"
	requestTraceMatrixUpstreamHeaderCanary = "matrix-upstream-visible"
)

// 凭据 canary：任何一个出现在任何阶段的正文、URL 或头里都算泄漏。
const (
	requestTraceMatrixAccountCredential    = "sk-matrix-account-CREDENTIAL_CANARY"
	requestTraceMatrixInboundAuthCanary    = "TRACE_MATRIX_INBOUND_AUTH_CANARY"
	requestTraceMatrixInboundQueryCanary   = "TRACE_MATRIX_INBOUND_QUERY_CANARY"
	requestTraceMatrixBodyCredentialCanary = "TRACE_MATRIX_BODY_CREDENTIAL_CANARY"
)

var requestTraceMatrixCredentials = map[string]string{
	"account credential":  requestTraceMatrixAccountCredential,
	"inbound auth header": requestTraceMatrixInboundAuthCanary,
	"inbound query key":   requestTraceMatrixInboundQueryCanary,
	"body credential":     requestTraceMatrixBodyCredentialCanary,
}

// requestTraceMatrixAnthropicSSE 是 Anthropic Messages 事件流。Anthropic 分组上的
// Chat Completions / Responses 入口都会把上游请求强制成 stream=true，即使客户端
// stream=false，所以本地上游必须真的按事件流作答。
//
// 末尾必须留下分隔事件用的空行：采集器会拒绝把没有完整终止的 SSE 事件当作已观察
// 事件，否则上游响应阶段会如实标成 truncated/incomplete_event。
func requestTraceMatrixAnthropicSSE() string {
	return strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_matrix","type":"message","role":"assistant","content":[],"model":"` +
			requestTraceMatrixModel + `","stop_reason":"","usage":{"input_tokens":5}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` +
			requestTraceMatrixResponseCanary + `"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
		// 终止空行：最后一个事件同样需要以空行结束，否则它不算完整事件。
		``,
	}, "\n")
}

// requestTraceMatrixResponsesSSE 是 OpenAI Responses 事件流。OpenAI 分组上的
// /v1/messages 会把上游请求强制成 stream=true，缓冲路径要求看到终止事件。
func requestTraceMatrixResponsesSSE() string {
	return "data: " + `{"type":"response.completed","response":{"id":"resp_matrix","object":"response",` +
		`"created_at":1700000000,"model":"` + requestTraceMatrixOpenAIModel + `","status":"completed",` +
		`"output":[{"type":"message","id":"msg_matrix","status":"completed","role":"assistant",` +
		`"content":[{"type":"output_text","text":"` + requestTraceMatrixResponseCanary + `","annotations":[]}]}],` +
		`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
}

// requestTraceMatrixUpstream 在既有记录器之上再记下真实出站请求的认证头。用途是
// 正向对照：只有先证明账号凭据真的在链路上出现过，"任何阶段都没有凭据 canary"才
// 不是一句空跑。
type requestTraceMatrixUpstream struct {
	requestTraceRealUpstream

	mu      sync.Mutex
	headers []http.Header
	bodies  [][]byte
}

func (u *requestTraceMatrixUpstream) recordRequest(req *http.Request, body []byte) {
	u.requestTraceRealUpstream.record(req, body)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.headers = append(u.headers, req.Header.Clone())
	u.bodies = append(u.bodies, append([]byte(nil), body...))
}

// carried 报告某个值是否真的出现在过出站请求的头或正文里。
func (u *requestTraceMatrixUpstream) carried(value string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, header := range u.headers {
		for _, values := range header {
			for _, item := range values {
				if strings.Contains(item, value) {
					return true
				}
			}
		}
	}
	for _, body := range u.bodies {
		if strings.Contains(string(body), value) {
			return true
		}
	}
	return false
}

// requestTraceMatrixEnv 是一次真实处理器矩阵运行所需的全部接线。
type requestTraceMatrixEnv struct {
	repo      *finalizingRequestTraceRepoStub
	queue     *service.RequestTraceCaptureQueue
	capture   *requestTraceMatrixUpstream
	anthropic *GatewayHandler
	openai    *OpenAIGatewayHandler
	group     *service.Group
	groupID   int64
	accountID int64
	apiKeyID  int64
	userID    int64
}

// newRequestTraceMatrixAnthropicEnv 装配 Anthropic 分组的真实网关：账号 base_url
// 指向本地 httptest 上游，该上游按 Anthropic 事件流作答。
func newRequestTraceMatrixAnthropicEnv(t *testing.T) *requestTraceMatrixEnv {
	t.Helper()

	capture := &requestTraceMatrixUpstream{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		capture.recordRequest(req, body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Upstream-Canary", requestTraceMatrixUpstreamHeaderCanary)
		_, _ = w.Write([]byte(requestTraceMatrixAnthropicSSE()))
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true

	const groupID int64 = 7801
	const accountID int64 = 9301
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	account := &service.Account{
		ID: accountID, Name: "matrix-anthropic-upstream", Platform: service.PlatformAnthropic,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Priority: 1, Concurrency: 1,
		Credentials:   map[string]any{"api_key": requestTraceMatrixAccountCredential, "base_url": upstream.URL},
		AccountGroups: []service.AccountGroup{{AccountID: accountID, GroupID: groupID}},
	}

	repo := &finalizingRequestTraceRepoStub{finalized: make(chan service.RequestTraceCaptureState, 1)}
	queue := service.NewRequestTraceCaptureQueue(repo)
	t.Cleanup(queue.Stop)

	return &requestTraceMatrixEnv{
		repo: repo, queue: queue, capture: capture,
		anthropic: newRequestTraceRealGatewayMessages(t, group, account, cfg),
		group:     group, groupID: groupID, accountID: accountID, apiKeyID: 11, userID: 21,
	}
}

// newRequestTraceMatrixOpenAIEnv 装配 OpenAI 分组的真实网关：账号 base_url 指向
// 本地 httptest 上游，该上游按 OpenAI Responses 事件流作答。
func newRequestTraceMatrixOpenAIEnv(t *testing.T) *requestTraceMatrixEnv {
	t.Helper()

	capture := &requestTraceMatrixUpstream{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		capture.recordRequest(req, body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Upstream-Canary", requestTraceMatrixUpstreamHeaderCanary)
		_, _ = w.Write([]byte(requestTraceMatrixResponsesSSE()))
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true

	const groupID int64 = 7811
	const accountID int64 = 9311
	// OpenAI 分组默认不允许 /v1/messages 调度，真实路由要先拿到这个开关。
	group := &service.Group{
		ID: groupID, Hydrated: true, Platform: service.PlatformOpenAI, Status: service.StatusActive,
		AllowMessagesDispatch: true,
	}
	accounts := []service.Account{{
		ID: accountID, Name: "matrix-openai-upstream", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Priority: 1, Concurrency: 1,
		Credentials: map[string]any{"api_key": requestTraceMatrixAccountCredential, "base_url": upstream.URL},
	}}

	repo := &finalizingRequestTraceRepoStub{finalized: make(chan service.RequestTraceCaptureState, 1)}
	queue := service.NewRequestTraceCaptureQueue(repo)
	t.Cleanup(queue.Stop)

	return &requestTraceMatrixEnv{
		repo: repo, queue: queue, capture: capture,
		openai: newRequestTraceRealGatewayOpenAI(t, accounts, cfg),
		group:  group, groupID: groupID, accountID: accountID, apiKeyID: 12, userID: 22,
	}
}

// requestTraceMatrixEmptyAccountRepo 没有任何可调度账号，但仍是完整的
// AccountRepository：真实路径在选号失败后还会调用模型可用性诊断，若沿用只覆盖
// 少数据方法的 stub，未被覆盖的嵌入接口会让这条真实路径直接 panic。
type requestTraceMatrixEmptyAccountRepo struct {
	openAIImagesFailoverAccountRepo
}

func (requestTraceMatrixEmptyAccountRepo) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]service.Account, error) {
	return nil, nil
}

// newRequestTraceMatrixGatewayWithoutAccounts 装配同一条真实链路，但没有任何可用
// 账号：真实路径会走完鉴权、解析与选号，然后在上游发出前失败。用于验证"没有观察到
// 真实 wire 尝试"同样是一条真实处理器路径。
func newRequestTraceMatrixGatewayWithoutAccounts(t *testing.T, group *service.Group, cfg *config.Config) *GatewayHandler {
	t.Helper()
	accountRepo := requestTraceMatrixEmptyAccountRepo{}
	rateLimit := service.NewRateLimitService(gateway429NoCooldownRepo{accountRepo.openAIImagesFailoverAccountRepo}, nil, &config.Config{}, nil, nil)
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{}, nil, nil, nil, nil)
	gw := service.NewGatewayService(
		accountRepo, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, nil,
		cfg, snapshot, nil, nil, rateLimit, nil, nil,
		// 真实 HTTPUpstream：这条路径的 trace 接缝与其它用例完全相同。
		repository.NewHTTPUpstream(cfg),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	return &GatewayHandler{
		gatewayService:      gw,
		billingCacheService: billing,
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0),
		maxAccountSwitches:  10,
		cfg:                 cfg,
	}
}

// serve 复刻生产中间件链：Trace 采集 -> 鉴权注入 -> Trace 绑定 -> 真实入口。
func (e *requestTraceMatrixEnv) serve(t *testing.T, path, body string, entry gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	group := e.group
	groupID := e.groupID
	apiKeyID := e.apiKeyID
	userID := e.userID

	r := gin.New()
	r.POST(path,
		RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return RequestTraceGateForCapture(true) }, e.repo, e.queue),
		func(c *gin.Context) {
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, group))
			apiKey := &service.APIKey{
				ID: apiKeyID, UserID: userID, GroupID: &groupID, Status: service.StatusActive,
				User: &service.User{ID: userID, Concurrency: 10, Balance: 100}, Group: group,
			}
			c.Set(string(middleware.ContextKeyAPIKey), apiKey)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: userID, Concurrency: 10})
			c.Next()
		},
		BindRequestTraceAfterAuth(),
		entry,
	)

	req := httptest.NewRequest(http.MethodPost,
		path+"?api_key="+requestTraceMatrixInboundQueryCanary+"&source=matrix", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+requestTraceMatrixInboundAuthCanary)
	req.Header.Set("X-Inbound-Canary", requestTraceMatrixInboundHeaderCanary)
	req.Header.Set("User-Agent", "matrix-client/1.0")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// requestTraceMatrixCase 是矩阵的一格：入站路由、真实处理器、以及这条路径在
// wire 上真实使用的协议。
type requestTraceMatrixCase struct {
	name         string
	path         string
	body         string
	entry        func(*requestTraceMatrixEnv) gin.HandlerFunc
	family       service.RequestTraceRouteFamily
	endpoint     string
	upstreamPath string
	protocol     string
	// attemptModel 为空表示不固定模型名，只要求事实里确实有模型。
	attemptModel string
	// counters 是本地上游真实作答里带有的用量计数，必须在最终下游体中以数值
	// 原样保留。名字里含 "token" 的计数不是凭据，采集器不得把它们擦掉：只有
	// "凭据被遮蔽"和"用量事实被保留"同时成立，保留的正文才是有用的采集结果。
	counters []string
}

// requestTraceMatrixStage 取唯一匹配的阶段，缺失或不唯一都直接失败。
func requestTraceMatrixStage(t *testing.T, stages []service.RequestTraceStage, stage, view string, attempt int) service.RequestTraceStage {
	t.Helper()
	var found []service.RequestTraceStage
	for _, item := range stages {
		if item.Stage == stage && item.View == view && item.AttemptIndex == attempt {
			found = append(found, item)
		}
	}
	require.Len(t, found, 1, "stage %q view %q attempt %d must be observed exactly once", stage, view, attempt)
	return found[0]
}

// requestTraceMatrixDescribe 把阶段序列渲染成一行，让"为什么没有终结为 stored"
// 的失败直接指向那个不诚实的阶段。
func requestTraceMatrixDescribe(stages []service.RequestTraceStage) string {
	parts := make([]string, 0, len(stages))
	for _, stage := range stages {
		parts = append(parts, stage.Stage+"/"+stage.View+"#"+strconv.Itoa(stage.AttemptIndex)+
			"="+string(stage.State)+"("+stage.Reason+")")
	}
	return strings.Join(parts, " ")
}

// requestTraceMatrixWaitQueueDrained 等到队列把一次逻辑请求的全部阶段写完
// （Stored 只在全部阶段落库成功后自增），避免读到半截 Trace。
func requestTraceMatrixWaitQueueDrained(t *testing.T, queue *service.RequestTraceCaptureQueue) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats := queue.Stats()
		if stats.Stored+stats.WriteFailed+stats.Dropped >= 1 {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("request trace queue never drained: %+v", queue.Stats())
}

// assertRequestTraceMatrixNoCredentialCanary 断言一个阶段的可观察事实里没有任何
// 凭据 canary：正文、脱敏后的 URL、以及脱敏后的请求/响应头。
func assertRequestTraceMatrixNoCredentialCanary(t *testing.T, label string, stage service.RequestTraceStage) {
	t.Helper()
	where := stage.Stage + "/" + stage.View
	for name, canary := range requestTraceMatrixCredentials {
		require.NotContains(t, string(stage.Payload), canary,
			"%s: %s leaked into the %s payload", label, name, where)
		if stage.Metadata == nil {
			continue
		}
		require.NotContains(t, stage.Metadata.URL, canary,
			"%s: %s leaked into the %s url", label, name, where)
		for header, values := range stage.Metadata.RequestHeaders {
			for _, value := range values {
				require.NotContains(t, value, canary,
					"%s: %s leaked into the %s request header %s", label, name, where, header)
			}
		}
		for header, values := range stage.Metadata.ResponseHeaders {
			for _, value := range values {
				require.NotContains(t, value, canary,
					"%s: %s leaked into the %s response header %s", label, name, where, header)
			}
		}
	}
}

// run 驱动矩阵的一格并断言该格的全部 Trace 事实。
func (e *requestTraceMatrixEnv) run(t *testing.T, tc requestTraceMatrixCase) {
	t.Helper()

	w := e.serve(t, tc.path, tc.body, tc.entry(e))
	require.Equal(t, http.StatusOK, w.Code, "%s: the real handler must answer the client: %s", tc.name, w.Body.String())

	// 真实上游只被调用一次，且真的收到了客户端提示词：wire 阶段不应是伪造的。
	paths, bodies := e.capture.snapshot()
	require.Equal(t, []string{tc.upstreamPath}, paths,
		"%s: the real path must call the local httptest upstream exactly once", tc.name)
	require.Len(t, bodies, 1)
	require.Contains(t, string(bodies[0]), requestTraceMatrixPromptCanary,
		"%s: the upstream wire bytes must carry the client prompt", tc.name)

	// 正向对照：账号凭据确实随这次真实出站请求上过线。没有这一步，"任何阶段都
	// 没有凭据 canary"完全可能是空跑——链路上本来就没有凭据可漏。
	require.True(t, e.capture.carried(requestTraceMatrixAccountCredential),
		"%s: the account credential must really be on the wire for the redaction sweep to mean anything", tc.name)

	select {
	case state := <-e.repo.finalized:
		require.Equal(t, service.RequestTraceStored, state,
			"%s: a fully observed real attempt must finalize as stored", tc.name)
	case <-time.After(2 * time.Second):
		e.repo.mu.Lock()
		observed := requestTraceMatrixDescribe(e.repo.stages)
		e.repo.mu.Unlock()
		t.Fatalf("%s: real trace was never finalized (queue stored=%d rejected=%d): %s",
			tc.name, e.queue.Stats().Stored, e.queue.Stats().Rejected, observed)
	}

	e.repo.mu.Lock()
	traces := append([]service.RequestTrace(nil), e.repo.traces...)
	stages := append([]service.RequestTraceStage(nil), e.repo.stages...)
	e.repo.mu.Unlock()

	// 一条逻辑请求恰好一条 Trace，且入站路由描述的是客户端真正打的路径。
	require.Len(t, traces, 1, "%s: one logical request must produce exactly one trace", tc.name)
	trace := traces[0]
	require.Equal(t, tc.family, trace.RouteFamily)
	require.Equal(t, tc.endpoint, trace.InboundEndpoint)
	// 入队时的信封刻意停留在 partial，直到全部阶段写完才由队列升级为 stored
	// （上面对 finalized 通道的断言正是那次升级），所以这里不得提前声称完整。
	require.Equal(t, service.RequestTracePartial, trace.CaptureState)
	require.Equal(t, http.StatusOK, trace.ClientStatus)
	require.Nil(t, trace.UsageLogID)

	var attempts []service.RequestTraceStage
	for _, stage := range stages {
		require.Equal(t, trace.TraceID, stage.TraceID,
			"%s: every stage must belong to the one trace of this logical request", tc.name)
		require.True(t, service.ValidRequestTraceStageReason(stage.Reason),
			"%s: stage %q must carry a bounded reason code, got %q", tc.name, stage.Stage, stage.Reason)
		if stage.Stage == "wire_attempt" {
			attempts = append(attempts, stage)
		}
		assertRequestTraceMatrixNoCredentialCanary(t, tc.name, stage)
	}

	// 请求阶段：入站正文被观察到，且保留的是客户端真的发来的字节。
	clientEntry := requestTraceMatrixStage(t, stages, "client_entry", "decoded", 0)
	require.Contains(t, string(clientEntry.Payload), requestTraceMatrixPromptCanary)
	require.Equal(t, service.RequestTraceStored, clientEntry.State)
	// 正向对照：入站正文里的结构化凭据字段被定点遮蔽，而不是整段正文被丢掉。
	require.Contains(t, string(clientEntry.Payload), "[REDACTED]",
		"%s: the inbound body must show where its credential field was replaced", tc.name)
	require.False(t, clientEntry.RedactionUnverified,
		"%s: an inbound body with a known credential must be redaction-verified", tc.name)

	// 入站头部与 URL 事实属于请求阶段，不属于某次尝试，且已知凭据已定点遮蔽。
	clientMetadata := requestTraceMatrixStage(t, stages, "client_metadata", "", 0)
	require.NotNil(t, clientMetadata.Metadata)
	require.Equal(t, http.MethodPost, clientMetadata.Metadata.Method)
	require.Contains(t, clientMetadata.Metadata.URL, "api_key=%5BREDACTED%5D")
	require.Contains(t, clientMetadata.Metadata.URL, "source=matrix")
	require.Equal(t, "[REDACTED]", clientMetadata.Metadata.RequestHeaders.Get("Authorization"))
	require.Equal(t, requestTraceMatrixInboundHeaderCanary, clientMetadata.Metadata.RequestHeaders.Get("X-Inbound-Canary"))

	// wire 阶段：至少一个真实尝试，协议必须是这条路径真实发出的协议。被标为
	// unsupported 或"未观察"的尝试不能算作一次真实尝试，因此这里直接拒绝它们，
	// 避免用空白冒充覆盖。
	require.NotEmpty(t, attempts, "%s: the real path must report the attempts it made", tc.name)
	for _, attempt := range attempts {
		require.NotNil(t, attempt.Metadata, "%s: a wire attempt must carry typed facts", tc.name)
		require.Equal(t, tc.protocol, attempt.Metadata.Protocol,
			"%s: a wire attempt must carry the protocol actually sent", tc.name)
		require.NotEqual(t, service.RequestTraceUnsupported, attempt.State,
			"%s: a phase-1 path must not be labelled as unsupported coverage", tc.name)
		require.NotEqual(t, "attempt_not_observed", attempt.Reason,
			"%s: this path did observe an attempt", tc.name)
	}
	require.Len(t, attempts, 1, "%s: this real path makes exactly one upstream attempt", tc.name)
	attempt := attempts[0]
	require.Equal(t, 1, attempt.AttemptIndex)
	require.EqualValues(t, e.accountID, attempt.Metadata.AccountID)
	require.NotEmpty(t, attempt.Metadata.Model)
	if tc.attemptModel != "" {
		require.Equal(t, tc.attemptModel, attempt.Metadata.Model)
	}
	require.Equal(t, http.StatusOK, attempt.Metadata.Status)
	require.NotNil(t, attempt.Metadata.StartedAt)
	require.NotNil(t, attempt.Metadata.EndedAt)
	require.Equal(t, "wire_observed", attempt.Reason)
	require.Equal(t, requestTraceMatrixUpstreamHeaderCanary, attempt.Metadata.ResponseHeaders.Get("X-Upstream-Canary"))

	// 正向对照：真实出站请求里的凭据出现在尝试事实的请求头里，但只剩遮蔽值——
	// 既证明凭据真的被观察到过，也证明它被定点替换而不是原样留存。
	var redactedCredentialHeaders int
	for _, values := range attempt.Metadata.RequestHeaders {
		for _, value := range values {
			if value == "[REDACTED]" {
				redactedCredentialHeaders++
			}
		}
	}
	require.NotZero(t, redactedCredentialHeaders,
		"%s: the wire attempt must show at least one redacted credential header", tc.name)

	// 真实发出的字节与真实收到的上游响应都按来源分别保留。
	wireRequest := requestTraceMatrixStage(t, stages, "wire_request", "wire", 1)
	require.Contains(t, string(wireRequest.Payload), requestTraceMatrixPromptCanary)

	upstreamResponse := requestTraceMatrixStage(t, stages, "upstream_response", "received", 1)
	require.Contains(t, string(upstreamResponse.Payload), requestTraceMatrixResponseCanary)

	// 最终下游体被观察到。这里保留客户端实际收到的全部非凭据事实：任何被定点
	// 遮蔽的位置都必须是"已知凭据字段"，不能连同用量计数一起擦掉，否则采集结果
	// 就失去了回答"这次请求到底发生了什么"的能力。
	clientResponse := requestTraceMatrixStage(t, stages, "client_response", "downstream", 0)
	require.Contains(t, string(clientResponse.Payload), requestTraceMatrixResponseCanary)
	require.Equal(t, service.RequestTraceStored, clientResponse.State)
	require.NotContains(t, string(clientResponse.Payload), requestTraceMatrixBodyCredentialCanary,
		"%s: the downstream body must not retain the inbound credential", tc.name)
	for _, counter := range tc.counters {
		require.Contains(t, string(clientResponse.Payload), counter,
			"%s: the retained downstream body must keep the usage counter %s verbatim, got %s",
			tc.name, counter, string(clientResponse.Payload))
	}
	require.NotContains(t, string(clientResponse.Payload), `":"[REDACTED]"}`,
		"%s: no trailing usage object may be left fully redacted", tc.name)
	// 除已知凭据字段外，保留的下游体必须逐字节重放客户端收到的内容。
	expectedDownstream, _, ok := service.RedactRequestTraceJSON([]byte(w.Body.String()))
	require.True(t, ok, "%s: the client body must be valid JSON for this comparison", tc.name)
	require.JSONEq(t, string(expectedDownstream), string(clientResponse.Payload),
		"%s: the retained downstream body must describe what the client actually received", tc.name)
}

// TestRequestTraceRealMatrixGatewayChatCompletions 覆盖 Anthropic 分组上的
// /v1/chat/completions：客户端是 OpenAI Chat Completions 形状，出站是
// Anthropic Messages。入站协议与 wire 协议不同，Trace 必须分别如实标注。
func TestRequestTraceRealMatrixGatewayChatCompletions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newRequestTraceMatrixAnthropicEnv(t)
	env.run(t, requestTraceMatrixCase{
		name: "gateway_chat_completions",
		path: "/v1/chat/completions",
		body: `{"model":"` + requestTraceMatrixModel + `","stream":false,` +
			`"api_key":"` + requestTraceMatrixBodyCredentialCanary + `",` +
			`"messages":[{"role":"user","content":"` + requestTraceMatrixPromptCanary + `"}]}`,
		entry:        func(e *requestTraceMatrixEnv) gin.HandlerFunc { return e.anthropic.ChatCompletions },
		family:       service.RequestTraceChatCompletions,
		endpoint:     "/v1/chat/completions",
		upstreamPath: "/v1/messages",
		protocol:     "anthropic.messages",
		attemptModel: requestTraceMatrixModel,
		counters:     []string{`"prompt_tokens":5`, `"completion_tokens":3`, `"total_tokens":8`},
	})
}

// TestRequestTraceRealMatrixGatewayResponses 覆盖 Anthropic 分组上的 /v1/responses：
// 客户端是 OpenAI Responses 形状，出站是 Anthropic Messages。
func TestRequestTraceRealMatrixGatewayResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newRequestTraceMatrixAnthropicEnv(t)
	env.run(t, requestTraceMatrixCase{
		name: "gateway_responses",
		path: "/v1/responses",
		body: `{"model":"` + requestTraceMatrixModel + `","stream":false,` +
			`"api_key":"` + requestTraceMatrixBodyCredentialCanary + `",` +
			`"input":"` + requestTraceMatrixPromptCanary + `"}`,
		entry:        func(e *requestTraceMatrixEnv) gin.HandlerFunc { return e.anthropic.Responses },
		family:       service.RequestTraceResponses,
		endpoint:     "/v1/responses",
		upstreamPath: "/v1/messages",
		protocol:     "anthropic.messages",
		attemptModel: requestTraceMatrixModel,
		counters:     []string{`"input_tokens":5`, `"output_tokens":3`},
	})
}

// TestRequestTraceRealMatrixOpenAIGatewayMessages 覆盖 OpenAI 分组上的 /v1/messages：
// 客户端是 Anthropic Messages 形状，出站是 OpenAI Responses。这条路径的入站协议
// 与出站协议同样不同，且未探测能力的 OpenAI APIKey 账号以 Responses 协议上行。
func TestRequestTraceRealMatrixOpenAIGatewayMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newRequestTraceMatrixOpenAIEnv(t)
	env.run(t, requestTraceMatrixCase{
		name: "openai_gateway_messages",
		path: "/v1/messages",
		body: `{"model":"` + requestTraceMatrixOpenAIModel + `","max_tokens":32,` +
			`"api_key":"` + requestTraceMatrixBodyCredentialCanary + `",` +
			`"messages":[{"role":"user","content":"` + requestTraceMatrixPromptCanary + `"}]}`,
		entry:        func(e *requestTraceMatrixEnv) gin.HandlerFunc { return e.openai.Messages },
		family:       service.RequestTraceMessages,
		endpoint:     "/v1/messages",
		upstreamPath: "/v1/responses",
		protocol:     "openai.responses",
		counters:     []string{`"input_tokens":1`, `"output_tokens":1`},
	})
}

// TestRequestTraceRealMatrixUnobservedAttemptIsNotStored 是矩阵的反例：
// 真实 GatewayHandler.Responses 路径上没有可用账号，上游一次都没被调用。此时
// Trace 必须显式报告 wire_attempt 未观察、不得伪造 wire_request/upstream_response
// 阶段，也不得声称已完整覆盖（不得终结为 stored）。
func TestRequestTraceRealMatrixUnobservedAttemptIsNotStored(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const groupID int64 = 7821
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	h := newRequestTraceMatrixGatewayWithoutAccounts(t, group, cfg)

	capture := &requestTraceMatrixUpstream{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		capture.recordRequest(req, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(requestTraceMatrixAnthropicSSE()))
	}))
	t.Cleanup(upstream.Close)

	repo := &finalizingRequestTraceRepoStub{finalized: make(chan service.RequestTraceCaptureState, 1)}
	queue := service.NewRequestTraceCaptureQueue(repo)
	t.Cleanup(queue.Stop)

	env := &requestTraceMatrixEnv{
		repo: repo, queue: queue, capture: capture,
		anthropic: h, group: group, groupID: groupID, accountID: 9401, apiKeyID: 13, userID: 23,
	}

	body := `{"model":"` + requestTraceMatrixModel + `","stream":false,` +
		`"api_key":"` + requestTraceMatrixBodyCredentialCanary + `",` +
		`"input":"` + requestTraceMatrixPromptCanary + `"}`
	w := env.serve(t, "/v1/responses", body, env.anthropic.Responses)

	require.GreaterOrEqual(t, w.Code, http.StatusBadRequest,
		"a request with no available account must fail: %s", w.Body.String())

	// 真实的 wire 尝试一次都没有发生。
	paths, _ := capture.snapshot()
	require.Empty(t, paths, "no account is configured, so nothing may reach the upstream")

	requestTraceMatrixWaitQueueDrained(t, queue)

	select {
	case state := <-repo.finalized:
		t.Fatalf("a trace without an observed wire attempt must not be finalized as complete, got %q", state)
	default:
	}

	repo.mu.Lock()
	traces := append([]service.RequestTrace(nil), repo.traces...)
	stages := append([]service.RequestTraceStage(nil), repo.stages...)
	repo.mu.Unlock()

	require.Len(t, traces, 1, "an authenticated but unattempted request still gets exactly one trace")
	trace := traces[0]
	require.Equal(t, service.RequestTraceResponses, trace.RouteFamily)
	require.Equal(t, "/v1/responses", trace.InboundEndpoint)
	require.NotEqual(t, service.RequestTraceStored, trace.CaptureState,
		"no observed wire attempt, so the trace must not claim full coverage")
	require.Equal(t, service.RequestTracePartial, trace.CaptureState)

	var described []string
	for _, stage := range stages {
		require.Equal(t, trace.TraceID, stage.TraceID)
		described = append(described, stage.Stage+"="+stage.Reason)
		switch stage.Stage {
		case "wire_request", "upstream_response":
			require.Failf(t, "fabricated attempt stage",
				"stage %q view %q exists although no upstream attempt was observed (%v)",
				stage.Stage, stage.View, described)
		}
		assertRequestTraceMatrixNoCredentialCanary(t, "unobserved_attempt", stage)
	}

	// 缺口必须被显式命名，而不是留白：恰好一个未观察的 wire_attempt。
	unobserved := requestTraceMatrixStage(t, stages, "wire_attempt", "", 0)
	require.Equal(t, service.RequestTraceNotObserved, unobserved.State)
	require.Equal(t, "attempt_not_observed", unobserved.Reason)
	require.Nil(t, unobserved.Metadata, "an unobserved attempt must not carry fabricated attempt facts")

	// 入站正文仍然被观察到：缺口只限于那次从未发出的上游尝试，而不是整条链路。
	clientEntry := requestTraceMatrixStage(t, stages, "client_entry", "decoded", 0)
	require.Contains(t, string(clientEntry.Payload), requestTraceMatrixPromptCanary,
		"the authenticated inbound body is still observed, so the gap is scoped to the attempt")
	require.Equal(t, service.RequestTraceStored, clientEntry.State)
}
