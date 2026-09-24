//go:build unit

package handler

// 请求内 429 账号上限（默认 2）在 OpenAI 兼容 Responses WebSocket 入口上的验收。
//
// 与 openai_429_account_limit_matrix_test.go 覆盖的 HTTP 入口相同，本文件把同一
// 口径搬到 websocket ingress：断言落在对外可观察行为上——上游 HTTP 桥实际被调用的
// 账号序列、客户端读到的连接终止状态，而不直接触碰内部计数结构。
//
// 账号刻意不使用 openai_apikey_responses_websockets_v2_* Extra 开关，配合
// cfg.Gateway.OpenAIWS.IngressModeDefault=http_bridge，使连接走 HTTP 桥而非真实
// 上游 WS，从而让 service.HTTPUpstream 假实现成为可观测的上游。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const openAI429WSBridgeSuccessSSE = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_ws_429_ok","model":"gpt-5.1"}}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_ws_429_ok","model":"gpt-5.1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"

// openAI429WSBridgeUpstream 记录 WS HTTP 桥每次命中的账号，并为 successAccountID
// （0 表示不存在）返回可被桥解析的 Responses SSE 成功流，其余一律 429。
type openAI429WSBridgeUpstream struct {
	service.HTTPUpstream

	mu               sync.Mutex
	accountIDs       []int64
	successAccountID int64
}

func (u *openAI429WSBridgeUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	success := u.successAccountID != 0 && u.successAccountID == accountID
	u.mu.Unlock()

	if success {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(openAI429WSBridgeSuccessSSE)),
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`)),
	}, nil
}

func (u *openAI429WSBridgeUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

// newOpenAI429WSBridgeEnv 装配一条“API Key 账号 -> HTTP 桥 -> 假上游”的
// Responses WebSocket 链路，账号 ID 依次为 1..accountCount。
func newOpenAI429WSBridgeEnv(t *testing.T, upstream *openAI429WSBridgeUpstream, accountCount int) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)

	groupID := int64(4299)
	accounts := make([]service.Account, 0, accountCount)
	for id := int64(1); id <= int64(accountCount); id++ {
		accounts = append(accounts, service.Account{
			ID:          id,
			Name:        "openai-429-ws-" + strconv.FormatInt(id, 10),
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeAPIKey,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    int(id),
			Credentials: map[string]any{
				"api_key":  "sk-429-ws",
				"base_url": "https://upstream.example.test",
			},
		})
	}

	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	// 关闭上游 WSv2 传输：账号没有开启 responses websockets v2，HTTP 桥才可观测。
	cfg.Gateway.OpenAIWS.IngressModeDefault = service.OpenAIWSIngressModeHTTPBridge
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	// 换号次数远高于 429 账号额度，确保终止来自请求内 429 上限而不是换号次数。
	cfg.Gateway.MaxAccountSwitches = 10

	accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
	rateLimitSvc := service.NewRateLimitService(accountRepo, nil, cfg, nil, nil)
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)

	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), rateLimitSvc, billingCacheSvc,
		upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
	)

	cache := &concurrencyCacheMock{
		acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) {
			return true, nil
		},
	}
	h := NewOpenAIGatewayHandler(
		gatewaySvc,
		service.NewConcurrencyService(cache),
		billingCacheSvc,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil, cfg,
	)

	apiKey := &service.APIKey{
		ID:      4290,
		GroupID: &groupID,
		User:    &service.User{ID: 4290, Status: service.StatusActive},
		Group:   &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.GET("/openai/v1/responses", h.ResponsesWebSocket)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

// openAI429WSDialAndCreate 建立客户端连接并发送首个 response.create 帧。
func openAI429WSDialAndCreate(t *testing.T, server *httptest.Server) *coderws.Conn {
	t.Helper()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	conn, _, err := coderws.Dial(
		dialCtx,
		"ws"+strings.TrimPrefix(server.URL, "http")+"/openai/v1/responses",
		&coderws.DialOptions{CompressionMode: coderws.CompressionContextTakeover},
	)
	cancelDial()
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.CloseNow() })

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 5*time.Second)
	err = conn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","stream":false}`))
	cancelWrite()
	require.NoError(t, err)
	return conn
}

// 三个连续可调度的 OpenAI API Key 账号全部 429：请求内 429 账号上限（默认 2）必须
// 在第二个账号之后终止整条连接，账号 3 永远不该被上游看到。
func TestOpenAI429AccountLimit_ResponsesWebSocketStopsBeforeThirdAccount(t *testing.T) {
	upstream := &openAI429WSBridgeUpstream{}
	server := newOpenAI429WSBridgeEnv(t, upstream, 3)
	conn := openAI429WSDialAndCreate(t, server)

	readCtx, cancelRead := context.WithTimeout(context.Background(), 15*time.Second)
	var closeErr coderws.CloseError
	for {
		_, _, readErr := conn.Read(readCtx)
		if readErr == nil {
			continue
		}
		require.ErrorAs(t, readErr, &closeErr, "连接应以服务端关闭帧结束，实际错误：%v", readErr)
		break
	}
	cancelRead()

	// 首个 429 后仅换到账号 2；账号 2 的 429 触顶，账号 3 不再被尝试。
	require.Equal(t, []int64{1, 2}, upstream.calls(), "请求内 429 账号上限必须停在第二个账号")
	require.Equal(t, coderws.StatusTryAgainLater, closeErr.Code)
	require.Equal(t, "upstream rate limit exceeded, please retry later", closeErr.Reason)
}

// 第一个账号 429、第二个账号成功：上游只被账号 1、2 命中，客户端拿到终止事件
// response.completed，本次逻辑请求就此完成，不再继续换号。
func TestOpenAI429AccountLimit_ResponsesWebSocketSecondAccountSuccessCompletes(t *testing.T) {
	upstream := &openAI429WSBridgeUpstream{successAccountID: 2}
	server := newOpenAI429WSBridgeEnv(t, upstream, 3)
	conn := openAI429WSDialAndCreate(t, server)

	var eventTypes []string
	completed := false
	readCtx, cancelRead := context.WithTimeout(context.Background(), 15*time.Second)
	for !completed {
		_, event, readErr := conn.Read(readCtx)
		require.NoError(t, readErr)
		eventType := gjson.GetBytes(event, "type").String()
		eventTypes = append(eventTypes, eventType)
		if eventType == "response.completed" {
			require.Equal(t, "resp_ws_429_ok", gjson.GetBytes(event, "response.id").String())
			completed = true
		}
	}
	cancelRead()

	require.Equal(t, []string{"response.created", "response.completed"}, eventTypes, "客户端应看到完整生命周期且最终读到终止事件")
	require.Equal(t, []int64{1, 2}, upstream.calls(), "账号 2 成功即结束本次逻辑请求")
	require.NoError(t, conn.Close(coderws.StatusNormalClosure, "done"))
}
