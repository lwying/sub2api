//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const gatewayMockAuditReply = "本地审计组合测试回复"

// 严格命中优先于内容审计：提示词引擎配置为拒绝时，命中 mock 的请求仍必须本地
// 200、两类审计调用为 0、上游 0 请求、命中事件恰好一条。
func TestGatewayMockMatchedRequestSkipsContentAudit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := &handlerPromptEngine{
		mode:     securityaudit.ModeBlocking,
		decision: &securityaudit.PromptDecision{Kind: securityaudit.DecisionBlock},
	}
	coordinator := securityaudit.NewCoordinator(nil, engine)
	events := &gatewayMockAuditEventStore{}
	w, upstream := serveGatewayMockAuditRequest(t, "messages", false, coordinator, nil, events)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Empty(t, upstream, "命中 mock 不得请求上游")
	require.Equal(t, int64(1), events.calls.Load(), "命中 mock 必须恰好留下一条事件")
	event := events.snapshot()
	require.Equal(t, service.GatewayMockContentAuditSkippedLocalMock, event.ContentAuditState,
		"命中 mock 的事件必须标记内容审计未执行")
	require.Zero(t, event.AccountID, "本地 mock 未选号，账号 ID 必须是 0")
	evaluated, enqueued, _ := engine.snapshot()
	require.Zero(t, evaluated, "命中 mock 不得执行提示词审计")
	require.Zero(t, enqueued, "命中 mock 不得入队提示词审计")
}

// 通过真实请求入口验证审计与 Mock 的组合，不用单独调用 Mock 闸门代替请求链路。
// 新优先级：严格命中 -> 跳过两类内容审计本地应答；未命中／Mock 关闭／带历史或工具
// 的请求 -> 照常执行审计并按原策略拒绝。
func TestGatewayMock_SecurityAuditCombination(t *testing.T) {
	gin.SetMode(gin.TestMode)
	entries := []string{"messages", "openai messages", "chat completions", "responses"}
	streams := []bool{false, true}

	t.Run("matched requests skip both content audits", func(t *testing.T) {
		allowModeration, allowCalls := newGatewayMockContentAudit(t, false)
		blockModeration, blockCalls := newGatewayMockContentAudit(t, true)
		type auditConfig struct {
			promptMode  securityaudit.Mode
			promptKind  securityaudit.DecisionKind
			promptError bool
			moderation  *service.ContentModerationService
			calls       *atomic.Int64
			legacyOnly  bool
		}
		configs := []struct {
			name string
			cfg  auditConfig
		}{
			{"audits disabled", auditConfig{legacyOnly: true}},
			{"prompt off", auditConfig{promptMode: securityaudit.ModeOff}},
			{"prompt async", auditConfig{promptMode: securityaudit.ModeAsync}},
			{"prompt blocking allows", auditConfig{promptMode: securityaudit.ModeBlocking, promptKind: securityaudit.DecisionAllow}},
			{"prompt flagged allows", auditConfig{promptMode: securityaudit.ModeBlocking, promptKind: securityaudit.DecisionFlag}},
			{"prompt blocks", auditConfig{promptMode: securityaudit.ModeBlocking, promptKind: securityaudit.DecisionBlock}},
			{"prompt unavailable", auditConfig{promptMode: securityaudit.ModeBlocking, promptKind: securityaudit.DecisionUnavailable}},
			{"prompt invalid", auditConfig{promptMode: securityaudit.ModeBlocking, promptKind: securityaudit.DecisionInvalid}},
			{"prompt error", auditConfig{promptMode: securityaudit.ModeBlocking, promptError: true}},
			{"content audit allows", auditConfig{moderation: allowModeration, calls: allowCalls}},
			{"content audit blocks", auditConfig{moderation: blockModeration, calls: blockCalls}},
			{"prompt and content both configured", auditConfig{promptMode: securityaudit.ModeBlocking, promptKind: securityaudit.DecisionUnavailable, moderation: blockModeration, calls: blockCalls}},
		}

		for _, tc := range configs {
			t.Run(tc.name, func(t *testing.T) {
				engine := &handlerPromptEngine{mode: tc.cfg.promptMode}
				if tc.cfg.promptKind != "" {
					engine.decision = &securityaudit.PromptDecision{Kind: tc.cfg.promptKind}
				}
				if tc.cfg.promptError {
					engine.err = errors.New("test audit unavailable")
				}
				coordinator := securityaudit.NewCoordinator(securityaudit.NewLegacyModerationAdapter(tc.cfg.moderation), engine)
				if tc.cfg.legacyOnly {
					coordinator = nil
				}
				for _, entry := range entries {
					t.Run(entry, func(t *testing.T) {
						for _, stream := range streams {
							name := "nonstream"
							if stream {
								name = "stream"
							}
							t.Run(name, func(t *testing.T) {
								var callsBefore int64
								if tc.cfg.calls != nil {
									callsBefore = tc.cfg.calls.Load()
								}
								events := &gatewayMockAuditEventStore{}
								raw := gatewayMockAuditBody(t, entry, "hi", stream, false, false)
								w, upstream := serveGatewayMockAuditBody(t, entry, raw, true, coordinator, tc.cfg.moderation, events)

								require.Equal(t, http.StatusOK, w.Code, w.Body.String())
								require.Empty(t, upstream, "命中 mock 不得请求上游")
								require.Equal(t, service.GatewayMockReplyHeaderValue, w.Header().Get(service.GatewayMockReplyHeader))
								require.Contains(t, w.Body.String(), gatewayMockAuditReply)
								require.Equal(t, int64(1), events.calls.Load(), "本地回复应产生一条真实命中记录")
								event := events.snapshot()
								require.Equal(t, service.GatewayMockContentAuditSkippedLocalMock, event.ContentAuditState)
								require.Zero(t, event.AccountID, "本地 mock 未选号，账号 ID 必须是 0")
								if tc.cfg.calls != nil {
									require.Equal(t, callsBefore, tc.cfg.calls.Load(), "命中 mock 不得执行内容审计")
								}
								if entry == "messages" || entry == "openai messages" {
									require.Contains(t, w.Body.String(), `"input_tokens":0`)
									require.Contains(t, w.Body.String(), `"output_tokens":0`)
								} else {
									require.Contains(t, w.Body.String(), `"total_tokens":0`)
								}
							})
						}
					})
				}
				if !tc.cfg.legacyOnly {
					evaluated, enqueued, _ := engine.snapshot()
					require.Zero(t, evaluated, "命中 mock 不得评估提示词")
					require.Zero(t, enqueued, "命中 mock 不得入队提示词")
				}
			})
		}
	})

	t.Run("unmatched, disabled, history and tools are still audited and rejected", func(t *testing.T) {
		engine := &handlerPromptEngine{mode: securityaudit.ModeBlocking, decision: &securityaudit.PromptDecision{Kind: securityaudit.DecisionBlock}}
		coordinator := securityaudit.NewCoordinator(nil, engine)
		cases := []struct {
			name    string
			text    string
			enabled bool
			history bool
			tools   bool
		}{
			{name: "no matching keyword", text: "你好今天天气怎么样", enabled: true},
			{name: "mock disabled", text: "hi", enabled: false},
			{name: "history request", text: "hi", enabled: true, history: true},
			{name: "tools request", text: "hi", enabled: true, tools: true},
		}
		for _, mc := range cases {
			t.Run(mc.name, func(t *testing.T) {
				for _, entry := range entries {
					t.Run(entry, func(t *testing.T) {
						events := &gatewayMockAuditEventStore{}
						raw := gatewayMockAuditBody(t, entry, mc.text, false, mc.history, mc.tools)
						w, upstream := serveGatewayMockAuditBody(t, entry, raw, mc.enabled, coordinator, nil, events)

						require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
						require.Empty(t, upstream, "被审计拦截的请求不得请求上游")
						require.Contains(t, w.Body.String(), securityaudit.ErrorCodeBlocked)
						require.NotContains(t, w.Body.String(), gatewayMockAuditReply, "被审计拦截的请求不能用 Mock 成功响应掩盖")
						require.Empty(t, w.Header().Get(service.GatewayMockReplyHeader))
						require.Zero(t, events.calls.Load(), "未命中不得产生命中事件")
					})
				}
			})
		}
		evaluated, _, _ := engine.snapshot()
		require.GreaterOrEqual(t, evaluated, len(cases)*len(entries), "未命中请求必须在选号、计费、转发之前真实执行提示词审计")
	})

	t.Run("miss with audit unavailable returns service unavailable", func(t *testing.T) {
		engine := &handlerPromptEngine{mode: securityaudit.ModeBlocking, decision: &securityaudit.PromptDecision{Kind: securityaudit.DecisionUnavailable}}
		coordinator := securityaudit.NewCoordinator(nil, engine)
		events := &gatewayMockAuditEventStore{}
		raw := gatewayMockAuditBody(t, "messages", "你好今天天气怎么样", false, false, false)
		w, upstream := serveGatewayMockAuditBody(t, "messages", raw, true, coordinator, nil, events)

		require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), securityaudit.ErrorCodeUnavailable)
		require.Empty(t, upstream)
		require.Zero(t, events.calls.Load())
	})
}

// gatewayMockAuditBody 为四个入口构造同一语义的请求体：text 为唯一用户文本，
// history=true 追加助手历史，tools=true 声明工具能力（两者都必须走审计而非 Mock）。
func gatewayMockAuditBody(t *testing.T, entry, text string, stream bool, history, tools bool) string {
	t.Helper()
	var body map[string]any
	switch entry {
	case "messages", "openai messages":
		messages := []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}}
		if history {
			messages = []any{
				map[string]any{"role": "user", "content": text},
				map[string]any{"role": "assistant", "content": "prev"},
			}
		}
		body = map[string]any{"model": requestTraceMatrixModel, "max_tokens": 256, "stream": stream, "messages": messages}
	case "chat completions":
		messages := []any{map[string]any{"role": "user", "content": text}}
		if history {
			messages = []any{
				map[string]any{"role": "user", "content": text},
				map[string]any{"role": "assistant", "content": "prev"},
			}
		}
		body = map[string]any{"model": "gpt-5.1", "stream": stream, "messages": messages}
	case "responses":
		if history {
			body = map[string]any{"model": "gpt-5.1", "stream": stream, "input": []any{
				map[string]any{"type": "message", "role": "user", "content": text},
				map[string]any{"type": "message", "role": "assistant", "content": "prev"},
			}}
		} else {
			body = map[string]any{"model": "gpt-5.1", "stream": stream, "input": text}
		}
	default:
		t.Fatalf("unknown entry %q", entry)
	}
	if tools {
		body["tools"] = []any{map[string]any{"type": "function", "name": "lookup"}}
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return string(raw)
}

// configureGatewayMockRules 写入启用或禁用的 Mock 规则。
func configureGatewayMockRules(t *testing.T, repo *gatewayMockSettingsRepo, enabled bool) {
	t.Helper()
	encoded, err := json.Marshal(service.GatewayMockSettings{
		Enabled: enabled,
		Rules:   []service.GatewayMockRule{{ID: "rule-1", Keyword: "hi", Reply: gatewayMockAuditReply, Enabled: true}},
	})
	require.NoError(t, err)
	require.NoError(t, repo.Set(context.Background(), service.SettingKeyGatewayMock, string(encoded)))
}

// serveGatewayMockAuditBody 用真实入口叠加审计依赖发送原始请求体。
func serveGatewayMockAuditBody(t *testing.T, entry, raw string, enabled bool, coordinator *securityaudit.Coordinator, moderation *service.ContentModerationService, events service.GatewayMockEventStore) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	if entry == "messages" {
		env := newGatewayMockEnv(t)
		configureGatewayMockRules(t, env.settingRepo, enabled)
		env.handler.securityAuditCoordinator = coordinator
		env.handler.contentModerationService = moderation
		env.handler.SetGatewayMockEventStore(events)
		w := env.serve(t, "/v1/messages", raw)
		return w, upstreamPaths(env.capture)
	}
	repo := &gatewayMockSettingsRepo{values: map[string]string{}}
	configureGatewayMockRules(t, repo, enabled)
	switch entry {
	case "openai messages":
		env := newRequestTraceMatrixOpenAIEnv(t)
		env.openai.SetSettingService(gatewayMockSettingService(t, repo))
		env.openai.securityAuditCoordinator = coordinator
		env.openai.contentModerationService = moderation
		env.openai.SetGatewayMockEventStore(events)
		w := env.serve(t, "/v1/messages", raw, env.openai.Messages)
		return w, upstreamPaths(env.capture)
	case "chat completions":
		env := newRequestTraceRealOpenAIEnv(t, false)
		env.handler.SetSettingService(gatewayMockSettingService(t, repo))
		env.handler.securityAuditCoordinator = coordinator
		env.handler.contentModerationService = moderation
		env.handler.SetGatewayMockEventStore(events)
		w := env.serve(t, "/v1/chat/completions", raw, env.handler.ChatCompletions)
		return w, realUpstreamPaths(env.capture)
	case "responses":
		env := newRequestTraceRealOpenAIEnv(t, false)
		env.handler.SetSettingService(gatewayMockSettingService(t, repo))
		env.handler.securityAuditCoordinator = coordinator
		env.handler.contentModerationService = moderation
		env.handler.SetGatewayMockEventStore(events)
		w := env.serve(t, "/v1/responses", raw, env.handler.Responses)
		return w, realUpstreamPaths(env.capture)
	default:
		t.Fatalf("unknown entry %q", entry)
		return nil, nil
	}
}

func serveGatewayMockAuditRequest(t *testing.T, entry string, stream bool, coordinator *securityaudit.Coordinator, moderation *service.ContentModerationService, events service.GatewayMockEventStore) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	return serveGatewayMockAuditBody(t, entry, gatewayMockAuditBody(t, entry, "hi", stream, false, false), true, coordinator, moderation, events)
}

// 内容审计使用真实服务和本地 HTTP 审计节点，设置与记录仓库仅替换外部存储。
func newGatewayMockContentAudit(t *testing.T, blocked bool) (*service.ContentModerationService, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		score := 0.0
		if blocked {
			score = 1.0
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"flagged": blocked, "category_scores": map[string]float64{"sexual": score}}}})
	}))
	t.Cleanup(server.Close)
	cfg := service.ContentModerationConfig{
		Enabled: true, Mode: service.ContentModerationModePreBlock,
		BaseURL: server.URL, APIKeys: []string{"test-audit-key"}, SampleRate: 100, AllGroups: true,
		BlockStatus: http.StatusBadRequest, BlockMessage: "测试内容审计拒绝", RetryCount: 0,
		Thresholds: map[string]float64{"sexual": 0.5},
	}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	repo := &gatewayMockAuditSettingsRepo{gatewayMockSettingsRepo{values: map[string]string{
		service.SettingKeyRiskControlEnabled:      "true",
		service.SettingKeyContentModerationConfig: string(raw),
	}}}
	moderation := service.NewContentModerationService(repo, &gatewayMockAuditLogRepo{}, nil, nil, nil, nil, nil, nil)
	return moderation, calls
}

type gatewayMockAuditSettingsRepo struct {
	gatewayMockSettingsRepo
}

func (r *gatewayMockAuditSettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			values[key] = value
		}
	}
	return values, nil
}

type gatewayMockAuditEventStore struct {
	calls atomic.Int64
	mu    sync.Mutex
	last  service.GatewayMockEventInput
}

func (s *gatewayMockAuditEventStore) RecordGatewayMockEvent(_ context.Context, input service.GatewayMockEventInput) error {
	s.calls.Add(1)
	s.mu.Lock()
	s.last = input
	s.mu.Unlock()
	return nil
}

func (s *gatewayMockAuditEventStore) snapshot() service.GatewayMockEventInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

type gatewayMockAuditLogRepo struct {
	service.ContentModerationRepository
}

func (*gatewayMockAuditLogRepo) CreateLog(context.Context, *service.ContentModerationLog) error {
	return nil
}

func (*gatewayMockAuditLogRepo) CleanupExpiredLogs(context.Context, time.Time, time.Time) (*service.ContentModerationCleanupResult, error) {
	return &service.ContentModerationCleanupResult{}, nil
}
