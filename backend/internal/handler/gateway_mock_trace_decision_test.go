//go:build unit

package handler

// 本地 mock 与 Trace 的接缝：命中管理员配置的下游测试关键词时，网关必须在
// **已开启且命中采集范围**的 Trace 里留下一条显式的网关决策（kind=mock、
// outcome=not_sent、source=inbound），而不是伪造一次上游尝试。
//
// 关键断言有两条，缺一不可：
//   1. 决策阶段存在且只带决策事实（body-less），真实链路上游零请求；
//   2. 这条逻辑请求"没有上游尝试"是**决定的结果**，不是缺口——因此不得再写出
//      通用的 attempt_not_observed 阶段把成功的本地 mock 谎报成 partial。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// serveMockWithTrace 复刻生产中间件链（Trace 采集 -> 鉴权注入 -> Trace 绑定 ->
// 真实入口），只把入口换成本地 mock 环境的真实 GatewayHandler.Messages。
func serveMockWithTrace(t *testing.T, env *gatewayMockEnv, repo *finalizingRequestTraceRepoStub, queue *service.RequestTraceCaptureQueue, body string) *httptest.ResponseRecorder {
	t.Helper()
	group := env.group
	groupID := env.groupID
	apiKeyID := env.apiKeyID
	userID := env.userID

	r := gin.New()
	r.POST("/v1/messages",
		RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate {
			return RequestTraceGateForCapture(true)
		}, repo, queue),
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
		env.handler.Messages,
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestRequestTraceGatewayMockDecisionIsLocalMockWithoutWireAttempt 是真实链路的
// 纵向用例：Trace 开启且范围内、下游测试请求命中规则时，Trace 里恰好有一条
// mock 决策，没有任何伪造成上游尝试的阶段，并且这条 Trace 仍被终结为 stored。
func TestRequestTraceGatewayMockDecisionIsLocalMockWithoutWireAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newGatewayMockEnv(t)
	startGatewayMockRules(t, env.settingRepo, "hi", "本地回复")

	repo := &finalizingRequestTraceRepoStub{finalized: make(chan service.RequestTraceCaptureState, 1)}
	queue := service.NewRequestTraceCaptureQueue(repo)
	t.Cleanup(queue.Stop)

	w := serveMockWithTrace(t, env, repo, queue, gatewayMockMessagesBody("  HI  ", false))

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, service.GatewayMockReplyHeaderValue, w.Header().Get(service.GatewayMockReplyHeader),
		"这条用例必须真的命中本地 mock，否则断言的是另一条路径")
	require.Empty(t, upstreamPaths(env.capture), "命中本地 mock 时不得向上游发出任何请求")

	requestTraceMatrixWaitQueueDrained(t, queue)

	repo.mu.Lock()
	traces := append([]service.RequestTrace(nil), repo.traces...)
	stages := append([]service.RequestTraceStage(nil), repo.stages...)
	repo.mu.Unlock()

	require.Len(t, traces, 1, "一次逻辑请求只产生一条 Trace")
	trace := traces[0]

	var mockDecisions []service.RequestTraceStage
	for _, stage := range stages {
		if stage.Stage == service.RequestTraceDecisionStage && stage.Decision != nil &&
			stage.Decision.Decision == service.RequestTraceDecisionMock {
			mockDecisions = append(mockDecisions, stage)
		}
		// 没有上游尝试，就不可能有尝试阶段——观察到的和"未观察到"的都不行：
		// 后者会把一次正常的本地 mock 谎报成采集缺口。
		switch stage.Stage {
		case "wire_attempt", "wire_request", "upstream_response":
			require.Failf(t, "fabricated attempt stage",
				"本地 mock 不得产生上游尝试阶段，却看到 %s/%s=%s(%s)",
				stage.Stage, stage.View, stage.State, stage.Reason)
		}
	}

	require.Len(t, mockDecisions, 1, "命中的本地 mock 必须恰好留下一条网关决策")
	decision := mockDecisions[0]
	require.Equal(t, string(service.RequestTraceDecisionMock), string(decision.Decision.Decision))
	require.Equal(t, service.RequestTraceDecisionNotSent, decision.Decision.Outcome)
	require.Equal(t, service.RequestTraceDecisionSourceInbound, decision.Decision.Source)
	// 决策阶段是 body-less 的元数据：既不是 wire 尝试，也不带正文或传输事实。
	require.Equal(t, service.RequestTraceNotObserved, decision.State)
	require.Empty(t, decision.Payload)
	require.Nil(t, decision.Metadata)
	require.True(t, service.ValidRequestTraceDecisionStage(decision))

	// "没有上游尝试"是网关的决定，不是缺口：这条 Trace 必须能终结为完整，
	// 而不是停在 partial。
	require.Equal(t, service.RequestTracePartial, trace.CaptureState,
		"入队时仍是待终结的 partial 信封")
	select {
	case state := <-repo.finalized:
		require.Equal(t, service.RequestTraceStored, state,
			"一次成功的本地 mock 不得因为缺少上游尝试而被判为 partial")
	case <-time.After(time.Second):
		t.Fatal("本地 mock 的 Trace 没有被终结为完整")
	}
}

// TestRequestTraceLocalMockDecisionSuppressesOnlyTheAttemptGap 是上面的流程级
// 对照：显式记录一条 mock 决策后不再输出通用的 attempt_not_observed；而没有该
// 决策时（真实的"一次都没发出"错误链路）这个缺口必须照旧显式命名。
func TestRequestTraceLocalMockDecisionSuppressesOnlyTheAttemptGap(t *testing.T) {
	gin.SetMode(gin.TestMode)

	attemptGapReasons := func(stages []service.RequestTraceStage) []string {
		var reasons []string
		for _, stage := range stages {
			if stage.Stage == "wire_attempt" {
				reasons = append(reasons, stage.Reason)
			}
		}
		return reasons
	}

	t.Run("local mock records a decision instead of a gap", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		flow := newRequestTraceFlow()
		c.Set("request_trace_flow", flow)
		recordLocalMockTraceDecision(c)

		stages := flow.finish(strings.Repeat("a", 32), true)
		require.Empty(t, attemptGapReasons(stages),
			"本地 mock 没有上游尝试，但这不是缺口，不得写成 attempt_not_observed")
	})

	t.Run("no wire attempt without a mock decision keeps the gap", func(t *testing.T) {
		flow := newRequestTraceFlow()
		stages := flow.finish(strings.Repeat("a", 32), true)
		require.Equal(t, []string{"attempt_not_observed"}, attemptGapReasons(stages),
			"真实的未发出上游必须继续显式报告缺口")
	})
}
