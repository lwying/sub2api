//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 使用真实账号测试服务生成探针，经 HTTP 送入上游网关，防止手写裸 hi 掩盖固定指令。
func TestGatewayMock_AccountTestResponsesProbeReturnsLocalReply(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newRequestTraceRealOpenAIEnv(t, true)
	repo := &gatewayMockSettingsRepo{values: map[string]string{}}
	startGatewayMockRules(t, repo, "hi", "本地账号测试回复")
	env.handler.SetSettingService(gatewayMockSettingService(t, repo))
	var matched atomic.Bool
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read probe failed", http.StatusBadRequest)
			return
		}
		response := env.serve(t, r.URL.Path, string(body), env.handler.Responses)
		matched.Store(response.Header().Get(service.GatewayMockReplyHeader) == service.GatewayMockReplyHeaderValue)
		for key, values := range response.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	}))
	t.Cleanup(gateway.Close)

	probe := runGatewayMockAccountTest(t, service.PlatformOpenAI, gateway.URL)
	require.True(t, matched.Load(), "默认 Responses 账号探针应命中已启用的 hi 规则")
	require.Contains(t, probe, "本地账号测试回复")
	require.Contains(t, probe, `"type":"test_complete","success":true`)
	require.Empty(t, realUpstreamPaths(env.capture), "下游账号测试命中本地回复时不得请求 Provider")
}

func TestGatewayMock_AccountTestMessagesProbeReturnsLocalReply(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newGatewayMockEnv(t)
	startGatewayMockRules(t, env.settingRepo, "hi", "本地账号测试回复")
	var matched atomic.Bool
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read probe failed", http.StatusBadRequest)
			return
		}
		response := env.serve(t, r.URL.Path, string(body))
		matched.Store(response.Header().Get(service.GatewayMockReplyHeader) == service.GatewayMockReplyHeaderValue)
		for key, values := range response.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	}))
	t.Cleanup(gateway.Close)

	probe := runGatewayMockAccountTest(t, service.PlatformAnthropic, gateway.URL)
	require.True(t, matched.Load(), "默认 Claude 账号探针应命中已启用的 hi 规则")
	require.Contains(t, probe, "本地账号测试回复")
	require.Contains(t, probe, `"type":"test_complete","success":true`)
	require.Empty(t, upstreamPaths(env.capture), "下游账号测试命中本地回复时不得请求 Provider")
}

func TestGatewayMock_ResponsesStreamProvidesOrderedOutputLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newRequestTraceRealOpenAIEnv(t, true)
	repo := &gatewayMockSettingsRepo{values: map[string]string{}}
	startGatewayMockRules(t, repo, "hi", "本地账号测试回复")
	env.handler.SetSettingService(gatewayMockSettingService(t, repo))
	response := env.serve(t, "/v1/responses", `{"model":"gpt-5.4","input":"hi","stream":true}`, env.handler.Responses)
	require.Equal(t, service.GatewayMockReplyHeaderValue, response.Header().Get(service.GatewayMockReplyHeader))
	var events []map[string]any
	for _, line := range strings.Split(response.Body.String(), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var event map[string]any
			require.NoError(t, json.Unmarshal([]byte(data), &event))
			events = append(events, event)
		}
	}
	var kinds []any
	for i, event := range events {
		kinds = append(kinds, event["type"])
		require.Equal(t, float64(i), event["sequence_number"], "严格 Responses 客户端要求每帧都有递增序号")
	}
	require.Equal(t, []any{
		"response.created", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.done", "response.content_part.done",
		"response.output_item.done", "response.completed",
	}, kinds)
	item, ok := events[1]["item"].(map[string]any)
	require.True(t, ok)
	itemID := item["id"]
	for _, i := range []int{2, 3, 4, 5} {
		require.Equal(t, itemID, events[i]["item_id"])
	}
	require.Equal(t, "本地账号测试回复", events[3]["delta"])
	require.Equal(t, []any{}, events[3]["logprobs"])
	done, ok := events[6]["item"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, itemID, done["id"])
	content, ok := done["content"].([]any)
	require.True(t, ok)
	require.Equal(t, "本地账号测试回复", content[0].(map[string]any)["text"])
	require.Empty(t, realUpstreamPaths(env.capture))
}

type gatewayMockProbeAccountRepo struct {
	service.AccountRepository
	account *service.Account
}

func (r *gatewayMockProbeAccountRepo) GetByID(context.Context, int64) (*service.Account, error) {
	return r.account, nil
}

func runGatewayMockAccountTest(t *testing.T, platform, baseURL string) string {
	t.Helper()
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	account := &service.Account{
		ID: 1, Name: "local-downstream", Platform: platform, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "local-test-key", "base_url": baseURL},
		Concurrency: 1,
	}
	probe := service.NewAccountTestService(&gatewayMockProbeAccountRepo{account: account}, nil, nil, nil, nil, repository.NewHTTPUpstream(cfg), cfg, nil)
	router := gin.New()
	router.POST("/api/v1/admin/accounts/1/test", func(c *gin.Context) {
		_ = probe.TestAccountConnection(c, 1, "", "", "")
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", strings.NewReader(`{"model_id":"","prompt":""}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	return recorder.Body.String()
}
