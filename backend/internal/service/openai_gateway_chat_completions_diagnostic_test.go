//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 票据 03（Chat Completions 的完整错误诊断）在服务侧的接缝契约：
// 网关真实发往上游的每个 Chat Completions 尝试，收到上游 HTTP 4xx/5xx 时都必须
// 显式 opt-in 上游错误诊断，并且诊断里的协议是**客户端入口协议** chat_completions
// ——即使本次出站的 wire 形态是转换后的 Responses 或原生 Messages。
//
// 与票据 01／02 的 Messages 接缝共用同一个观察者实现（error_diagnostic_observer.go），
// 因此这里断言的是 CC 绑定点必须提供的部分：绑定时机、协议归属、尝试序号与字节保真。
//
// 测试替身说明：假上游在每次 Do 里对本地假 HTTP 服务器做一次真实 RoundTrip，并按
// internal/repository/http_upstream.go:397-426 的公开接缝（httpattempt 的
// NewDiagnosticBodyCapture / CountingReadCloser / ObserveUpstreamError）把真实收到的
// 4xx/5xx 交给请求上绑定的观察者。该接缝在通用传输路径上对每个上游请求都会执行
// （Do / DoWithTLS 都无条件经 httpClientWithGrokAccessDeniedFallback 走 roundTripAttempt，
// 包装名只反映它最初的用途），所以本文件验收的是「服务侧是否把观察者绑上去、绑成什么协议」，
// 传输层自身的采集行为由 repository 的聚焦测试覆盖。

const (
	ccDiagnosticCaptureSettings = `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":true}`
	ccDiagnosticMetadataSetting = `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":false}`
	// ccDiagnosticCanary 是只出现在用户消息正文里的哨兵，用来断言留存的就是真实出站字节。
	// 它只是普通入站内容，不是被合成出来的凭据字段。
	ccDiagnosticCanary = "cc-diagnostic-canary-7b41"
)

// ccDiagnosticSuccessSSE 是转换后的 Responses 上游的成功响应（票 03 的「4xx 后成功」）。
var ccDiagnosticSuccessSSE = strings.Join([]string{
	`data: {"type":"response.completed","response":{"id":"resp_cc_diag","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}]},"usage":{"input_tokens":11,"output_tokens":5,"total_tokens":16}}`,
	"",
	"data: [DONE]",
	"",
}, "\n")

// ccDiagnosticAnthropicSuccessSSE 是原生 Messages 上游的成功响应。
var ccDiagnosticAnthropicSuccessSSE = strings.Join([]string{
	`event: message_start`,
	`data: {"type":"message_start","message":{"id":"msg_cc_native","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"stop_reason":null,"usage":{"input_tokens":9,"output_tokens":0}}}`,
	"",
	`event: content_block_start`,
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	"",
	`event: content_block_delta`,
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
	"",
	`event: message_delta`,
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
	"",
	`event: message_stop`,
	`data: {"type":"message_stop"}`,
	"",
}, "\n")

// ccDiagnosticSettingRepo 只回答错误诊断门控，其余设置键按未配置处理。
type ccDiagnosticSettingRepo struct {
	SettingRepository
	value string
}

func (r ccDiagnosticSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if key == SettingKeyErrorDiagnosticRiskAcknowledgement {
		// 门控真正打开还要求一条覆盖当前语句版本的有效书面确认
		// （ApplyErrorDiagnosticRiskAcknowledgement），夹具据此给出。
		return errorDiagnosticAckJSON(ErrorDiagnosticRiskAcknowledgementVersion), nil
	}
	if key != SettingKeyErrorDiagnostic || r.value == "" {
		return "", ErrSettingNotFound
	}
	return r.value, nil
}

// ccDiagnosticRecorder 只实现诊断写入契约，记录每次写入的尝试内容。
type ccDiagnosticRecorder struct {
	mu       sync.Mutex
	attempts []ErrorDiagnosticAttempt
}

func (r *ccDiagnosticRecorder) RecordErrorDiagnostic(_ context.Context, attempt ErrorDiagnosticAttempt) (ErrorDiagnosticRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts = append(r.attempts, attempt)
	return ErrorDiagnosticRecord{}, nil
}

func (r *ccDiagnosticRecorder) snapshot() []ErrorDiagnosticAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ErrorDiagnosticAttempt(nil), r.attempts...)
}

func (r *ccDiagnosticRecorder) waitFor(t *testing.T, want int) []ErrorDiagnosticAttempt {
	t.Helper()
	require.Eventually(t, func() bool {
		return len(r.snapshot()) >= want
	}, 2*time.Second, 5*time.Millisecond, "waited for %d diagnostic record(s)", want)
	return r.snapshot()
}

// ccDiagnosticUpstream 是假上游：真实 RoundTrip + 传输层公开观察接缝。
type ccDiagnosticUpstream struct {
	t        *testing.T
	server   *httptest.Server
	statuses []int

	mu              sync.Mutex
	payloads        []string
	received        [][]byte
	transportErrors []error
}

func newCCDiagnosticUpstream(t *testing.T, statuses []int, payloads []string) *ccDiagnosticUpstream {
	t.Helper()

	upstream := &ccDiagnosticUpstream{t: t, statuses: statuses, payloads: payloads}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		upstream.mu.Lock()
		index := len(upstream.received)
		upstream.received = append(upstream.received, body)
		upstream.mu.Unlock()

		status := http.StatusOK
		if index < len(upstream.statuses) {
			status = upstream.statuses[index]
		}
		payload := ""
		if index < len(upstream.payloads) {
			payload = upstream.payloads[index]
		}
		contentType := "text/event-stream"
		if status >= 400 {
			contentType = "application/json"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("x-request-id", "cc-diagnostic-rid")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *ccDiagnosticUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.roundTrip(req)
}

func (u *ccDiagnosticUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.roundTrip(req)
}

func (u *ccDiagnosticUpstream) roundTrip(req *http.Request) (*http.Response, error) {
	// 传输层故障（DNS/TCP/TLS，没有 HTTP 响应）：不发送、不观察，
	// 因此不可能被伪造成一次上游 4xx/5xx。
	if transportErr := u.nextTransportError(); transportErr != nil {
		return nil, transportErr
	}

	attempt := httpattempt.StartRequestAttempt(req)
	capture := httpattempt.NewDiagnosticBodyCapture(req)
	defer capture.Release()

	request := req.Clone(req.Context())
	request.URL.Scheme = "http"
	request.URL.Host = strings.TrimPrefix(u.server.URL, "http://")
	request.Host = ""
	if (attempt != nil || capture != nil) && request.Body != nil && request.Body != http.NoBody {
		request.Body = &httpattempt.CountingReadCloser{
			ReadCloser: request.Body,
			OnRead:     attempt.AddRequestBytes,
			Capture:    capture,
		}
	}

	resp, err := http.DefaultTransport.RoundTrip(request)
	if resp != nil {
		if attempt != nil {
			attempt.SetResponse(resp.StatusCode, resp.Header, resp.Body != nil && resp.Body != http.NoBody)
		}
		httpattempt.ObserveUpstreamError(req, attempt, resp, capture)
	}
	return resp, err
}

// failNextTransportError 让下一次 Do 以传输层故障结束：没有发送、没有 HTTP 响应。
func (u *ccDiagnosticUpstream) failNextTransportError(err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.transportErrors = append(u.transportErrors, err)
}

func (u *ccDiagnosticUpstream) nextTransportError() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.transportErrors) == 0 {
		return nil
	}
	err := u.transportErrors[0]
	u.transportErrors = u.transportErrors[1:]
	return err
}

func (u *ccDiagnosticUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.received)
}

func (u *ccDiagnosticUpstream) sentBody(t *testing.T, index int) []byte {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	require.Greater(t, len(u.received), index, "the fake upstream never saw attempt %d", index+1)
	return append([]byte(nil), u.received[index]...)
}

// newCCDiagnosticGateway 组装一个只覆盖 CC→Responses 转换分支的最小网关。
func newCCDiagnosticGateway(t *testing.T, upstream *ccDiagnosticUpstream, settings string, recorder ErrorDiagnosticRecorder) *OpenAIGatewayService {
	t.Helper()
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	svc.settingService = &SettingService{
		settingRepo: ccDiagnosticSettingRepo{value: settings},
		// 接缝只在稳定密钥可用时才 tee 出站正文；夹具照有密钥的部署建模，
		// 否则断言正文的用例会静默退化成未采正文。
		cfg: &config.Config{Totp: config.TotpConfig{
			EncryptionKey:           errorDiagnosticStableEncryptionKey,
			EncryptionKeyConfigured: true,
		}},
	}
	svc.SetErrorDiagnosticRecorder(recorder)
	require.NotNil(t, svc.errorDiagnostics, "the injected recorder must open the diagnostic seam")
	return svc
}

func newCCDiagnosticContext(t *testing.T, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder
}

func ccDiagnosticOAuthAccount() *Account {
	return &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}
}

// TestChatCompletionsErrorDiagnosticRecordsFailedAttemptBeforeRetrySucceeds 覆盖票 03 的
// 主要验收边界：先 4xx 后成功时，管理员能查到先前失败尝试的独立诊断，
// 其协议、状态、尝试序号与字节与上游实际所见一致；重试成功不产生第二条。
func TestChatCompletionsErrorDiagnosticRecordsFailedAttemptBeforeRetrySucceeds(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"` + ccDiagnosticCanary + `"}],"stream":false}`)
	c, _ := newCCDiagnosticContext(t, body)
	account := ccDiagnosticOAuthAccount()

	upstream := newCCDiagnosticUpstream(t,
		[]int{http.StatusBadRequest, http.StatusOK},
		[]string{`{"error":{"type":"invalid_request_error","message":"upstream rejected"}}`, ccDiagnosticSuccessSSE},
	)
	recorder := &ccDiagnosticRecorder{}
	svc := newCCDiagnosticGateway(t, upstream, ccDiagnosticCaptureSettings, recorder)

	// 第一次尝试：上游 4xx。诊断必须在这里就已经成立，不能等最终结果。
	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.Error(t, err)
	require.Nil(t, result)

	// 同一逻辑请求的第二次尝试：上游成功。
	retried, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.NotNil(t, retried)

	attempts := recorder.waitFor(t, 1)
	require.Len(t, attempts, 1, "只有收到上游 4xx 的那次尝试产生诊断，重试成功不产生第二条")

	got := attempts[0]
	// 入站是 Chat Completions：出站即使被转换成 Responses，协议也不能跟着漂移成 responses。
	require.Equal(t, ErrorDiagnosticProtocolChatCompletions, got.Protocol)
	require.Equal(t, ErrorDiagnosticStageWire, got.Stage)
	require.Equal(t, http.StatusBadRequest, got.UpstreamStatusCode)
	require.Equal(t, 1, got.AttemptIndex, "第一次真实发送的尝试序号")
	require.Zero(t, got.UsageLogID, "失败尝试没有使用记录，不得伪造用量关联")
	require.True(t, got.BodyReadComplete)
	require.Equal(t, upstream.sentBody(t, 0), got.Body,
		"诊断正文必须逐字节等于上游实际收到的出站 JSON")
	require.Contains(t, string(got.Body), ccDiagnosticCanary, "哨兵证明留存的是真实出站正文")
}

// TestChatCompletionsErrorDiagnosticCoversFailureWithoutUsage 覆盖票 03 的「全失败无 usage」：
// 没有使用记录的失败同样形成独立诊断，且不因此伪造一条用量。
func TestChatCompletionsErrorDiagnosticCoversFailureWithoutUsage(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"` + ccDiagnosticCanary + `"}],"stream":false}`)
	c, _ := newCCDiagnosticContext(t, body)
	account := ccDiagnosticOAuthAccount()

	upstream := newCCDiagnosticUpstream(t,
		[]int{http.StatusInternalServerError},
		[]string{`{"error":{"type":"server_error","message":"upstream failed"}}`},
	)
	recorder := &ccDiagnosticRecorder{}
	svc := newCCDiagnosticGateway(t, upstream, ccDiagnosticCaptureSettings, recorder)

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.Error(t, err)
	require.Nil(t, result)

	attempts := recorder.waitFor(t, 1)
	require.Len(t, attempts, 1)
	require.Equal(t, http.StatusInternalServerError, attempts[0].UpstreamStatusCode)
	require.Zero(t, attempts[0].UsageLogID, "无 usage 的诊断不得伪造用量关联")
	require.Equal(t, upstream.sentBody(t, 0), attempts[0].Body)
}

// TestChatCompletionsErrorDiagnosticKeepsClientProtocolOnNativeMessagesUpstream 覆盖票 03 的
// 「Chat Completions 可能转换协议」：入站是 Chat Completions、出站是原生 Messages 时，
// 诊断协议仍然必须是 chat_completions，不能跟着出站 wire 形态漂移成 messages。
func TestChatCompletionsErrorDiagnosticKeepsClientProtocolOnNativeMessagesUpstream(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"` + ccDiagnosticCanary + `"}],"stream":false}`)
	c, _ := newCCDiagnosticContext(t, body)

	upstream := newCCDiagnosticUpstream(t,
		[]int{http.StatusBadRequest, http.StatusOK},
		[]string{`{"type":"error","error":{"type":"invalid_request_error","message":"upstream rejected"}}`, ccDiagnosticAnthropicSuccessSSE},
	)
	recorder := &ccDiagnosticRecorder{}
	svc := newCCDiagnosticGateway(t, upstream, ccDiagnosticCaptureSettings, recorder)

	account := &Account{
		ID:          3,
		Name:        "opencode-go-anthropic",
		Platform:    PlatformOpenCodeGo,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":      "sk-opencode-go",
			"base_url":     "https://opencode.example/v1",
			"api_protocol": APIProtocolAnthropic,
		},
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.Error(t, err)
	require.Nil(t, result)

	retried, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.NotNil(t, retried)

	attempts := recorder.waitFor(t, 1)
	require.Len(t, attempts, 1)
	require.Equal(t, ErrorDiagnosticProtocolChatCompletions, attempts[0].Protocol,
		"出站是原生 Messages，诊断协议仍是客户端入口协议 chat_completions")
	require.Equal(t, http.StatusBadRequest, attempts[0].UpstreamStatusCode)
	require.Equal(t, upstream.sentBody(t, 0), attempts[0].Body)
}

// TestChatCompletionsErrorDiagnosticKeepsEveryFailedAttemptAcrossAccountSwitch 覆盖票 03 的
// 「账号切换不丢前一次失败」：同一逻辑请求换账号再失败时，两次失败各有独立诊断与序号，
// 后一次不会覆盖前一次。
func TestChatCompletionsErrorDiagnosticKeepsEveryFailedAttemptAcrossAccountSwitch(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"` + ccDiagnosticCanary + `"}],"stream":false}`)
	c, _ := newCCDiagnosticContext(t, body)

	upstream := newCCDiagnosticUpstream(t,
		[]int{http.StatusBadRequest, http.StatusInternalServerError},
		[]string{`{"error":{"message":"bad request"}}`, `{"error":{"message":"server error"}}`},
	)
	recorder := &ccDiagnosticRecorder{}
	svc := newCCDiagnosticGateway(t, upstream, ccDiagnosticCaptureSettings, recorder)

	first := ccDiagnosticOAuthAccount()
	second := ccDiagnosticOAuthAccount()
	second.ID = 2
	second.Name = "openai-oauth-2"

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, first, body, "", "")
	require.Error(t, err)
	_, err = svc.ForwardAsChatCompletions(context.Background(), c, second, body, "", "")
	require.Error(t, err)

	attempts := recorder.waitFor(t, 2)
	require.Len(t, attempts, 2, "每次真实失败尝试各有独立诊断，账号切换不覆盖前一次")
	require.Equal(t, []int{1, 2}, []int{attempts[0].AttemptIndex, attempts[1].AttemptIndex})
	require.Equal(t, []int{http.StatusBadRequest, http.StatusInternalServerError}, []int{
		attempts[0].UpstreamStatusCode, attempts[1].UpstreamStatusCode,
	})
	for i := range attempts {
		require.Equal(t, ErrorDiagnosticProtocolChatCompletions, attempts[i].Protocol)
		require.Equal(t, upstream.sentBody(t, i), attempts[i].Body, "第 %d 次尝试的字节", i+1)
	}
}

// TestChatCompletionsErrorDiagnosticCoversRawChatCompletionsSender 覆盖票 03 里唯一经共享 CC
// 发送点（sendCCUpstreamRequest）的 Chat Completions 分支：raw chat completions 直转。
// 该发送点同时服务 /v1/messages 与 /v1/responses 的 raw-chat 回退，所以协议由调用方显式传入，
// 不能从路径或 ctx 推断；本分支必须写成 chat_completions。
func TestChatCompletionsErrorDiagnosticCoversRawChatCompletionsSender(t *testing.T) {
	body := []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"` + ccDiagnosticCanary + `"}],"stream":false}`)
	c, _ := newCCDiagnosticContext(t, body)

	upstream := newCCDiagnosticUpstream(t,
		[]int{http.StatusBadRequest},
		[]string{`{"error":{"message":"bad request"}}`},
	)
	recorder := &ccDiagnosticRecorder{}
	svc := newCCDiagnosticGateway(t, upstream, ccDiagnosticCaptureSettings, recorder)
	svc.cfg = rawChatCompletionsTestConfig()

	account := adaptiveProtocolTestAccount(PlatformZhipu, map[string]any{
		APIProtocolChatCompletions: "http://chat.example",
	})

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.Error(t, err)

	attempts := recorder.waitFor(t, 1)
	require.Len(t, attempts, 1)
	require.Equal(t, ErrorDiagnosticProtocolChatCompletions, attempts[0].Protocol,
		"共享 CC 发送点的协议由调用方给出，raw CC 分支必须记 chat_completions")
	require.Equal(t, http.StatusBadRequest, attempts[0].UpstreamStatusCode)
	require.Equal(t, upstream.sentBody(t, 0), attempts[0].Body)
}

// TestChatCompletionsErrorDiagnosticCoversGrokChatResponsesBridge 覆盖票 03 的 Grok CC→Responses
// 桥接分支：入站是 /v1/chat/completions（Grok 分流选择 Responses 桥），出站是 Responses 形状，
// 诊断协议仍必须是 chat_completions。绑定在请求上，因此不会波及 compose-image 探测等辅助发送。
func TestChatCompletionsErrorDiagnosticCoversGrokChatResponsesBridge(t *testing.T) {
	body := []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"` + ccDiagnosticCanary + `"}],"stream":false,"prompt_cache_key":"cc-diag-grok-session"}`)
	c, _ := newCCDiagnosticContext(t, body)
	c.Set("api_key", &APIKey{ID: 7101})

	upstream := newCCDiagnosticUpstream(t,
		[]int{http.StatusBadRequest},
		[]string{`{"error":{"message":"bad request"}}`},
	)
	recorder := &ccDiagnosticRecorder{}
	svc := newCCDiagnosticGateway(t, upstream, ccDiagnosticCaptureSettings, recorder)

	account := grokChatBridgeTestAccount(71)
	repo := &grokQuotaAccountRepo{mockAccountRepoForPlatform: &mockAccountRepoForPlatform{
		accountsByID: map[int64]*Account{account.ID: account},
	}}
	svc.grokTokenProvider = NewGrokTokenProvider(repo, nil)
	svc.accountRepo = repo

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.Error(t, err)

	attempts := recorder.waitFor(t, 1)
	require.Len(t, attempts, 1, "Grok 桥接只由 CC 入站触发，失败尝试必须产生一条诊断")
	require.Equal(t, ErrorDiagnosticProtocolChatCompletions, attempts[0].Protocol,
		"出站是 Responses 形状，诊断协议仍是客户端入口协议 chat_completions")
	require.Equal(t, http.StatusBadRequest, attempts[0].UpstreamStatusCode)
	require.Equal(t, upstream.sentBody(t, 0), attempts[0].Body)
}

// TestChatCompletionsErrorDiagnosticCoversResponsesShapedCCIngress 覆盖票 03 的
// 「CC 入站 + Responses 形状 body → 原生 Anthropic」跨协议组合：路由入口仍是
// /v1/chat/completions，因此诊断协议是 chat_completions，不是 responses。
func TestChatCompletionsErrorDiagnosticCoversResponsesShapedCCIngress(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","input":"` + ccDiagnosticCanary + `","stream":false}`)
	c, _ := newCCDiagnosticContext(t, body)

	upstream := newCCDiagnosticUpstream(t,
		[]int{http.StatusBadRequest, http.StatusOK},
		[]string{`{"type":"error","error":{"message":"bad request"}}`, ccDiagnosticAnthropicSuccessSSE},
	)
	recorder := &ccDiagnosticRecorder{}
	svc := newCCDiagnosticGateway(t, upstream, ccDiagnosticCaptureSettings, recorder)

	account := &Account{
		ID:          5,
		Name:        "opencode-go-anthropic",
		Platform:    PlatformOpenCodeGo,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":      "sk-opencode-go",
			"base_url":     "https://opencode.example/v1",
			"api_protocol": APIProtocolAnthropic,
		},
	}

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.Error(t, err)

	attempts := recorder.waitFor(t, 1)
	require.Len(t, attempts, 1)
	require.Equal(t, ErrorDiagnosticProtocolChatCompletions, attempts[0].Protocol,
		"body 是 Responses 形状不代表客户端协议是 responses")
	require.Equal(t, http.StatusBadRequest, attempts[0].UpstreamStatusCode)
	require.Equal(t, upstream.sentBody(t, 0), attempts[0].Body)
}

// TestResponsesIngressErrorDiagnosticStaysResponsesOnSharedNativeAnthropicSend 是共享发送点的
// 回归保护：同一条原生 Anthropic 发送路径也服务真正的 /v1/responses 入站，那里的诊断协议
// 必须仍是 responses。若有人把该发送点的协议写死成 chat_completions，这条用例会失败。
func TestResponsesIngressErrorDiagnosticStaysResponsesOnSharedNativeAnthropicSend(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","input":"` + ccDiagnosticCanary + `","stream":false}`)
	c, _ := newCCDiagnosticContext(t, body)

	upstream := newCCDiagnosticUpstream(t,
		[]int{http.StatusBadRequest},
		[]string{`{"type":"error","error":{"message":"bad request"}}`},
	)
	recorder := &ccDiagnosticRecorder{}
	svc := newCCDiagnosticGateway(t, upstream, ccDiagnosticCaptureSettings, recorder)

	account := &Account{
		ID:          6,
		Name:        "opencode-go-anthropic-responses",
		Platform:    PlatformOpenCodeGo,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":      "sk-opencode-go",
			"base_url":     "https://opencode.example/v1",
			"api_protocol": APIProtocolAnthropic,
		},
	}

	_, err := svc.Forward(context.Background(), c, account, body)
	require.Error(t, err)

	attempts := recorder.waitFor(t, 1)
	require.Len(t, attempts, 1)
	require.Equal(t, ErrorDiagnosticProtocolResponses, attempts[0].Protocol,
		"/v1/responses 入站的诊断协议必须是 responses，不能因为共用发送点而漂移")
	require.Equal(t, http.StatusBadRequest, attempts[0].UpstreamStatusCode)
	require.Equal(t, upstream.sentBody(t, 0), attempts[0].Body)
}

// TestChatCompletionsErrorDiagnosticExcludesGrokComposeImageProbe 覆盖票 03 的辅助发送排除：
// Grok composer 分支会先发一次 compose-image 描述探测（它同样是"真实上游尝试"并已接入
// 请求审计），再发真正的对话请求。诊断只允许覆盖真正的对话请求——探测不能被记成一次
// Chat Completions 失败尝试。绑定放在请求上而不是 ctx 上，因此探测天然拿不到观察者。
func TestChatCompletionsErrorDiagnosticExcludesGrokComposeImageProbe(t *testing.T) {
	body := []byte(`{"model":"grok-composer-fast","messages":[{"role":"user","content":[{"type":"text","text":"` + ccDiagnosticCanary + `"},{"type":"image_url","image_url":{"url":"https://example.invalid/cc-diag.png"}}]}],"stream":false}`)
	c, _ := newCCDiagnosticContext(t, body)

	probeResponse := `{"id":"resp_probe","object":"response","status":"completed","model":"grok-4.5","output":[{"type":"message","id":"msg_probe","role":"assistant","status":"completed","content":[{"type":"output_text","text":"a screenshot of a terminal"}]}],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`
	upstream := newCCDiagnosticUpstream(t,
		[]int{http.StatusOK, http.StatusBadRequest},
		[]string{probeResponse, `{"error":{"message":"bad request"}}`},
	)
	recorder := &ccDiagnosticRecorder{}
	svc := newCCDiagnosticGateway(t, upstream, ccDiagnosticCaptureSettings, recorder)

	account := grokChatBridgeTestAccount(72)
	repo := &grokQuotaAccountRepo{mockAccountRepoForPlatform: &mockAccountRepoForPlatform{
		accountsByID: map[int64]*Account{account.ID: account},
	}}
	svc.grokTokenProvider = NewGrokTokenProvider(repo, nil)
	svc.accountRepo = repo

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.Error(t, err)
	require.Equal(t, 2, upstream.callCount(), "compose-image 探测与真正的对话请求各发送一次")

	probeBody := upstream.sentBody(t, 0)
	require.Contains(t, string(probeBody), "input_image", "第一次发送是辅助的图像描述探测")
	dialogueBody := upstream.sentBody(t, 1)

	attempts := recorder.waitFor(t, 1)
	require.Len(t, attempts, 1)
	require.Equal(t, dialogueBody, attempts[0].Body,
		"诊断只能对应真正的对话请求，不能是 compose-image 探测")
	require.NotEqual(t, probeBody, attempts[0].Body)
	require.Equal(t, ErrorDiagnosticProtocolChatCompletions, attempts[0].Protocol)
	require.Equal(t, http.StatusBadRequest, attempts[0].UpstreamStatusCode)

	// 探测没有被记成第二条：两次发送都已完成，诊断总数必须稳定为 1。
	require.Never(t, func() bool { return len(recorder.snapshot()) != 1 },
		200*time.Millisecond, 10*time.Millisecond, "辅助探测不得产生诊断")
}

// TestChatCompletionsErrorDiagnosticNeverInventsStatusForTransportFailure 覆盖票 03 的反例：
// 没有 HTTP 响应的连接故障不产生任何诊断，不得被伪造成上游 4xx/5xx。
func TestChatCompletionsErrorDiagnosticNeverInventsStatusForTransportFailure(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"` + ccDiagnosticCanary + `"}],"stream":false}`)
	c, _ := newCCDiagnosticContext(t, body)

	upstream := newCCDiagnosticUpstream(t, nil, nil)
	recorder := &ccDiagnosticRecorder{}
	svc := newCCDiagnosticGateway(t, upstream, ccDiagnosticCaptureSettings, recorder)

	upstream.failNextTransportError(errors.New("dial tcp 203.0.113.7:443: connect: connection refused"))

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, ccDiagnosticOAuthAccount(), body, "", "")
	require.Error(t, err)
	require.Empty(t, recorder.snapshot(),
		"连接故障没有上游 HTTP 状态，不得产生假的 4xx/5xx 诊断")
}

// TestChatCompletionsErrorDiagnosticIsExplicitOptIn 覆盖关闭态：未注入接缝或门控未开启时
// 不绑定观察者，请求上下文里不会多出任何诊断状态，也不产生任何诊断写入。
func TestChatCompletionsErrorDiagnosticIsExplicitOptIn(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"` + ccDiagnosticCanary + `"}],"stream":false}`)

	t.Run("no recorder injected", func(t *testing.T) {
		c, _ := newCCDiagnosticContext(t, body)
		upstream := newCCDiagnosticUpstream(t, []int{http.StatusBadRequest}, []string{`{"error":{"message":"bad"}}`})
		svc := &OpenAIGatewayService{httpUpstream: upstream}

		_, err := svc.ForwardAsChatCompletions(context.Background(), c, ccDiagnosticOAuthAccount(), body, "", "")
		require.Error(t, err)
		require.Nil(t, svc.errorDiagnostics)
	})

	t.Run("capture gate disabled", func(t *testing.T) {
		c, _ := newCCDiagnosticContext(t, body)
		upstream := newCCDiagnosticUpstream(t, []int{http.StatusBadRequest}, []string{`{"error":{"message":"bad"}}`})
		recorder := &ccDiagnosticRecorder{}
		svc := &OpenAIGatewayService{httpUpstream: upstream}
		svc.settingService = &SettingService{settingRepo: ccDiagnosticSettingRepo{value: `{"enabled":false,"risk_acknowledged":true,"body_retention_enabled":true}`}}
		svc.SetErrorDiagnosticRecorder(recorder)

		_, err := svc.ForwardAsChatCompletions(context.Background(), c, ccDiagnosticOAuthAccount(), body, "", "")
		require.Error(t, err)
		require.Empty(t, recorder.snapshot(), "采集关闭时不得产生诊断")
	})
}

// TestChatCompletionsErrorDiagnosticKeepsExactBytesWhenBodyCaptureDisabled 覆盖票 02 的分阶段
// opt-in：只开元数据时仍然逐次记录状态与尝试序号，但不留任何出站字节。
func TestChatCompletionsErrorDiagnosticKeepsExactBytesWhenBodyCaptureDisabled(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"` + ccDiagnosticCanary + `"}],"stream":false}`)
	c, _ := newCCDiagnosticContext(t, body)

	upstream := newCCDiagnosticUpstream(t, []int{http.StatusBadRequest}, []string{`{"error":{"message":"bad"}}`})
	recorder := &ccDiagnosticRecorder{}
	svc := newCCDiagnosticGateway(t, upstream, ccDiagnosticMetadataSetting, recorder)

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, ccDiagnosticOAuthAccount(), body, "", "")
	require.Error(t, err)

	attempts := recorder.waitFor(t, 1)
	require.Len(t, attempts, 1)
	require.Equal(t, ErrorDiagnosticProtocolChatCompletions, attempts[0].Protocol)
	require.Empty(t, attempts[0].Body, "未开启正文留存时不带出站字节")
	require.False(t, attempts[0].BodyReadComplete)
}
