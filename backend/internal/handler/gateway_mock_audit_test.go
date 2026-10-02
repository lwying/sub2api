//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 通过真实请求入口验证审计与 Mock 的组合，不用单独调用 Mock 闸门代替请求链路。
func TestGatewayMock_SecurityAuditCombination(t *testing.T) {
	gin.SetMode(gin.TestMode)
	allowModeration, allowAuditCalls := newGatewayMockContentAudit(t, false)
	blockModeration, blockAuditCalls := newGatewayMockContentAudit(t, true)
	tests := []struct {
		name         string
		promptMode   securityaudit.Mode
		promptKind   securityaudit.DecisionKind
		promptError  bool
		contentAudit bool
		contentBlock bool
		legacyOnly   bool
		status       int
		errorCode    string
	}{
		{name: "audits disabled", legacyOnly: true, status: http.StatusOK},
		{name: "prompt off", promptMode: securityaudit.ModeOff, status: http.StatusOK},
		{name: "prompt async", promptMode: securityaudit.ModeAsync, status: http.StatusOK},
		{name: "prompt blocking allows", promptMode: securityaudit.ModeBlocking, promptKind: securityaudit.DecisionAllow, status: http.StatusOK},
		{name: "prompt flagged allows", promptMode: securityaudit.ModeBlocking, promptKind: securityaudit.DecisionFlag, status: http.StatusOK},
		{name: "content audit allows", contentAudit: true, status: http.StatusOK},
		{name: "legacy content audit allows", contentAudit: true, legacyOnly: true, status: http.StatusOK},
		{name: "both audits allow", promptMode: securityaudit.ModeBlocking, promptKind: securityaudit.DecisionAllow, contentAudit: true, status: http.StatusOK},
		{name: "async prompt and content allow", promptMode: securityaudit.ModeAsync, contentAudit: true, status: http.StatusOK},
		{name: "prompt blocks before mock", promptMode: securityaudit.ModeBlocking, promptKind: securityaudit.DecisionBlock, status: http.StatusBadRequest, errorCode: securityaudit.ErrorCodeBlocked},
		{name: "prompt unavailable before mock", promptMode: securityaudit.ModeBlocking, promptKind: securityaudit.DecisionUnavailable, status: http.StatusServiceUnavailable, errorCode: securityaudit.ErrorCodeUnavailable},
		{name: "prompt invalid before mock", promptMode: securityaudit.ModeBlocking, promptKind: securityaudit.DecisionInvalid, status: http.StatusServiceUnavailable, errorCode: securityaudit.ErrorCodeInvalidResponse},
		{name: "prompt error before mock", promptMode: securityaudit.ModeBlocking, promptError: true, status: http.StatusServiceUnavailable, errorCode: securityaudit.ErrorCodeUnavailable},
		{name: "content blocks before mock", contentAudit: true, contentBlock: true, status: http.StatusBadRequest, errorCode: "content_policy_violation"},
		{name: "both audits content blocks", promptMode: securityaudit.ModeBlocking, promptKind: securityaudit.DecisionAllow, contentAudit: true, contentBlock: true, status: http.StatusBadRequest, errorCode: "content_policy_violation"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var moderation *service.ContentModerationService
			var auditCalls *atomic.Int64
			if tc.contentAudit {
				moderation, auditCalls = allowModeration, allowAuditCalls
				if tc.contentBlock {
					moderation, auditCalls = blockModeration, blockAuditCalls
				}
			}
			for _, entry := range []string{"messages", "openai messages", "chat completions", "responses"} {
				t.Run(entry, func(t *testing.T) {
					for _, stream := range []bool{false, true} {
						name := "nonstream"
						if stream {
							name = "stream"
						}
						t.Run(name, func(t *testing.T) {
							engine := &handlerPromptEngine{mode: tc.promptMode}
							if tc.promptKind != "" {
								engine.decision = &securityaudit.PromptDecision{Kind: tc.promptKind}
							}
							if tc.promptError {
								engine.err = errors.New("test audit unavailable")
							}
							coordinator := securityaudit.NewCoordinator(securityaudit.NewLegacyModerationAdapter(moderation), engine)
							if tc.legacyOnly {
								coordinator = nil
							}
							var callsBefore int64
							if auditCalls != nil {
								callsBefore = auditCalls.Load()
							}
							events := &gatewayMockAuditEventStore{}
							w, upstream := serveGatewayMockAuditRequest(t, entry, stream, coordinator, moderation, events)

							require.Equal(t, tc.status, w.Code, w.Body.String())
							require.Empty(t, upstream, "审计放行后应本地 Mock，拒绝时也不得请求上游")
							if tc.status == http.StatusOK {
								require.Equal(t, int64(1), events.calls.Load(), "本地回复应产生一条真实命中记录")
								require.Equal(t, service.GatewayMockReplyHeaderValue, w.Header().Get(service.GatewayMockReplyHeader), "开启审计且放行不能导致 Mock 失效")
								require.Contains(t, w.Body.String(), "本地审计组合测试回复")
								require.NotContains(t, w.Body.String(), "upstream")
								if entry == "messages" || entry == "openai messages" {
									require.Contains(t, w.Body.String(), `"input_tokens":0`)
									require.Contains(t, w.Body.String(), `"output_tokens":0`)
								} else {
									require.Contains(t, w.Body.String(), `"total_tokens":0`)
								}
							} else {
								require.Zero(t, events.calls.Load(), "审计提前拒绝时没有实际 Mock 命中，不应产生命中记录")
								require.Empty(t, w.Header().Get(service.GatewayMockReplyHeader))
								require.Contains(t, w.Body.String(), tc.errorCode)
								require.NotContains(t, w.Body.String(), "本地审计组合测试回复", "被审计拦截的请求不能用 Mock 成功响应掩盖")
							}
							if auditCalls != nil {
								require.Equal(t, callsBefore+1, auditCalls.Load(), "必须真实执行内容审计，不能以未配置或跳过审计冒充放行")
							}
							if !tc.legacyOnly {
								evaluated, enqueued, _ := engine.snapshot()
								switch tc.promptMode {
								case securityaudit.ModeBlocking:
									require.Equal(t, 1, evaluated)
									require.Zero(t, enqueued)
								case securityaudit.ModeAsync:
									require.Zero(t, evaluated)
									require.Equal(t, 1, enqueued)
								default:
									require.Zero(t, evaluated)
									require.Zero(t, enqueued)
								}
							}
						})
					}
				})
			}
		})
	}
}

func serveGatewayMockAuditRequest(t *testing.T, entry string, stream bool, coordinator *securityaudit.Coordinator, moderation *service.ContentModerationService, events service.GatewayMockEventStore) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	const reply = "本地审计组合测试回复"
	if entry == "messages" {
		env := newGatewayMockEnv(t)
		startGatewayMockRules(t, env.settingRepo, "hi", reply)
		env.handler.securityAuditCoordinator = coordinator
		env.handler.contentModerationService = moderation
		env.handler.SetGatewayMockEventStore(events)
		w := env.serve(t, "/v1/messages", gatewayMockMessagesBody("hi", stream))
		return w, upstreamPaths(env.capture)
	}
	repo := &gatewayMockSettingsRepo{values: map[string]string{}}
	startGatewayMockRules(t, repo, "hi", reply)
	if entry == "openai messages" {
		env := newRequestTraceMatrixOpenAIEnv(t)
		env.openai.SetSettingService(gatewayMockSettingService(t, repo))
		env.openai.securityAuditCoordinator = coordinator
		env.openai.contentModerationService = moderation
		env.openai.SetGatewayMockEventStore(events)
		w := env.serve(t, "/v1/messages", gatewayMockMessagesBody("hi", stream), env.openai.Messages)
		return w, upstreamPaths(env.capture)
	}
	env := newRequestTraceRealOpenAIEnv(t, false)
	env.handler.SetSettingService(gatewayMockSettingService(t, repo))
	env.handler.securityAuditCoordinator = coordinator
	env.handler.contentModerationService = moderation
	env.handler.SetGatewayMockEventStore(events)
	body := map[string]any{"model": "gpt-5.1", "stream": stream}
	path, handle := "/v1/responses", env.handler.Responses
	if entry == "chat completions" {
		path, handle = "/v1/chat/completions", env.handler.ChatCompletions
		body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
	} else {
		body["input"] = "hi"
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	w := env.serve(t, path, string(raw), handle)
	return w, realUpstreamPaths(env.capture)
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
		service.SettingKeyRiskControlEnabled: "true", service.SettingKeyContentModerationConfig: string(raw),
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
}

func (s *gatewayMockAuditEventStore) RecordGatewayMockEvent(context.Context, service.GatewayMockEventInput) error {
	s.calls.Add(1)
	return nil
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
