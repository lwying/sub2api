package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 request-trace ticket 05 的 wire 事实缺口：GatewayService.Forward 的
// Vertex service_account 分支走 Google AI Platform 的 rawPredict wire，但
// bindRequestAuditHTTPAttempt 把每次尝试都记成 anthropic.messages。真实 wire 协议族
// （Metadata.ValueProtocol）必须只对**真实发出的** service_account HTTP 尝试记为
// vertex（含同账号重试），长期审计协议家族（含强制审计门禁语义）保持不变，
// 且绑定过程不得复制任何凭证。

const (
	vertexAuditAccessToken = "vertex-audit-access-token"
	vertexAuditPrivateKey  = "vertex-audit-private-key-material"
	// 服务账号 JSON 用最小可用字段（parseVertexServiceAccountKey 要求
	// client_email / private_key / project_id），值全部是本地合成常量。
	vertexAuditServiceAccountJSON  = `{"type":"service_account","project_id":"vertex-audit-proj","private_key_id":"vertex-audit-kid","private_key":"` + vertexAuditPrivateKey + `","client_email":"vertex-audit@vertex-audit-proj.iam.gserviceaccount.com","token_uri":"https://oauth2.googleapis.com/token"}`
	vertexAuditSuccessBody         = `{"id":"msg_audit","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
	vertexAuditSignatureErrBody    = `{"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content.0.thinking.signature: Invalid signature"}}`
	vertexAuditToolSigErrBody      = `{"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content.0.tool_use.signature: Invalid signature"}}`
	vertexAuditBudgetErrBody       = `{"type":"error","error":{"type":"invalid_request_error","message":"thinking.budget_tokens: Input should be greater than or equal to 1024"}}`
	vertexAuditRequestModel        = "claude-sonnet-4-5"
	vertexAuditRequestBody         = `{"model":"` + vertexAuditRequestModel + `","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`
	vertexAuditThinkingRequestBody = `{"model":"` + vertexAuditRequestModel + `","max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":512},"messages":[{"role":"user","content":"hello"}]}`
)

type vertexAuditTokenCache struct {
	token string
}

func (c *vertexAuditTokenCache) GetAccessToken(context.Context, string) (string, error) {
	return c.token, nil
}

func (c *vertexAuditTokenCache) SetAccessToken(context.Context, string, string, time.Duration) error {
	return nil
}

func (c *vertexAuditTokenCache) DeleteAccessToken(context.Context, string) error { return nil }

func (c *vertexAuditTokenCache) AcquireRefreshLock(context.Context, string, time.Duration) (bool, error) {
	return false, nil
}

func (c *vertexAuditTokenCache) ReleaseRefreshLock(context.Context, string) error { return nil }

// vertexAuditSettingRepo 只会被读取整流器与转发开关；返回空值即落到内置默认值。
type vertexAuditSettingRepo struct {
	SettingRepository
}

func (r *vertexAuditSettingRepo) GetValue(context.Context, string) (string, error) {
	return "", nil
}

func (r *vertexAuditSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

type vertexAuditUpstreamResponse struct {
	status int
	header http.Header
	body   string
}

// vertexAuditUpstream 复刻 internal/repository/http_upstream.go 的审计接缝：每次真实
// RoundTrip 都通过 httpattempt.StartRequestAttempt 追加一条 attempt，因此测试断言的是
// 与生产同一条事实通路，而不是服务层内部的中间变量。
type vertexAuditUpstream struct {
	planned  []vertexAuditUpstreamResponse
	requests []*http.Request
	urls     []string
}

func (u *vertexAuditUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func (u *vertexAuditUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("nil upstream request")
	}
	u.requests = append(u.requests, req)
	if req.URL != nil {
		u.urls = append(u.urls, req.URL.String())
	}
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
		req.Body = http.NoBody
	}
	if len(u.planned) == 0 {
		return nil, errors.New("no planned upstream response")
	}
	planned := u.planned[0]
	if len(u.planned) > 1 {
		u.planned = u.planned[1:]
	}
	resp := &http.Response{
		StatusCode: planned.status,
		Header:     planned.header,
		Body:       io.NopCloser(strings.NewReader(planned.body)),
	}
	if attempt := httpattempt.StartRequestAttempt(req); attempt != nil {
		attempt.SetResponse(resp.StatusCode, resp.Header, true)
	}
	return resp, nil
}

func vertexAuditForwardService(upstream HTTPUpstream) *GatewayService {
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	return &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     &RateLimitService{},
		deferredService:      &DeferredService{},
		settingService:       &SettingService{settingRepo: &vertexAuditSettingRepo{}},
		claudeTokenProvider:  &ClaudeTokenProvider{tokenCache: &vertexAuditTokenCache{token: vertexAuditAccessToken}},
	}
}

func vertexAuditServiceAccount() *Account {
	return &Account{
		ID:          601,
		Name:        "vertex-audit-service-account",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeServiceAccount,
		Concurrency: 1,
		Credentials: map[string]any{
			"project_id":           "vertex-audit-proj",
			"location":             "us-east5",
			"service_account_json": vertexAuditServiceAccountJSON,
		},
		Status:      StatusActive,
		Schedulable: true,
	}
}

func vertexAuditAnthropicAPIKeyAccount() *Account {
	return &Account{
		ID:          602,
		Name:        "vertex-audit-api-key",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-ant-audit",
			"base_url": "https://api.anthropic.com",
		},
		Status:      StatusActive,
		Schedulable: true,
	}
}

func vertexAuditGinContext(t *testing.T, body string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

func vertexAuditParsedRequest(t *testing.T, body string) *ParsedRequest {
	t.Helper()
	parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(body)), PlatformAnthropic)
	require.NoError(t, err)
	return parsed
}

// 主验收：Vertex service_account 的真实出站尝试（含 400 签名错误后的两段同账号重试）
// 逐次记为 wire=vertex，同时长期审计协议家族仍是 anthropic.messages。
func TestGatewayService_ForwardVertexServiceAccountAuditsVertexWirePerAttempt(t *testing.T) {
	upstream := &vertexAuditUpstream{planned: []vertexAuditUpstreamResponse{
		{
			status: http.StatusBadRequest,
			header: http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"rid-audit-1"}},
			body:   vertexAuditSignatureErrBody,
		},
		{
			status: http.StatusBadRequest,
			header: http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"rid-audit-2"}},
			body:   vertexAuditToolSigErrBody,
		},
		{
			status: http.StatusOK,
			header: http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"rid-audit-3"}},
			body:   vertexAuditSuccessBody,
		},
	}}
	svc := vertexAuditForwardService(upstream)
	account := vertexAuditServiceAccount()
	c := vertexAuditGinContext(t, vertexAuditRequestBody)

	result, err := svc.Forward(context.Background(), c, account, vertexAuditParsedRequest(t, vertexAuditRequestBody))
	require.NoError(t, err)
	require.NotNil(t, result)

	// 首发 + 签名整流重试 + 工具块降级重试 = 三次真实 HTTP 尝试，全部沿用同一账号身份。
	require.Len(t, upstream.urls, 3, "两段签名整流重试都必须真实发出请求")
	for _, url := range upstream.urls {
		require.Contains(t, url, "aiplatform.googleapis.com",
			"service_account 走 Vertex rawPredict wire：%s", url)
	}
	for _, req := range upstream.requests {
		require.Equal(t, "Bearer "+vertexAuditAccessToken, getHeaderRaw(req.Header, "authorization"))
	}

	metadata := RequestAuditHTTPAttemptMetadata(c)
	require.Len(t, metadata, 3, "每次真实尝试都要有一条 attempt 事实")
	for i, item := range metadata {
		require.Equal(t, RequestAuditProtocolAnthropic, item.Protocol,
			"attempt %d 长期审计协议家族不得改名", i+1)
		require.Equal(t, requestAuditValueWireProtocolVertex, item.ValueProtocol,
			"attempt %d 真实 wire 协议族必须是 vertex", i+1)
		require.Equal(t, requestAuditValueWireProtocolVertex, RequestAuditValueWireProtocolForMetadata(item),
			"attempt %d 经 ValueProtocol 解析出的真实 wire 协议族也必须是 vertex", i+1)
		require.Equal(t, account.ID, item.AccountID)
	}

	// 绑定不得把凭证带进 Trace 事实（metadata 是纯常量 + 账号/模型标识）。
	blob, marshalErr := json.Marshal(metadata)
	require.NoError(t, marshalErr)
	require.NotContains(t, string(blob), vertexAuditPrivateKey)
	require.NotContains(t, string(blob), vertexAuditAccessToken)
}

// 主验收（budget 整流分支）：thinking budget 约束错误触发的同账号重试同样记为 vertex。
func TestGatewayService_ForwardVertexServiceAccountBudgetsRetryAsVertexWire(t *testing.T) {
	upstream := &vertexAuditUpstream{planned: []vertexAuditUpstreamResponse{
		{
			status: http.StatusBadRequest,
			header: http.Header{"Content-Type": []string{"application/json"}},
			body:   vertexAuditBudgetErrBody,
		},
		{
			status: http.StatusOK,
			header: http.Header{"Content-Type": []string{"application/json"}},
			body:   vertexAuditSuccessBody,
		},
	}}
	svc := vertexAuditForwardService(upstream)
	account := vertexAuditServiceAccount()
	c := vertexAuditGinContext(t, vertexAuditThinkingRequestBody)

	result, err := svc.Forward(context.Background(), c, account, vertexAuditParsedRequest(t, vertexAuditThinkingRequestBody))
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Len(t, upstream.urls, 2, "budget 整流必须真实发出第二次请求")
	for _, url := range upstream.urls {
		require.Contains(t, url, "aiplatform.googleapis.com")
	}

	metadata := RequestAuditHTTPAttemptMetadata(c)
	require.Len(t, metadata, 2)
	for i, item := range metadata {
		require.Equal(t, RequestAuditProtocolAnthropic, item.Protocol, "attempt %d", i+1)
		require.Equal(t, requestAuditValueWireProtocolVertex, item.ValueProtocol, "attempt %d", i+1)
	}
}

// 反面：Anthropic API Key 账号走 api.anthropic.com 的 Messages wire，ValueProtocol
// 必须回落到协议家族，不能被 Vertex 标签污染。
func TestGatewayService_ForwardAnthropicAPIKeyKeepsMessagesValueProtocol(t *testing.T) {
	upstream := &vertexAuditUpstream{planned: []vertexAuditUpstreamResponse{{
		status: http.StatusOK,
		header: http.Header{"Content-Type": []string{"application/json"}},
		body:   vertexAuditSuccessBody,
	}}}
	svc := vertexAuditForwardService(upstream)
	account := vertexAuditAnthropicAPIKeyAccount()
	c := vertexAuditGinContext(t, vertexAuditRequestBody)

	result, err := svc.Forward(context.Background(), c, account, vertexAuditParsedRequest(t, vertexAuditRequestBody))
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Len(t, upstream.urls, 1)
	require.Contains(t, upstream.urls[0], "api.anthropic.com")

	metadata := RequestAuditHTTPAttemptMetadata(c)
	require.Len(t, metadata, 1)
	require.Equal(t, RequestAuditProtocolAnthropic, metadata[0].Protocol)
	require.Equal(t, RequestAuditProtocolAnthropic, metadata[0].ValueProtocol,
		"非 service_account 尝试的 wire 协议族回落到协议家族，不得被标成 vertex")
	require.Equal(t, RequestAuditProtocolAnthropic, RequestAuditValueWireProtocolForMetadata(metadata[0]))
}

// 接缝：只有 Anthropic platform 的 service_account 判定为 Vertex wire。
// 仅凭 IsVertexServiceAccount()（只看 Type）会把其他平台的 service account 误标成 vertex。
func TestBindForwardRequestAuditHTTPAttemptVertexWirePredicate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name string
		// wantOverride 是挂在请求 context 上的显式 wire 协议族覆写；空表示不覆写。
		account      *Account
		wantOverride string
	}{
		{
			name:         "anthropic service account",
			account:      vertexAuditServiceAccount(),
			wantOverride: requestAuditValueWireProtocolVertex,
		},
		{
			name: "gemini service account",
			account: &Account{
				ID: 603, Platform: PlatformGemini, Type: AccountTypeServiceAccount, Credentials: map[string]any{},
			},
		},
		{
			name:    "anthropic api key",
			account: vertexAuditAnthropicAPIKeyAccount(),
		},
		{
			name: "anthropic oauth",
			account: &Account{
				ID: 604, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "oauth-token"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/messages", strings.NewReader(`{"model":"x"}`))
			req.Header.Set("authorization", "Bearer caller-token")

			bound := bindForwardRequestAuditHTTPAttempt(req, c, tc.account, "  claude-sonnet-4-5@20250929  ")
			require.NotNil(t, bound)

			override, _ := bound.Context().Value(requestAuditValueWireProtocolContextKey{}).(string)
			require.Equal(t, tc.wantOverride, override)

			// 没有覆写时，值快照回落到长期审计的协议家族。
			wantValueProtocol := tc.wantOverride
			if wantValueProtocol == "" {
				wantValueProtocol = RequestAuditProtocolAnthropic
			}

			attempt := httpattempt.StartRequestAttempt(bound)
			require.NotNil(t, attempt)
			metadata := RequestAuditHTTPAttemptMetadata(c)
			require.Len(t, metadata, 1)
			require.Equal(t, RequestAuditProtocolAnthropic, metadata[0].Protocol, "长期审计协议家族固定为 messages")
			require.Equal(t, wantValueProtocol, metadata[0].ValueProtocol)
			require.Equal(t, tc.account.ID, metadata[0].AccountID)
			require.Equal(t, "claude-sonnet-4-5@20250929", metadata[0].Model)
		})
	}
}

// 接缝：绑定只挂一个常量协议名，不得改动出站请求本身（URL / method / header / body）。
func TestBindForwardRequestAuditHTTPAttemptDoesNotMutateWireRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "https://us-east5-aiplatform.googleapis.com/v1/projects/p/locations/us-east5/publishers/anthropic/models/m:rawPredict", strings.NewReader(vertexAuditRequestBody))
	req.Header.Set("authorization", "Bearer "+vertexAuditAccessToken)
	req.Header.Set("content-type", "application/json")
	urlBefore := req.URL.String()
	headersBefore := req.Header.Clone()
	bodyBefore := readRequestBodyForTest(t, req)
	req.Body = io.NopCloser(strings.NewReader(string(bodyBefore)))

	bound := bindForwardRequestAuditHTTPAttempt(req, c, vertexAuditServiceAccount(), vertexAuditRequestModel)

	require.Equal(t, urlBefore, bound.URL.String())
	require.Equal(t, http.MethodPost, bound.Method)
	require.Equal(t, headersBefore, bound.Header)
	require.Equal(t, bodyBefore, readRequestBodyForTest(t, bound))
}

// 接缝：逐次尝试独立判定。同一条 Trace 内先发 service_account、再切换成 API Key 账号时，
// 第二条尝试不得继承上一条的 vertex（反之亦然）。
func TestBindForwardRequestAuditHTTPAttemptEvaluatesPerAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	first := httptest.NewRequest(http.MethodPost, "https://vertex.example/rawPredict", strings.NewReader("{}"))
	first = bindForwardRequestAuditHTTPAttempt(first, c, vertexAuditServiceAccount(), "m")
	require.NotNil(t, httpattempt.StartRequestAttempt(first))

	second := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
	second = bindForwardRequestAuditHTTPAttempt(second, c, vertexAuditAnthropicAPIKeyAccount(), "m")
	require.NotNil(t, httpattempt.StartRequestAttempt(second))

	metadata := RequestAuditHTTPAttemptMetadata(c)
	require.Len(t, metadata, 2)
	require.Equal(t, requestAuditValueWireProtocolVertex, metadata[0].ValueProtocol)
	require.Equal(t, RequestAuditProtocolAnthropic, metadata[1].ValueProtocol)
}
