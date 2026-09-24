//go:build unit

package handler

// Ticket 02 — OpenAI 兼容真实换号入口的请求内 429 账号上限验收矩阵。
//
// 覆盖入口（各自有独立换号循环）：
//   - Responses (/v1/responses)
//   - Messages (/v1/messages)
//   - ChatCompletions (/v1/chat/completions)
//   - Images (/v1/images/generations)
//   - Embeddings (/v1/embeddings)
//
// ResponsesWebSocket 的接入点位于 ResponsesWebSocket 内的 handleWSFailover 闭包，
// 需要真实的上游 WS 服务端才能驱动，不计入本文件的可执行矩阵（见票面记录）。
//
// 断言口径全部落在对外可观察行为上：上游实际被调用的账号序列、客户端状态码与响应体，
// 不直接调用内部计数结构。

import (
	"bytes"
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
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// openAI429MatrixResponsesSSESuccess 是最小的 Responses 协议成功事件流，
// 含终止事件 response.completed。
const openAI429MatrixResponsesSSESuccess = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_healthy\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.1\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"

// ---------------------------------------------------------------------------
// 脚本化上游：按账号返回 429 / 5xx / 成功，并记录被调用的账号
// ---------------------------------------------------------------------------

type openAI429MatrixUpstream struct {
	service.HTTPUpstream

	mu              sync.Mutex
	hits            []int64
	statusByAccount map[int64]int
	onFirstDo       func()
	// quota429 让 429 带上配额重置信号，从而落入 OpenAI OAuth 的"配额耗尽"分支
	// 而非瞬时 429；后者会占用 2 分钟同账号重试窗口，不适合作为测试节奏。
	quota429 bool
	// responsesSSESuccess 让成功响应使用 Responses 协议的事件流；Chat Completions
	// 入口在 OpenAI 账号上以 Responses SSE 上游 + 缓冲转 JSON 的方式工作。
	responsesSSESuccess bool
}

func (u *openAI429MatrixUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.hits = append(u.hits, accountID)
	first := len(u.hits) == 1
	status, ok := u.statusByAccount[accountID]
	if !ok {
		status = http.StatusTooManyRequests
	}
	onFirstDo := u.onFirstDo
	quota429 := u.quota429
	responsesSSESuccess := u.responsesSSESuccess
	u.mu.Unlock()

	if first && onFirstDo != nil {
		onFirstDo()
	}

	switch {
	case status == http.StatusOK:
		if responsesSSESuccess {
			return openAI429MatrixResponseWithType(status, "text/event-stream", openAI429MatrixResponsesSSESuccess), nil
		}
		contentType, body := openAI429MatrixSuccessBody(req)
		return openAI429MatrixResponseWithType(status, contentType, body), nil
	case status == http.StatusTooManyRequests:
		if quota429 {
			resetsAt := time.Now().Add(time.Hour).Unix()
			return openAI429MatrixResponse(status, `{"error":{"type":"usage_limit_reached","message":"usage limit reached","resets_at":`+strconv.FormatInt(resetsAt, 10)+`}}`), nil
		}
		return openAI429MatrixResponse(status, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`), nil
	default:
		return openAI429MatrixResponse(status, `{"error":{"message":"upstream unavailable","type":"server_error"}}`), nil
	}
}

func openAI429MatrixResponse(status int, body string) *http.Response {
	return openAI429MatrixResponseWithType(status, "application/json", body)
}

func openAI429MatrixResponseWithType(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{contentType},
			"X-Request-Id": []string{"req_429_matrix"},
		},
		Body: io.NopCloser(bytes.NewBufferString(body)),
	}
}

// openAI429MatrixSuccessBody 按上游路径给出该入口可接受的最小成功载荷。
// Chat Completions 走 SSE 上游传输，必须给出带终止事件的事件流。
func openAI429MatrixSuccessBody(req *http.Request) (contentType string, body string) {
	path := ""
	if req != nil && req.URL != nil {
		path = req.URL.Path
	}
	switch {
	case strings.Contains(path, "/embeddings"):
		return "application/json", `{"object":"list","model":"text-embedding-3-small","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"usage":{"prompt_tokens":1,"total_tokens":1}}`
	case strings.Contains(path, "/images"):
		return "application/json", `{"created":1700000000,"data":[{"b64_json":"aGVsbG8=","revised_prompt":"a cat"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	case strings.Contains(path, "/chat/completions"):
		return "text/event-stream", openAI429MatrixResponsesSSESuccess
	default:
		return "application/json", `{"id":"resp_healthy","object":"response","created_at":1700000000,"model":"gpt-5.1","status":"completed","output":[{"type":"message","id":"msg_healthy","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	}
}

func (u *openAI429MatrixUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.hits...)
}

// distinctAccounts 折叠同账号的多次上游尝试，仅保留"不同账号"的出现顺序。
func distinctAccounts(hits []int64) []int64 {
	seen := make(map[int64]struct{}, len(hits))
	out := make([]int64, 0, len(hits))
	for _, id := range hits {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// ---------------------------------------------------------------------------
// 设置仓储：让入口读到管理员配置的上限（而非仅默认值 2）
// ---------------------------------------------------------------------------

type openAI429MatrixSettingRepo struct {
	service.SettingRepository

	mu     sync.Mutex
	values map[string]string
}

func newOpenAI429MatrixSettingRepo(limit int) *openAI429MatrixSettingRepo {
	values := map[string]string{}
	if limit > 0 {
		values[service.SettingKeyRateLimit429AccountLimit] = strconv.Itoa(limit)
	}
	return &openAI429MatrixSettingRepo{values: values}
}

func (r *openAI429MatrixSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", service.ErrSettingNotFound
}

func (r *openAI429MatrixSettingRepo) Get(_ context.Context, key string) (*service.Setting, error) {
	value, err := r.GetValue(context.Background(), key)
	if err != nil {
		return nil, err
	}
	return &service.Setting{Key: key, Value: value}, nil
}

func (r *openAI429MatrixSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *openAI429MatrixSettingRepo) GetAll(_ context.Context) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.values))
	for key, value := range r.values {
		out[key] = value
	}
	return out, nil
}

func (r *openAI429MatrixSettingRepo) Set(_ context.Context, key, value string) error {
	return r.SetMultiple(context.Background(), map[string]string{key: value})
}

func (r *openAI429MatrixSettingRepo) SetMultiple(_ context.Context, settings map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, value := range settings {
		r.values[key] = value
	}
	return nil
}

func (r *openAI429MatrixSettingRepo) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, key)
	return nil
}

// ---------------------------------------------------------------------------
// 环境构造
// ---------------------------------------------------------------------------

type openAI429MatrixEnv struct {
	handler *OpenAIGatewayHandler
}

// newOpenAI429MatrixEnv 构造一个接入了全站 429 账号上限的 OpenAI 兼容 handler。
// configuredLimit <= 0 表示不写设置（入口应回退到默认 2）。
func newOpenAI429MatrixEnv(t *testing.T, accounts []service.Account, upstream service.HTTPUpstream, configuredLimit int) *openAI429MatrixEnv {
	t.Helper()

	cfg := &config.Config{RunMode: config.RunModeSimple}
	accountRepo := openAIImagesFailoverAccountRepo{accounts: accounts}

	var settingService *service.SettingService
	if configuredLimit > 0 {
		settingService = service.NewSettingService(newOpenAI429MatrixSettingRepo(configuredLimit), cfg)
	}

	gatewayService := service.NewOpenAIGatewayService(
		accountRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		nil,
		nil,
		nil,
		upstream,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		settingService,
		nil,
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)

	handler := NewOpenAIGatewayHandler(
		gatewayService,
		service.NewConcurrencyService(nil),
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil,
		nil,
		nil,
		nil,
		cfg,
	)
	handler.maxAccountSwitches = 10

	return &openAI429MatrixEnv{handler: handler}
}

func openAI429MatrixAPIKeyAccounts(count int, accountType string) []service.Account {
	accounts := make([]service.Account, 0, count)
	for id := int64(1); id <= int64(count); id++ {
		credentials := map[string]any{"api_key": "sk-upstream", "base_url": "https://example.com"}
		if accountType == service.AccountTypeOAuth {
			credentials = map[string]any{"access_token": "token-" + strconv.FormatInt(id, 10)}
		}
		accounts = append(accounts, service.Account{
			ID:          id,
			Name:        "openai-429-matrix-" + strconv.FormatInt(id, 10),
			Platform:    service.PlatformOpenAI,
			Type:        accountType,
			Status:      service.StatusActive,
			Schedulable: true,
			Priority:    int(id),
			Credentials: credentials,
		})
	}
	return accounts
}

func openAI429MatrixGroup(id int64) *service.Group {
	return &service.Group{ID: id, Platform: service.PlatformOpenAI, Status: service.StatusActive}
}

func openAI429MatrixAPIKey(group *service.Group) *service.APIKey {
	groupID := group.ID
	return &service.APIKey{
		ID:      4290,
		GroupID: &groupID,
		Group:   group,
		User:    &service.User{ID: 4290},
	}
}

func openAI429MatrixContext(t *testing.T, apiKey *service.APIKey, path string, body []byte, ctx context.Context) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: apiKey.User.ID, Concurrency: 0})
	return c, rec
}

// ---------------------------------------------------------------------------
// 入口调用封装
// ---------------------------------------------------------------------------

func openAI429MatrixInvokeResponses(t *testing.T, env *openAI429MatrixEnv) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	c, rec := openAI429MatrixContext(t, openAI429MatrixAPIKey(openAI429MatrixGroup(4291)),
		"/v1/responses", []byte(`{"model":"gpt-5.1","stream":false,"input":"hello"}`), nil)
	env.handler.Responses(c)
	return rec, c
}

func openAI429MatrixInvokeChatCompletions(t *testing.T, env *openAI429MatrixEnv) *httptest.ResponseRecorder {
	t.Helper()
	group := openAI429MatrixGroup(4292)
	c, rec := openAI429MatrixContext(t, openAI429MatrixAPIKey(group),
		"/v1/chat/completions", []byte(`{"model":"gpt-5.1","stream":false,"messages":[{"role":"user","content":"hi"}]}`), nil)
	env.handler.ChatCompletions(c)
	return rec
}

func openAI429MatrixInvokeMessages(t *testing.T, env *openAI429MatrixEnv) *httptest.ResponseRecorder {
	t.Helper()
	group := openAI429MatrixGroup(4293)
	group.AllowMessagesDispatch = true
	c, rec := openAI429MatrixContext(t, openAI429MatrixAPIKey(group),
		"/v1/messages", []byte(`{"model":"gpt-5.1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`), nil)
	env.handler.Messages(c)
	return rec
}

func openAI429MatrixInvokeImages(t *testing.T, env *openAI429MatrixEnv) *httptest.ResponseRecorder {
	t.Helper()
	group := openAI429MatrixGroup(4294)
	group.AllowImageGeneration = true
	c, rec := openAI429MatrixContext(t, openAI429MatrixAPIKey(group),
		"/v1/images/generations", []byte(`{"model":"gpt-image-1","prompt":"draw a cat","size":"1024x1024"}`), nil)
	env.handler.Images(c)
	return rec
}

func openAI429MatrixInvokeEmbeddings(t *testing.T, env *openAI429MatrixEnv) *httptest.ResponseRecorder {
	t.Helper()
	c, rec := openAI429MatrixContext(t, openAI429MatrixAPIKey(openAI429MatrixGroup(4295)),
		"/v1/embeddings", []byte(`{"model":"text-embedding-3-small","input":"hello"}`), nil)
	env.handler.Embeddings(c)
	return rec
}

// ---------------------------------------------------------------------------
// Responses 入口
// ---------------------------------------------------------------------------

// openAI429SSEStartedUpstream 在账号 1 上先输出语义增量、再以 429 类错误结束；
// 账号 2 为健康事件流。用于验证"流已产生语义输出后不得再换号"。
type openAI429SSEStartedUpstream struct {
	service.HTTPUpstream

	mu   sync.Mutex
	hits []int64
}

func (u *openAI429SSEStartedUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.hits = append(u.hits, accountID)
	u.mu.Unlock()

	if accountID == 1 {
		return openAI429MatrixResponseWithType(http.StatusOK, "text/event-stream", strings.Join([]string{
			"event: response.created",
			`data: {"type":"response.created","response":{"id":"resp_started"}}`,
			"",
			"event: response.output_text.delta",
			`data: {"type":"response.output_text.delta","delta":"semantic output"}`,
			"",
			"event: response.failed",
			`data: {"type":"response.failed","response":{"id":"resp_started","status":"failed","error":{"code":"rate_limit_exceeded","message":"rate limited"}}}`,
			"",
		}, "\n")), nil
	}
	return openAI429MatrixResponseWithType(http.StatusOK, "text/event-stream", openAI429MatrixResponsesSSESuccess), nil
}

func (u *openAI429SSEStartedUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.hits...)
}

// 上游在已经输出语义增量后才返回 429 类失败时，不得凭新的账号上限继续换号。
func TestOpenAI429AccountLimit_ResponsesStreamAlreadyStartedDoesNotSwitchAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429SSEStartedUpstream{}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, 0)

	c, rec := openAI429MatrixContext(t, openAI429MatrixAPIKey(openAI429MatrixGroup(4291)),
		"/v1/responses", []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`), nil)
	env.handler.Responses(c)

	require.Equal(t, []int64{1}, upstream.calls(), "流已产生语义输出后不得换号，且必须先触达首个账号")
	require.Equal(t, http.StatusOK, rec.Code, "SSE 响应头在流开始时已提交，状态码不再改变")
	require.NotContains(t, rec.Body.String(), "resp_healthy",
		"不得把健康账号的成功响应补写到已经开始的流")
}

// 管理员把上限调成 1 后，第一个账号最终 429 即停止；证明该入口读取的是配置值
// 而不是写死的默认 2。
func TestOpenAI429AccountLimit_ResponsesReadsConfiguredLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429MatrixUpstream{}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, 1)

	rec, _ := openAI429MatrixInvokeResponses(t, env)

	require.Equal(t, []int64{1}, distinctAccounts(upstream.calls()), "上限为 1 时不应换到第二个账号")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// 上限可被管理员调高：调成 3 后第三个账号也应被尝试，而第四个不试。
// 与 limit=1 的用例一起证明读取的是配置值，而不是写死的 2。
func TestOpenAI429AccountLimit_ResponsesHigherConfiguredLimitAllowsThirdAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429MatrixUpstream{}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(4, service.AccountTypeAPIKey), upstream, 3)

	rec, _ := openAI429MatrixInvokeResponses(t, env)

	require.Equal(t, []int64{1, 2, 3}, distinctAccounts(upstream.calls()), "上限为 3 时应允许尝试第三个账号")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// 非 429 的 5xx 失败不占 429 账号额度：账号 1 先 500，账号 2/3 各自最终 429 时才触顶，
// 账号 4 不再被尝试。若 5xx 误占额度，会在账号 2 之后提前停止。
func TestOpenAI429AccountLimit_ResponsesNon429FailureDoesNotConsumeBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429MatrixUpstream{statusByAccount: map[int64]int{1: http.StatusInternalServerError}}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(4, service.AccountTypeAPIKey), upstream, 0)

	rec, _ := openAI429MatrixInvokeResponses(t, env)

	require.Equal(t, []int64{1, 2, 3}, distinctAccounts(upstream.calls()), "5xx 不应消耗 429 账号额度")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// 第一个账号最终 429、第二个账号成功 → 对外成功，且不再切换。
func TestOpenAI429AccountLimit_ResponsesSecondAccountSuccessReturnsSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429MatrixUpstream{statusByAccount: map[int64]int{2: http.StatusOK}}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, 0)

	rec, _ := openAI429MatrixInvokeResponses(t, env)

	require.Equal(t, []int64{1, 2}, upstream.calls(), "账号 2 成功即结束本次逻辑请求")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// 池模式账号的同账号多次 429 只算一个不同账号：账号 1 被重试多次后仍只占 1 个额度，
// 触顶发生在账号 2 的最终 429，账号 3 不被尝试。
func TestOpenAI429AccountLimit_ResponsesSameAccountRetriesCountOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)

	accounts := openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey)
	accounts[0].Credentials["pool_mode"] = true

	upstream := &openAI429MatrixUpstream{}
	env := newOpenAI429MatrixEnv(t, accounts, upstream, 0)

	rec, _ := openAI429MatrixInvokeResponses(t, env)

	hits := upstream.calls()
	retriesOnFirst := 0
	for _, id := range hits {
		if id == 1 {
			retriesOnFirst++
		}
	}
	require.Greater(t, retriesOnFirst, 1, "池模式账号应先耗尽同账号重试")
	require.Equal(t, []int64{1, 2}, distinctAccounts(hits), "同账号多次 429 只计一个账号")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// OpenAI OAuth 的既有更严格换号规则不被放宽：即便把全站上限调到远高于该规则的 100，
// 账号 4 仍不会被尝试。
func TestOpenAI429AccountLimit_ResponsesKeepsOAuthStricterRule(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429MatrixUpstream{quota429: true}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(4, service.AccountTypeOAuth), upstream, 100)

	rec, _ := openAI429MatrixInvokeResponses(t, env)

	require.Equal(t, []int64{1, 2, 3}, distinctAccounts(upstream.calls()),
		"OAuth 更严格的换号上限（3 个账号）必须先于全站上限（100）生效")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// 下一次逻辑请求独立计数：同一个 handler 连续两次调用都会各自尝试账号 1 和 2。
func TestOpenAI429AccountLimit_ResponsesNextRequestStartsFresh(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429MatrixUpstream{}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, 0)

	rec, _ := openAI429MatrixInvokeResponses(t, env)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	firstCall := distinctAccounts(upstream.calls())

	afterFirstCall := len(upstream.calls())
	rec, _ = openAI429MatrixInvokeResponses(t, env)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	secondCall := distinctAccounts(upstream.calls()[afterFirstCall:])

	require.Equal(t, []int64{1, 2}, firstCall)
	require.Equal(t, []int64{1, 2}, secondCall, "新调用必须重新开始计数")
}

// 客户端已断开时不再换号，且不把取消误报成 429 账号上限触顶。
func TestOpenAI429AccountLimit_ResponsesClientCancelSwitchesNoAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upstream := &openAI429MatrixUpstream{onFirstDo: cancel}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, 0)

	c, rec := openAI429MatrixContext(t, openAI429MatrixAPIKey(openAI429MatrixGroup(4291)),
		"/v1/responses", []byte(`{"model":"gpt-5.1","stream":false,"input":"hello"}`), ctx)
	env.handler.Responses(c)

	require.Equal(t, []int64{1}, upstream.calls(), "客户端断开后不应再切换到账号 2")
	require.NotEqual(t, http.StatusTooManyRequests, rec.Code, "取消不应被误报成 429 账号上限触顶")
	require.Zero(t, rec.Body.Len(), "客户端断开时不应再补写错误响应体")
}

// 触顶后沿用既有 429 映射，且不泄露上游正文或账号凭据。
func TestOpenAI429AccountLimit_ResponsesExhaustedResponseDoesNotLeakUpstreamDetail(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429MatrixUpstream{}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, 0)

	rec, _ := openAI429MatrixInvokeResponses(t, env)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	body := rec.Body.String()
	require.NotContains(t, body, "rate limited", "不应回传上游错误正文")
	require.NotContains(t, body, "sk-upstream", "不应回传上游凭据")
	require.NotContains(t, body, "openai-429-matrix", "不应回传上游账号标识")
}

// ---------------------------------------------------------------------------
// Chat Completions 入口
// ---------------------------------------------------------------------------

func TestOpenAI429AccountLimit_ChatCompletionsStopsBeforeThirdAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429MatrixUpstream{}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, 0)

	rec := openAI429MatrixInvokeChatCompletions(t, env)

	require.Equal(t, []int64{1, 2}, upstream.calls(), "第二个账号最终 429 后不应尝试账号 3")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

func TestOpenAI429AccountLimit_ChatCompletionsSecondAccountSuccessReturnsSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429MatrixUpstream{statusByAccount: map[int64]int{2: http.StatusOK}, responsesSSESuccess: true}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, 0)

	rec := openAI429MatrixInvokeChatCompletions(t, env)

	require.Equal(t, []int64{1, 2}, upstream.calls())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// ---------------------------------------------------------------------------
// Messages 入口
// ---------------------------------------------------------------------------

func TestOpenAI429AccountLimit_MessagesStopsBeforeThirdAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429MatrixUpstream{}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, 0)

	rec := openAI429MatrixInvokeMessages(t, env)

	require.Equal(t, []int64{1, 2}, upstream.calls(), "第二个账号最终 429 后不应尝试账号 3")
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
}

func TestOpenAI429AccountLimit_MessagesSecondAccountSuccessReturnsSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429MatrixUpstream{statusByAccount: map[int64]int{2: http.StatusOK}, responsesSSESuccess: true}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, 0)

	rec := openAI429MatrixInvokeMessages(t, env)

	require.Equal(t, []int64{1, 2}, upstream.calls(), "账号 2 成功即结束本次逻辑请求")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// ---------------------------------------------------------------------------
// Images 入口（API-key 账号：OAuth 瞬时 429 会占用 2 分钟同账号重试窗口）
// ---------------------------------------------------------------------------

func TestOpenAI429AccountLimit_ImagesStopsBeforeThirdAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429MatrixUpstream{}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, 0)

	rec := openAI429MatrixInvokeImages(t, env)

	require.Equal(t, []int64{1, 2}, upstream.calls(), "第二个账号最终 429 后不应尝试账号 3")
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
}

// ---------------------------------------------------------------------------
// Embeddings 入口
// ---------------------------------------------------------------------------

func TestOpenAI429AccountLimit_EmbeddingsStopsBeforeThirdAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429MatrixUpstream{}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, 0)

	rec := openAI429MatrixInvokeEmbeddings(t, env)

	require.Equal(t, []int64{1, 2}, upstream.calls(), "第二个账号最终 429 后不应尝试账号 3")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// 非 Responses 入口同样读取管理员配置的上限（不是只对 Responses 生效）。
func TestOpenAI429AccountLimit_EmbeddingsReadsConfiguredLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429MatrixUpstream{}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, 1)

	rec := openAI429MatrixInvokeEmbeddings(t, env)

	require.Equal(t, []int64{1}, distinctAccounts(upstream.calls()), "上限为 1 时不应换到第二个账号")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

func TestOpenAI429AccountLimit_EmbeddingsSecondAccountSuccessReturnsSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAI429MatrixUpstream{statusByAccount: map[int64]int{2: http.StatusOK}}
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, 0)

	rec := openAI429MatrixInvokeEmbeddings(t, env)

	require.Equal(t, []int64{1, 2}, upstream.calls())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
