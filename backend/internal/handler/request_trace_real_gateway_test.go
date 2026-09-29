//go:build unit

package handler

// 真实网关链路（非模拟 transport）的请求审计采集接缝。
//
// 既有 request_trace_*_test.go 全部把 trace 接线放在测试自己的 countingTraceTransport
// 里，只证明「采集器本身」可用，没有证明生产转发路径真的把观察者接到了出站请求上。
// 本用例走真实装配：真实 GatewayHandler.Messages -> 真实 service.GatewayService
// -> repository.NewHTTPUpstream 的真实 RoundTrip（其 grokAccessDeniedFallbackTransport
// 是生产代码里唯一调用 httpattempt.*Trace* 的接缝）-> 本地 httptest 上游。
//
// 只使用本地 httptest 作为上游，不访问任何生产上游。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

const requestTraceRealCanary = "TRACE_REAL_GATEWAY_CANARY"

// requestTraceRealUpstream 记录真实 transport 实际送出的字节，供与 trace 留存比对。
type requestTraceRealUpstream struct {
	mu     sync.Mutex
	paths  []string
	bodies [][]byte
}

func (u *requestTraceRealUpstream) record(req *http.Request, body []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.paths = append(u.paths, req.URL.Path)
	u.bodies = append(u.bodies, append([]byte(nil), body...))
}

func (u *requestTraceRealUpstream) snapshot() ([]string, [][]byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	paths := append([]string(nil), u.paths...)
	bodies := make([][]byte, 0, len(u.bodies))
	for _, body := range u.bodies {
		bodies = append(bodies, append([]byte(nil), body...))
	}
	return paths, bodies
}

// newRequestTraceRealGatewayMessages 装配真实上游 client（repository.NewHTTPUpstream）
// 的最小 Anthropic Messages 网关，账号 base_url 指向本地 httptest 上游。
func newRequestTraceRealGatewayMessages(t *testing.T, group *service.Group, account *service.Account, cfg *config.Config) *GatewayHandler {
	t.Helper()
	accountRepo := openAIImagesFailoverAccountRepo{accounts: []service.Account{*account}}
	rateLimit := service.NewRateLimitService(gateway429NoCooldownRepo{accountRepo}, nil, &config.Config{}, nil, nil)
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account}}, nil, nil, nil, nil)
	gw := service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, nil,
		cfg, snapshot, nil, nil, rateLimit, nil, nil,
		// 真实 HTTPUpstream：真实 RoundTrip，真实 trace 接缝。
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

// TestRequestTraceRealGatewayMessagesPersistsRealWireAttemptFacts 是本文件的核心接缝：
// 真实 Messages 入口 -> 真实上游 transport -> queue stub，要求采集到请求/wire/响应
// 三个阶段以及本次上游尝试的账号、协议、状态码。
func TestRequestTraceRealGatewayMessagesPersistsRealWireAttemptFacts(t *testing.T) {
	gin.SetMode(gin.TestMode)

	capture := &requestTraceRealUpstream{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		capture.record(req, body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Canary", "real-upstream-visible")
		_, _ = w.Write([]byte(`{"id":"msg_real","type":"message","role":"assistant","model":"claude-sonnet-4-5",` +
			`"content":[{"type":"text","text":"TRACE_REAL_GATEWAY_RESPONSE_CANARY"}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true

	const groupID int64 = 7781
	const accountID int64 = 9101
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	account := &service.Account{
		ID: accountID, Name: "real-upstream", Platform: service.PlatformAnthropic,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Priority: 1, Concurrency: 1,
		Credentials:   map[string]any{"api_key": "sk-real-upstream", "base_url": upstream.URL},
		AccountGroups: []service.AccountGroup{{AccountID: accountID, GroupID: groupID}},
	}
	h := newRequestTraceRealGatewayMessages(t, group, account, cfg)

	repo := &finalizingRequestTraceRepoStub{finalized: make(chan service.RequestTraceCaptureState, 1)}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()

	r := gin.New()
	r.POST("/v1/messages",
		RequestTraceCaptureMiddleware(func(context.Context) bool { return true }, repo, queue),
		func(c *gin.Context) {
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, group))
			keyGroupID := groupID
			apiKey := &service.APIKey{
				ID: 8, UserID: 10, GroupID: &keyGroupID, Status: service.StatusActive,
				User: &service.User{ID: 10, Concurrency: 10, Balance: 100}, Group: group,
			}
			c.Set(string(middleware.ContextKeyAPIKey), apiKey)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 10, Concurrency: 10})
			c.Next()
		},
		BindRequestTraceAfterAuth(),
		h.Messages,
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-5","max_tokens":32,"messages":[{"role":"user","content":"`+requestTraceRealCanary+`"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Inbound-Canary", "real-inbound-visible")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "real gateway must answer the client: %s", w.Body.String())

	// 真实上游只被调用一次，且真的收到了客户端提示词：wire 阶段不应是伪造的。
	paths, bodies := capture.snapshot()
	require.Equal(t, []string{"/v1/messages"}, paths)
	require.Len(t, bodies, 1)
	require.Contains(t, string(bodies[0]), requestTraceRealCanary)

	select {
	case state := <-repo.finalized:
		require.Equal(t, service.RequestTraceStored, state,
			"a fully observed real attempt must finalize as stored")
	case <-time.After(2 * time.Second):
		t.Fatalf("real gateway trace was never finalized (queue stored=%d rejected=%d)",
			queue.Stats().Stored, queue.Stats().Rejected)
	}

	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Len(t, repo.traces, 1, "one logical request must produce exactly one trace")
	trace := repo.traces[0]
	require.Equal(t, service.RequestTraceMessages, trace.RouteFamily)
	require.Equal(t, "/v1/messages", trace.InboundEndpoint)
	require.Equal(t, http.StatusOK, trace.ClientStatus)
	require.Nil(t, trace.UsageLogID)

	stages := map[string]service.RequestTraceStage{}
	var attempts []service.RequestTraceStage
	for _, stage := range repo.stages {
		require.Equal(t, trace.TraceID, stage.TraceID)
		stages[stage.Stage] = stage
		if stage.Stage == "wire_attempt" {
			attempts = append(attempts, stage)
		}
	}

	// 请求阶段：入站正文被观察到，且保留的是客户端真的发来的字节。
	clientEntry, ok := stages["client_entry"]
	require.True(t, ok, "client request stage must be captured on the real gateway path")
	require.Contains(t, string(clientEntry.Payload), requestTraceRealCanary)

	// wire 阶段：真实发出的字节 + 本次尝试的账号/协议/状态。
	require.Len(t, attempts, 1, "the real path must report exactly the attempts it made")
	attempt := attempts[0]
	require.NotNil(t, attempt.Metadata)
	require.Equal(t, 1, attempt.AttemptIndex)
	require.EqualValues(t, accountID, attempt.Metadata.AccountID)
	require.Equal(t, "anthropic.messages", attempt.Metadata.Protocol)
	require.Equal(t, "claude-sonnet-4-5", attempt.Metadata.Model)
	require.Equal(t, http.StatusOK, attempt.Metadata.Status)
	require.NotNil(t, attempt.Metadata.StartedAt)
	require.NotNil(t, attempt.Metadata.EndedAt)

	wireRequest, ok := stages["wire_request"]
	require.True(t, ok)
	require.Contains(t, string(wireRequest.Payload), requestTraceRealCanary)
	require.Equal(t, "real-upstream-visible", attempt.Metadata.ResponseHeaders.Get("X-Upstream-Canary"))

	// 入站头部事实属于请求阶段，不属于某次尝试。
	clientMetadata, ok := stages["client_metadata"]
	require.True(t, ok)
	require.NotNil(t, clientMetadata.Metadata)
	require.Equal(t, http.MethodPost, clientMetadata.Metadata.Method)
	require.Equal(t, "real-inbound-visible", clientMetadata.Metadata.RequestHeaders.Get("X-Inbound-Canary"))

	// 响应阶段：真实上游响应被归到本次尝试，并作为客户端最终响应留存。
	upstreamResponse, ok := stages["upstream_response"]
	require.True(t, ok)
	require.Contains(t, string(upstreamResponse.Payload), "TRACE_REAL_GATEWAY_RESPONSE_CANARY")

	clientResponse, ok := stages["client_response"]
	require.True(t, ok)
	require.Contains(t, string(clientResponse.Payload), "TRACE_REAL_GATEWAY_RESPONSE_CANARY")
}

// ---------------------------------------------------------------------------
// OpenAI 入口（Responses / Chat Completions）
//
// OpenAI 兼容入口由 OpenAIGatewayHandler + service.OpenAIGatewayService 承载，
// 与上面的 Anthropic Messages 入口不是同一条装配路径，因此单独走一遍真实
// repository.NewHTTPUpstream 的 RoundTrip。
// ---------------------------------------------------------------------------

const requestTraceRealOpenAIModel = "gpt-5.1"
const requestTraceRealOpenAICanary = "TRACE_REAL_OPENAI_CANARY"

// requestTraceRealOpenAITerminal 是最小的 Responses 协议终止事件，包住同一份结果。
// 未探测能力的 OpenAI APIKey 账号（Extra 为空）两个入口都以 Responses 协议上行，
// Chat Completions 入口依赖事件流的终止事件才能缓冲出 JSON。
const requestTraceRealOpenAITerminal = `{"type":"response.completed","response":` + requestTraceRealOpenAIJSON + `}`

// requestTraceRealOpenAIJSON 是同一份 Responses 结果的非流式形态，供 Responses 入口使用。
const requestTraceRealOpenAIJSON = `{"id":"resp_real","object":"response","created_at":1700000000,"model":"gpt-5.1","status":"completed","output":[{"type":"message","id":"msg_real","status":"completed","role":"assistant","content":[{"type":"output_text","text":"TRACE_REAL_OPENAI_RESPONSE_CANARY","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`

func requestTraceRealOpenAISSE() string {
	return "data: " + requestTraceRealOpenAITerminal + "\n\n"
}

// newRequestTraceRealGatewayOpenAI 装配真实上游 client（repository.NewHTTPUpstream）
// 的最小 OpenAI 网关。装配形状沿用 openAI429MatrixEnv，只把 HTTPUpstream 换成真实
// 实现，并把账号 base_url 指向本地 httptest 上游。
func newRequestTraceRealGatewayOpenAI(t *testing.T, accounts []service.Account, cfg *config.Config) *OpenAIGatewayHandler {
	t.Helper()
	gatewayService := service.NewOpenAIGatewayService(
		openAIImagesFailoverAccountRepo{accounts: accounts},
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil,
		// 真实 HTTPUpstream：真实 RoundTrip，真实 trace 接缝。
		repository.NewHTTPUpstream(cfg),
		nil, nil, nil, nil, nil, nil, nil, nil,
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

type requestTraceRealOpenAIEnv struct {
	handler   *OpenAIGatewayHandler
	capture   *requestTraceRealUpstream
	repo      *finalizingRequestTraceRepoStub
	queue     *service.RequestTraceCaptureQueue
	group     *service.Group
	accountID int64
}

// newRequestTraceRealOpenAIEnv 的 upstreamSSE 决定本地上游用什么形态作答：
//   - false：Responses 协议的非流式 JSON 响应（对应客户端 stream=false）。
//   - true：Responses 协议事件流（Chat Completions 入口把它缓冲成 JSON 后才回给客户端）。
//
// 两个入口在未探测能力的 OpenAI APIKey 账号上都以 Responses 协议上行，因此用形态
// 而不是路径区分；这与既有 openAI429MatrixSuccessBody 的处理一致。
func newRequestTraceRealOpenAIEnv(t *testing.T, upstreamSSE bool) *requestTraceRealOpenAIEnv {
	t.Helper()
	capture := &requestTraceRealUpstream{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		capture.record(req, raw)
		w.Header().Set("X-Upstream-Canary", "real-upstream-visible")
		if upstreamSSE {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(requestTraceRealOpenAISSE()))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(requestTraceRealOpenAIJSON))
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true

	const groupID int64 = 7791
	const accountID int64 = 9201
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformOpenAI, Status: service.StatusActive}
	accounts := []service.Account{{
		ID: accountID, Name: "real-openai-upstream", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Priority: 1, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-real-openai", "base_url": upstream.URL},
	}}
	repo := &finalizingRequestTraceRepoStub{finalized: make(chan service.RequestTraceCaptureState, 1)}
	queue := service.NewRequestTraceCaptureQueue(repo)
	t.Cleanup(queue.Stop)
	return &requestTraceRealOpenAIEnv{
		handler: newRequestTraceRealGatewayOpenAI(t, accounts, cfg),
		capture: capture, repo: repo, queue: queue, group: group, accountID: accountID,
	}
}

func (e *requestTraceRealOpenAIEnv) serve(t *testing.T, path, body string, entry gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	group := e.group
	r := gin.New()
	r.POST(path,
		RequestTraceCaptureMiddleware(func(context.Context) bool { return true }, e.repo, e.queue),
		func(c *gin.Context) {
			keyGroupID := group.ID
			apiKey := &service.APIKey{
				ID: 9, UserID: 11, GroupID: &keyGroupID, Status: service.StatusActive,
				User: &service.User{ID: 11, Concurrency: 10, Balance: 100}, Group: group,
			}
			c.Set(string(middleware.ContextKeyAPIKey), apiKey)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 11, Concurrency: 10})
			c.Next()
		},
		BindRequestTraceAfterAuth(),
		entry,
	)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// assertRequestTraceRealOpenAIAttempt 断言真实 OpenAI 链路把请求/wire/响应三阶段，
// 以及本次尝试的账号、协议、状态码落进 queue stub。upstreamPath 是真实发出的路径，
// 对未探测能力的 OpenAI APIKey 账号它与客户端入口可能不同。
func assertRequestTraceRealOpenAIAttempt(
	t *testing.T,
	env *requestTraceRealOpenAIEnv,
	clientEndpoint, upstreamPath string,
	family service.RequestTraceRouteFamily,
	protocol string,
) {
	t.Helper()
	paths, bodies := env.capture.snapshot()
	require.Equal(t, []string{upstreamPath}, paths, "the real OpenAI path must call the local httptest upstream")
	require.Len(t, bodies, 1)
	require.Contains(t, string(bodies[0]), requestTraceRealOpenAICanary)

	select {
	case state := <-env.repo.finalized:
		require.Equal(t, service.RequestTraceStored, state,
			"a fully observed real OpenAI attempt must finalize as stored")
	case <-time.After(2 * time.Second):
		t.Fatalf("real OpenAI trace was never finalized (queue stored=%d rejected=%d)",
			env.queue.Stats().Stored, env.queue.Stats().Rejected)
	}

	env.repo.mu.Lock()
	defer env.repo.mu.Unlock()
	require.Len(t, env.repo.traces, 1)
	trace := env.repo.traces[0]
	require.Equal(t, family, trace.RouteFamily)
	require.Equal(t, clientEndpoint, trace.InboundEndpoint)
	require.Equal(t, http.StatusOK, trace.ClientStatus)
	require.Nil(t, trace.UsageLogID)

	stages := map[string]service.RequestTraceStage{}
	var attempts []service.RequestTraceStage
	for _, stage := range env.repo.stages {
		require.Equal(t, trace.TraceID, stage.TraceID)
		stages[stage.Stage] = stage
		if stage.Stage == "wire_attempt" {
			attempts = append(attempts, stage)
		}
	}

	clientEntry, ok := stages["client_entry"]
	require.True(t, ok, "client request stage must be captured on the real OpenAI path")
	require.Contains(t, string(clientEntry.Payload), requestTraceRealOpenAICanary)

	require.Len(t, attempts, 1, "the real OpenAI path must report exactly the attempts it made")
	attempt := attempts[0]
	require.NotNil(t, attempt.Metadata)
	require.EqualValues(t, env.accountID, attempt.Metadata.AccountID)
	require.Equal(t, protocol, attempt.Metadata.Protocol)
	require.Equal(t, http.StatusOK, attempt.Metadata.Status)

	wireRequest, ok := stages["wire_request"]
	require.True(t, ok)
	require.Contains(t, string(wireRequest.Payload), requestTraceRealOpenAICanary)
	require.Equal(t, "real-upstream-visible", attempt.Metadata.ResponseHeaders.Get("X-Upstream-Canary"))

	upstreamResponse, ok := stages["upstream_response"]
	require.True(t, ok)
	require.Contains(t, string(upstreamResponse.Payload), "TRACE_REAL_OPENAI_RESPONSE_CANARY")

	clientResponse, ok := stages["client_response"]
	require.True(t, ok)
	require.Contains(t, string(clientResponse.Payload), "TRACE_REAL_OPENAI_RESPONSE_CANARY")
}

func TestRequestTraceRealGatewayConvertedResponsesSSEHasJSONDownstreamTrace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newRequestTraceRealOpenAIEnv(t, true)
	body := `{"model":"` + requestTraceRealOpenAIModel + `","stream":false,"input":"` + requestTraceRealOpenAICanary + `"}`
	w := env.serve(t, "/v1/responses", body, env.handler.Responses)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
	assertRequestTraceRealOpenAIAttempt(t, env,
		"/v1/responses", "/v1/responses", service.RequestTraceResponses, "openai.responses")
}

func TestRequestTraceRealGatewayOpenAIResponsesPersistsRealWireAttemptFacts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newRequestTraceRealOpenAIEnv(t, false)
	body := `{"model":"` + requestTraceRealOpenAIModel + `","stream":false,"input":"` + requestTraceRealOpenAICanary + `"}`
	w := env.serve(t, "/v1/responses", body, env.handler.Responses)
	require.Equal(t, http.StatusOK, w.Code, "real OpenAI gateway must answer the client: %s", w.Body.String())
	assertRequestTraceRealOpenAIAttempt(t, env,
		"/v1/responses", "/v1/responses", service.RequestTraceResponses, "openai.responses")
}

func TestRequestTraceRealGatewayOpenAIChatCompletionsPersistsRealWireAttemptFacts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newRequestTraceRealOpenAIEnv(t, true)
	body := `{"model":"` + requestTraceRealOpenAIModel + `","stream":false,"messages":[{"role":"user","content":"` + requestTraceRealOpenAICanary + `"}]}`
	w := env.serve(t, "/v1/chat/completions", body, env.handler.ChatCompletions)
	require.Equal(t, http.StatusOK, w.Code, "real OpenAI gateway must answer the client: %s", w.Body.String())
	// 未探测能力的 OpenAI APIKey 账号把 Chat Completions 转成 Responses 上行。
	assertRequestTraceRealOpenAIAttempt(t, env,
		"/v1/chat/completions", "/v1/responses", service.RequestTraceChatCompletions, "openai.responses")
}
