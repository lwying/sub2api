//go:build unit

package service

// Responses 分支的上游错误诊断绑定测试（契约见
// docs/adr/0005-short-lived-error-body-diagnostics-scope.md）。
//
// 发送替身刻意复刻 repository/http_upstream.go 的真实接缝顺序：
// BeforeRequest → StartRequestAttempt → NewDiagnosticBodyCapture → CountingReadCloser →
// SetResponse → ObserveUpstreamError。因此「上游实际收到的字节」就是诊断观察者看到并交给
// Responses 分支绑定的字节：正文逐字节比对针对这些字节，而不是入站 JSON，也不是最后一次
// 重试的正文。绑定必须发生在覆盖分支内部（ctx-local），不能落在 gin 请求上下文上，否则
// 同一进程里的 WS／插件／辅助请求会被误采集。

import (
	"bytes"
	"context"
	"encoding/json"
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

// errorDiagnosticBranchSettingRepo 只提供错误诊断门控所需的读取能力。
type errorDiagnosticBranchSettingRepo struct{ values map[string]string }

func (r *errorDiagnosticBranchSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", ErrSettingNotFound
}

func (r *errorDiagnosticBranchSettingRepo) Get(context.Context, string) (*Setting, error) {
	return nil, ErrSettingNotFound
}

func (r *errorDiagnosticBranchSettingRepo) Set(context.Context, string, string) error { return nil }

func (r *errorDiagnosticBranchSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return r.values, nil
}

func (r *errorDiagnosticBranchSettingRepo) SetMultiple(context.Context, map[string]string) error {
	return nil
}

func (r *errorDiagnosticBranchSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return r.values, nil
}

func (r *errorDiagnosticBranchSettingRepo) Delete(context.Context, string) error { return nil }

// errorDiagnosticBranchSettingService 构造带稳定密钥的门控读取链。
//
// 接缝只在「门控允许 + 稳定密钥可用」时才 tee 出站正文，因此断言正文的用例必须让夹具
// 带上密钥，否则会静默退化成未采正文（元数据采集不受影响）。
func errorDiagnosticBranchSettingService(repo SettingRepository) *SettingService {
	return NewSettingService(repo, &config.Config{Totp: config.TotpConfig{
		EncryptionKey:           errorDiagnosticStableEncryptionKey,
		EncryptionKeyConfigured: true,
	}})
}

// errorDiagnosticUpstreamCall 记录一次真实发送：出站请求是否携带观察者，以及传输层实际
// 从请求体读走的字节。
type errorDiagnosticUpstreamCall struct {
	observerBound bool
	body          []byte
}

// errorDiagnosticResponsesUpstream 是复刻真实传输接缝的 HTTPUpstream 替身。
type errorDiagnosticResponsesUpstream struct {
	responses []*http.Response
	errs      []error
	// observeSeam=false 模拟不经过该接缝的发送（插件／WS 桥）：网关能看到 4xx/5xx，
	// 但传输层不产生任何观察，因此不得据此伪造上游 HTTP 失败。
	observeSeam bool
	// bodyReadLimit>0 时只读出站正文的前若干字节就返回响应，模拟「没读完正文」的发送。
	bodyReadLimit int

	mu    sync.Mutex
	calls []errorDiagnosticUpstreamCall
}

func newErrorDiagnosticResponsesUpstream(responses ...*http.Response) *errorDiagnosticResponsesUpstream {
	return &errorDiagnosticResponsesUpstream{responses: responses, observeSeam: true}
}

func newBypassingErrorDiagnosticResponsesUpstream(responses ...*http.Response) *errorDiagnosticResponsesUpstream {
	return &errorDiagnosticResponsesUpstream{responses: responses}
}

func newTruncatingErrorDiagnosticResponsesUpstream(bodyReadLimit int, responses ...*http.Response) *errorDiagnosticResponsesUpstream {
	return &errorDiagnosticResponsesUpstream{responses: responses, observeSeam: true, bodyReadLimit: bodyReadLimit}
}

func (u *errorDiagnosticResponsesUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.send(req)
}

func (u *errorDiagnosticResponsesUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.send(req)
}

func (u *errorDiagnosticResponsesUpstream) send(req *http.Request) (*http.Response, error) {
	u.mu.Lock()
	index := len(u.calls)
	u.mu.Unlock()

	observerBound := false
	if req != nil {
		_, observerBound = httpattempt.DiagnosticObserverFromContext(req.Context())
	}

	var (
		attempt *httpattempt.Attempt
		capture *httpattempt.DiagnosticBodyCapture
	)
	if u.observeSeam && req != nil {
		if err := httpattempt.BeforeRequest(req); err != nil {
			return nil, err
		}
		attempt = httpattempt.StartRequestAttempt(req)
		capture = httpattempt.NewDiagnosticBodyCapture(req)
		defer capture.Release()
	}

	var consumed []byte
	if req != nil && req.Body != nil && req.Body != http.NoBody {
		reader := io.ReadCloser(req.Body)
		if attempt != nil || capture != nil {
			reader = &httpattempt.CountingReadCloser{
				ReadCloser: req.Body,
				OnRead:     attempt.AddRequestBytes,
				Capture:    capture,
			}
		}
		if u.bodyReadLimit > 0 {
			// 只读一部分：模拟传输层在读完出站正文之前就拿到响应（例如被代理提前拒绝），
			// 观察者对本次出站正文的结论因此是 incomplete。
			buf := make([]byte, u.bodyReadLimit)
			n, readErr := io.ReadFull(reader, buf)
			if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
				return nil, readErr
			}
			consumed = buf[:n]
		} else {
			body, err := io.ReadAll(reader)
			if err != nil {
				return nil, err
			}
			consumed = body
		}
	}

	var resp *http.Response
	if index < len(u.responses) {
		resp = u.responses[index]
	}
	var err error
	if index < len(u.errs) {
		err = u.errs[index]
	}
	if resp == nil && err == nil {
		return nil, errors.New("unexpected upstream call")
	}
	if resp != nil && u.observeSeam && attempt != nil {
		attempt.SetResponse(resp.StatusCode, resp.Header, resp.Body != nil && resp.Body != http.NoBody)
		httpattempt.ObserveUpstreamError(req, attempt, resp, capture)
	}

	u.mu.Lock()
	u.calls = append(u.calls, errorDiagnosticUpstreamCall{observerBound: observerBound, body: bytes.Clone(consumed)})
	u.mu.Unlock()
	return resp, err
}

func (u *errorDiagnosticResponsesUpstream) sentBodies() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([][]byte, 0, len(u.calls))
	for _, call := range u.calls {
		out = append(out, bytes.Clone(call.body))
	}
	return out
}

func (u *errorDiagnosticResponsesUpstream) boundObservers() []bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]bool, 0, len(u.calls))
	for _, call := range u.calls {
		out = append(out, call.observerBound)
	}
	return out
}

func (u *errorDiagnosticResponsesUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

// errorDiagnosticBranchRecorder 把接缝的异步写入变成可等待的事实。
type errorDiagnosticBranchRecorder struct {
	service *ErrorDiagnosticService
	done    chan struct{}

	mu       sync.Mutex
	attempts []ErrorDiagnosticAttempt
	records  []ErrorDiagnosticRecord
}

func newErrorDiagnosticBranchRecorder(service *ErrorDiagnosticService) *errorDiagnosticBranchRecorder {
	return &errorDiagnosticBranchRecorder{service: service, done: make(chan struct{}, 32)}
}

func (r *errorDiagnosticBranchRecorder) RecordErrorDiagnostic(ctx context.Context, attempt ErrorDiagnosticAttempt) (ErrorDiagnosticRecord, error) {
	record, err := r.service.RecordErrorDiagnostic(ctx, attempt)
	r.mu.Lock()
	r.attempts = append(r.attempts, attempt)
	if err == nil {
		r.records = append(r.records, record)
	}
	r.mu.Unlock()
	select {
	case r.done <- struct{}{}:
	default:
	}
	return record, err
}

func (r *errorDiagnosticBranchRecorder) attemptsSnapshot() []ErrorDiagnosticAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ErrorDiagnosticAttempt(nil), r.attempts...)
}

func (r *errorDiagnosticBranchRecorder) recordsSnapshot() []ErrorDiagnosticRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ErrorDiagnosticRecord(nil), r.records...)
}

func (r *errorDiagnosticBranchRecorder) waitForAttempts(t *testing.T, want int) []ErrorDiagnosticAttempt {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		if attempts := r.attemptsSnapshot(); len(attempts) >= want {
			return attempts
		}
		select {
		case <-r.done:
		case <-deadline.C:
			t.Fatalf("expected at least %d diagnostic attempt(s), got %d", want, len(r.attemptsSnapshot()))
		}
	}
}

func (r *errorDiagnosticBranchRecorder) requireNoAttempts(t *testing.T) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	require.Empty(t, r.attemptsSnapshot(), "uncovered branch must not record upstream error diagnostics")
}

// requireNoAttemptsAfter 断言安静期内没有新增诊断（既有条目不增加）。
func (r *errorDiagnosticBranchRecorder) requireNoAttemptsAfter(t *testing.T, existing int) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	require.Len(t, r.attemptsSnapshot(), existing, "auxiliary or uncovered sends must not add diagnostics")
}

type errorDiagnosticResponsesBranchHarness struct {
	gateway  *GatewayService
	service  *ErrorDiagnosticService
	repo     *errorDiagnosticRepoFake
	cipher   *errorDiagnosticCipherFake
	recorder *errorDiagnosticBranchRecorder
	upstream *errorDiagnosticResponsesUpstream
	c        *gin.Context
	out      *httptest.ResponseRecorder
}

func newErrorDiagnosticResponsesBranchHarness(
	t *testing.T,
	gatewaySettings ErrorDiagnosticSettings,
	serviceSettings ErrorDiagnosticSettings,
	upstream *errorDiagnosticResponsesUpstream,
) *errorDiagnosticResponsesBranchHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	repo := newErrorDiagnosticRepoFake()
	cipher := &errorDiagnosticCipherFake{version: 1}
	service := newTestErrorDiagnosticService(t, repo, serviceSettings, cipher)
	recorder := newErrorDiagnosticBranchRecorder(service)

	raw, err := json.Marshal(gatewaySettings)
	require.NoError(t, err)
	// 门控真正打开需要「存量布尔值 + 当前版本的有效书面确认」
	// （见 ApplyErrorDiagnosticRiskAcknowledgement），因此夹具同时给出两个键。
	settingRepo := &errorDiagnosticBranchSettingRepo{values: map[string]string{
		SettingKeyErrorDiagnostic:                    string(raw),
		SettingKeyErrorDiagnosticRiskAcknowledgement: errorDiagnosticAckJSON(ErrorDiagnosticRiskAcknowledgementVersion),
	}}

	gateway := &GatewayService{
		cfg:                 &config.Config{},
		httpUpstream:        upstream,
		tlsFPProfileService: &TLSFingerprintProfileService{},
		settingService:      errorDiagnosticBranchSettingService(settingRepo),
	}
	gateway.SetErrorDiagnosticRecorder(recorder)

	out := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(out)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	return &errorDiagnosticResponsesBranchHarness{
		gateway:  gateway,
		service:  service,
		repo:     repo,
		cipher:   cipher,
		recorder: recorder,
		upstream: upstream,
		c:        c,
		out:      out,
	}
}

func (h *errorDiagnosticResponsesBranchHarness) responsesAccount() *Account {
	return &Account{
		ID:       7,
		Name:     "responses-diagnostic-account",
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "sk-diagnostic-test",
		},
	}
}

func (h *errorDiagnosticResponsesBranchHarness) forward(t *testing.T, body []byte) (*ForwardResult, error) {
	t.Helper()
	return h.gateway.ForwardAsResponses(context.Background(), h.c, h.responsesAccount(), body, nil)
}

func errorDiagnosticResponsesFailure(status int, requestID string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"x-request-id": []string{requestID}},
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"api_error","message":"upstream failed"}}`)),
	}
}

func errorDiagnosticResponsesSuccess(requestID string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"x-request-id": []string{requestID}},
		Body:       io.NopCloser(strings.NewReader(toolAnthropicSSEStream())),
	}
}

// TestResponsesBranchErrorDiagnostic_RecordsFailedAttemptBytesThenSucceeds 固定票据 04 的
// 核心事实：同一个逻辑请求先收到 5xx、随后重试成功时，管理员能查到那次失败尝试的协议、
// 状态、序号与「真实发往上游」的字节，且成功那次不产生任何诊断。
func TestResponsesBranchErrorDiagnostic_RecordsFailedAttemptBytesThenSucceeds(t *testing.T) {
	upstream := newErrorDiagnosticResponsesUpstream(
		errorDiagnosticResponsesFailure(http.StatusInternalServerError, "resp-fail"),
		errorDiagnosticResponsesSuccess("resp-ok"),
	)
	h := newErrorDiagnosticResponsesBranchHarness(t, enabledErrorDiagnostics(), enabledErrorDiagnostics(), upstream)

	body := []byte(`{"model":"claude-sonnet-4-5","input":"diagnose this failure"}`)

	_, err := h.forward(t, body)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, http.StatusInternalServerError, failover.StatusCode)

	result, err := h.forward(t, body)
	require.NoError(t, err)
	require.NotNil(t, result)

	attempts := h.recorder.waitForAttempts(t, 1)
	require.Len(t, attempts, 1, "only the real 5xx send may produce a diagnostic")

	attempt := attempts[0]
	require.Equal(t, ErrorDiagnosticProtocolResponses, attempt.Protocol,
		"the diagnostic protocol is the inbound branch, not the converted wire protocol")
	require.Equal(t, ErrorDiagnosticStageWire, attempt.Stage)
	require.Equal(t, http.StatusInternalServerError, attempt.UpstreamStatusCode)
	require.Equal(t, 1, attempt.AttemptIndex)
	require.True(t, attempt.BodyReadComplete)
	require.Equal(t, ErrorDiagnosticBodyVerdictComplete, attempt.BodyVerdict)
	require.Zero(t, attempt.UsageLogID, "phase 1 keeps diagnostics independent of usage rows")

	records := h.recorder.recordsSnapshot()
	require.Len(t, records, 1)
	require.False(t, records[0].HasUsage)
	require.Equal(t, ErrorDiagnosticBodyStateStored, records[0].BodyState)
	require.Equal(t, ErrorDiagnosticBodyRetained, records[0].BodyReason)

	stored, err := h.service.ReadErrorDiagnosticBody(context.Background(), records[0].ID)
	require.NoError(t, err)
	require.Equal(t, 2, upstream.callCount(), "audit keeps both real sends")
	sent := upstream.sentBodies()
	require.Equal(t, string(sent[0]), string(stored), "diagnostic body must be the bytes the transport consumed")
	require.NotEqual(t, string(body), string(stored), "diagnostic body must not be the inbound client body")
	require.Contains(t, string(stored), "diagnose this failure", "converted upstream body keeps the client input")
	require.Equal(t, uint64(2), RequestAuditHTTPAttemptCount(h.c), "usage-owned request audit stays unchanged")

	_, leaked := httpattempt.DiagnosticObserverFromContext(h.c.Request.Context())
	require.False(t, leaked, "the observer must stay on the branch context, never on the gin request context")
}

// TestResponsesBranchErrorDiagnostic_KeepsEveryFailedAttemptBeforeSuccess 固定「转换／切换
// 账号／内部 fallback 不得覆盖前次失败」：两次失败的正文各自对应、顺序与序号递增，随后成功
// 不影响已记录的失败。
func TestResponsesBranchErrorDiagnostic_KeepsEveryFailedAttemptBeforeSuccess(t *testing.T) {
	upstream := newErrorDiagnosticResponsesUpstream(
		errorDiagnosticResponsesFailure(http.StatusInternalServerError, "resp-fail-1"),
		errorDiagnosticResponsesFailure(http.StatusBadGateway, "resp-fail-2"),
		errorDiagnosticResponsesSuccess("resp-ok"),
	)
	h := newErrorDiagnosticResponsesBranchHarness(t, enabledErrorDiagnostics(), enabledErrorDiagnostics(), upstream)

	firstBody := []byte(`{"model":"claude-sonnet-4-5","input":"first failing attempt"}`)
	secondBody := []byte(`{"model":"claude-sonnet-4-5","input":"second failing attempt"}`)

	_, err := h.forward(t, firstBody)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, http.StatusInternalServerError, failover.StatusCode)

	_, err = h.forward(t, secondBody)
	require.ErrorAs(t, err, &failover)
	require.Equal(t, http.StatusBadGateway, failover.StatusCode)

	result, err := h.forward(t, firstBody)
	require.NoError(t, err)
	require.NotNil(t, result)

	attempts := h.recorder.waitForAttempts(t, 2)
	require.Len(t, attempts, 2, "the successful retry must not add a diagnostic")

	byIndex := map[int]ErrorDiagnosticAttempt{}
	for _, attempt := range attempts {
		require.Equal(t, ErrorDiagnosticProtocolResponses, attempt.Protocol)
		require.Equal(t, ErrorDiagnosticBodyVerdictComplete, attempt.BodyVerdict)
		byIndex[attempt.AttemptIndex] = attempt
	}
	require.Equal(t, http.StatusInternalServerError, byIndex[1].UpstreamStatusCode)
	require.Equal(t, http.StatusBadGateway, byIndex[2].UpstreamStatusCode)

	records := h.recorder.recordsSnapshot()
	require.Len(t, records, 2)
	sent := upstream.sentBodies()
	require.Len(t, sent, 3)
	require.NotEqual(t, string(sent[0]), string(sent[1]), "each attempt sends its own converted body")

	storedByIndex := map[int]string{}
	for _, record := range records {
		stored, readErr := h.service.ReadErrorDiagnosticBody(context.Background(), record.ID)
		require.NoError(t, readErr)
		storedByIndex[record.AttemptIndex] = string(stored)
	}
	require.Equal(t, string(sent[0]), storedByIndex[1], "first failure keeps its own outbound bytes")
	require.Equal(t, string(sent[1]), storedByIndex[2], "second failure keeps its own outbound bytes")
	require.Contains(t, storedByIndex[1], "first failing attempt")
	require.Contains(t, storedByIndex[2], "second failing attempt")
}

// TestResponsesBranchErrorDiagnostic_ForwardBranchKeepsResponsesProtocolThroughConversion
// 固定 OpenAIGatewayService.Forward 这个入站 /v1/responses 入口：上游是 Anthropic 协议账号时
// 请求被转换成 Anthropic 形态，但诊断协议仍是入站的 responses（不随着 wire 协议漂移），
// 并且管理员取到的正文就是该次转换后真实发出的字节。
func TestResponsesBranchErrorDiagnostic_ForwardBranchKeepsResponsesProtocolThroughConversion(t *testing.T) {
	upstream := newErrorDiagnosticResponsesUpstream(
		errorDiagnosticResponsesFailure(http.StatusInternalServerError, "forward-fail"),
	)
	gateway, recorder, service := newErrorDiagnosticOpenAIGatewayHarness(t, enabledErrorDiagnostics(), upstream)

	gin.SetMode(gin.TestMode)
	out := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(out)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)

	account := &Account{
		ID:       41,
		Name:     "cn-anthropic-protocol-account",
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "kimi-diagnostic-test",
			"base_url":     "https://api.moonshot.test",
			"api_protocol": APIProtocolAnthropic,
		},
	}
	body := []byte(`{"model":"kimi-k2","input":"forward branch conversion"}`)

	_, err := gateway.Forward(context.Background(), c, account, body)
	require.Error(t, err)

	attempts := recorder.waitForAttempts(t, 1)
	require.Len(t, attempts, 1)
	require.Equal(t, ErrorDiagnosticProtocolResponses, attempts[0].Protocol,
		"inbound /v1/responses must stay responses even when converted to Anthropic on the wire")
	require.Equal(t, http.StatusInternalServerError, attempts[0].UpstreamStatusCode)
	require.Equal(t, 1, attempts[0].AttemptIndex)
	require.Equal(t, ErrorDiagnosticBodyVerdictComplete, attempts[0].BodyVerdict)
	require.True(t, upstream.boundObservers()[0], "Forward must bind the observer on the Responses branch sends")

	records := recorder.recordsSnapshot()
	require.Len(t, records, 1)
	stored, readErr := service.ReadErrorDiagnosticBody(context.Background(), records[0].ID)
	require.NoError(t, readErr)
	require.Equal(t, string(upstream.sentBodies()[0]), string(stored))
	require.Contains(t, string(stored), "forward branch conversion")
	require.NotEqual(t, string(body), string(stored), "the converted wire body is not the inbound body")
}

// TestResponsesBranchErrorDiagnostic_RawChatFallbackKeepsResponsesProtocol 固定 /v1/responses
// 回退到 Chat Completions 上游形态时仍按入站路由记 responses：这条发送走共享的 CC 发送点，
// 协议由本分支的调用方显式给出，不跟着 wire 形态漂移。
func TestResponsesBranchErrorDiagnostic_RawChatFallbackKeepsResponsesProtocol(t *testing.T) {
	upstream := newErrorDiagnosticResponsesUpstream(
		errorDiagnosticResponsesFailure(http.StatusInternalServerError, "cc-fallback-fail"),
	)
	gateway, recorder, service := newErrorDiagnosticOpenAIGatewayHarness(t, enabledErrorDiagnostics(), upstream)

	gin.SetMode(gin.TestMode)
	out := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(out)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)

	account := &Account{
		ID:       51,
		Name:     "cn-chat-completions-account",
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "kimi-cc-test",
			"base_url": "https://api.moonshot.test",
		},
	}
	body := []byte(`{"model":"kimi-k2","input":"raw chat fallback"}`)

	_, err := gateway.forwardResponsesViaRawChatCompletions(context.Background(), c, account, body)
	require.Error(t, err)

	attempts := recorder.waitForAttempts(t, 1)
	require.Len(t, attempts, 1)
	require.Equal(t, ErrorDiagnosticProtocolResponses, attempts[0].Protocol,
		"the inbound route stays responses even when the wire form is Chat Completions")
	require.Equal(t, http.StatusInternalServerError, attempts[0].UpstreamStatusCode)
	require.Equal(t, ErrorDiagnosticBodyVerdictComplete, attempts[0].BodyVerdict)
	require.True(t, upstream.boundObservers()[0], "the shared CC send must carry the caller's protocol")

	records := recorder.recordsSnapshot()
	require.Len(t, records, 1)
	stored, readErr := service.ReadErrorDiagnosticBody(context.Background(), records[0].ID)
	require.NoError(t, readErr)
	require.Equal(t, string(upstream.sentBodies()[0]), string(stored))
	require.Contains(t, string(stored), "raw chat fallback")
}

// TestResponsesBranchErrorDiagnostic_GrokTurnIsRecordedButImageProbeIsNot 固定 Grok 子分支的
// 边界：/v1/responses 的 Grok 对话发送按「一次真实发送」绑定并记 responses；而 Grok composer
// 的图像描述探测是辅助发送（只从 Chat 入站的 raw 回退进入），它自己不绑定，调用方也不为
// Grok 分支套整段上下文，因此不会被记成上游 Responses 失败。
func TestResponsesBranchErrorDiagnostic_GrokTurnIsRecordedButImageProbeIsNot(t *testing.T) {
	upstream := newErrorDiagnosticResponsesUpstream(
		errorDiagnosticResponsesFailure(http.StatusInternalServerError, "grok-turn-fail"),
		errorDiagnosticResponsesFailure(http.StatusInternalServerError, "grok-image-probe-fail"),
	)
	gateway, recorder, service := newErrorDiagnosticOpenAIGatewayHarness(t, enabledErrorDiagnostics(), upstream)

	gin.SetMode(gin.TestMode)
	out := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(out)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)

	account := &Account{
		ID:       31,
		Name:     "grok-responses-account",
		Platform: PlatformGrok,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "xai-diagnostic-test",
		},
	}

	// 1) 对话发送：真实 5xx 必须留下一条 responses 诊断，正文逐字节等于传输层读到的字节。
	_, _ = gateway.Forward(context.Background(), c, account, []byte(`{"model":"grok-4","input":"grok turn"}`))
	require.Equal(t, 1, upstream.callCount(), "the grok turn must really reach the upstream")

	attempts := recorder.waitForAttempts(t, 1)
	require.Len(t, attempts, 1)
	require.Equal(t, ErrorDiagnosticProtocolResponses, attempts[0].Protocol)
	require.Equal(t, http.StatusInternalServerError, attempts[0].UpstreamStatusCode)
	require.Equal(t, 1, attempts[0].AttemptIndex)
	require.Equal(t, ErrorDiagnosticBodyVerdictComplete, attempts[0].BodyVerdict)
	require.True(t, upstream.boundObservers()[0], "the grok turn send must carry the observer")

	records := recorder.recordsSnapshot()
	require.Len(t, records, 1)
	stored, err := service.ReadErrorDiagnosticBody(context.Background(), records[0].ID)
	require.NoError(t, err)
	require.Equal(t, string(upstream.sentBodies()[0]), string(stored))

	// 2) 辅助探测：即使由同一条请求的 gin 上下文驱动，探测自身也不绑定观察者，
	// 它的 5xx 不产生任何诊断（也不得被当成 Responses 失败）。
	_, _, probeErr := gateway.describeGrokComposerImage(context.Background(), c, account, "xai-token", "https://img.test/a.png", 1)
	require.Error(t, probeErr)
	require.Equal(t, 2, upstream.callCount())
	require.False(t, upstream.boundObservers()[1], "the auxiliary image probe must never carry the observer")
	recorder.requireNoAttemptsAfter(t, 1)
}

// TestResponsesBranchErrorDiagnostic_TransportFailureIsNotRecorded 固定「没有真实 HTTP 响应
// 的连接故障不得被伪造成上游 4xx/5xx 诊断」。
func TestResponsesBranchErrorDiagnostic_TransportFailureIsNotRecorded(t *testing.T) {
	upstream := newErrorDiagnosticResponsesUpstream()
	upstream.errs = []error{errors.New("dial tcp 127.0.0.1:443: connect: connection refused")}
	h := newErrorDiagnosticResponsesBranchHarness(t, enabledErrorDiagnostics(), enabledErrorDiagnostics(), upstream)

	_, err := h.forward(t, []byte(`{"model":"claude-sonnet-4-5","input":"transport failure"}`))
	require.Error(t, err)

	h.recorder.requireNoAttempts(t)
	require.Equal(t, 1, upstream.callCount())
}

// TestResponsesBranchErrorDiagnostic_UnobservedSendIsNotFabricated 固定「不经过真实传输接缝
// 的发送（插件／WS 桥形态）不能被当成上游 HTTP 失败」：网关自己看到 4xx 也不算事实。
func TestResponsesBranchErrorDiagnostic_UnobservedSendIsNotFabricated(t *testing.T) {
	upstream := newBypassingErrorDiagnosticResponsesUpstream(
		errorDiagnosticResponsesFailure(http.StatusBadRequest, "resp-bypass-1"),
		errorDiagnosticResponsesFailure(http.StatusInternalServerError, "resp-bypass-2"),
	)
	h := newErrorDiagnosticResponsesBranchHarness(t, enabledErrorDiagnostics(), enabledErrorDiagnostics(), upstream)

	_, _ = h.forward(t, []byte(`{"model":"claude-sonnet-4-5","input":"bypassing transport"}`))
	_, _ = h.forward(t, []byte(`{"model":"claude-sonnet-4-5","input":"bypassing transport"}`))

	h.recorder.requireNoAttempts(t)
	require.Equal(t, 2, upstream.callCount())
}

// TestResponsesBranchErrorDiagnostic_DisabledGateBindsNothing 固定门控 fail-closed：未启用时
// 既不写诊断，也不在出站请求上留下观察者。
func TestResponsesBranchErrorDiagnostic_DisabledGateBindsNothing(t *testing.T) {
	disabled := ErrorDiagnosticSettings{}
	upstream := newErrorDiagnosticResponsesUpstream(
		errorDiagnosticResponsesFailure(http.StatusInternalServerError, "resp-disabled"),
	)
	h := newErrorDiagnosticResponsesBranchHarness(t, disabled, disabled, upstream)

	_, err := h.forward(t, []byte(`{"model":"claude-sonnet-4-5","input":"disabled gate"}`))
	require.Error(t, err)

	h.recorder.requireNoAttempts(t)
	for i, bound := range upstream.boundObservers() {
		require.False(t, bound, "attempt %d must not carry an observer while the gate is closed", i)
	}
}

// TestResponsesBranchErrorDiagnostic_MetadataOnlyKeepsBytesOutOfStorage 固定正文留存是独立的
// 分阶段开关：只开元数据时，状态与序号仍然可见，但正文一律不留存。
func TestResponsesBranchErrorDiagnostic_MetadataOnlyKeepsBytesOutOfStorage(t *testing.T) {
	metadataOnly := ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true}
	upstream := newErrorDiagnosticResponsesUpstream(
		errorDiagnosticResponsesFailure(http.StatusInternalServerError, "resp-metadata-only"),
	)
	h := newErrorDiagnosticResponsesBranchHarness(t, metadataOnly, metadataOnly, upstream)

	_, err := h.forward(t, []byte(`{"model":"claude-sonnet-4-5","input":"metadata only"}`))
	require.Error(t, err)

	attempts := h.recorder.waitForAttempts(t, 1)
	require.Len(t, attempts, 1)
	require.Equal(t, http.StatusInternalServerError, attempts[0].UpstreamStatusCode)
	require.Equal(t, ErrorDiagnosticBodyVerdictNotRequested, attempts[0].BodyVerdict,
		"body capture was not opted in, so the verdict must not claim an observation")
	require.False(t, attempts[0].BodyReadComplete)

	records := h.recorder.recordsSnapshot()
	require.Len(t, records, 1)
	require.NotEqual(t, ErrorDiagnosticBodyStateStored, records[0].BodyState)
	require.Empty(t, h.repo.body, "no ciphertext may be stored when body retention is off")
}

// TestResponsesBranchErrorDiagnostic_OversizeBodyKeepsOnlySafeReason 固定「超限正文不截断冒充
// 完整」：传输层扣住 >1 MiB 的出站正文，管理员只看到安全原因码，且不能声称读取完整。
func TestResponsesBranchErrorDiagnostic_OversizeBodyKeepsOnlySafeReason(t *testing.T) {
	oversize := strings.Repeat("x", ErrorDiagnosticMaxBodyBytes+512)
	upstream := newErrorDiagnosticResponsesUpstream(
		errorDiagnosticResponsesFailure(http.StatusInternalServerError, "resp-too-large"),
	)
	h := newErrorDiagnosticResponsesBranchHarness(t, enabledErrorDiagnostics(), enabledErrorDiagnostics(), upstream)

	_, err := h.forward(t, []byte(`{"model":"claude-sonnet-4-5","input":"`+oversize+`"}`))
	require.Error(t, err)

	attempts := h.recorder.waitForAttempts(t, 1)
	require.Len(t, attempts, 1)
	require.Equal(t, ErrorDiagnosticBodyVerdictTooLarge, attempts[0].BodyVerdict)
	require.False(t, attempts[0].BodyReadComplete, "withheld bytes must never claim a complete read")
	require.Empty(t, attempts[0].Body, "the transport withholds oversize bytes")

	records := h.recorder.recordsSnapshot()
	require.Len(t, records, 1)
	require.Equal(t, ErrorDiagnosticBodyStateSkipped, records[0].BodyState)
	require.Equal(t, ErrorDiagnosticBodySkippedTooLarge, records[0].BodyReason)
	require.Empty(t, h.repo.body, "an oversize body must never be stored, not even truncated")
}

// TestResponsesBranchErrorDiagnostic_IncompleteReadKeepsOnlySafeReason 固定「传输层没把出站
// 正文读完」的发送：只留安全原因码，绝不把读到的片段当作完整出站正文。
func TestResponsesBranchErrorDiagnostic_IncompleteReadKeepsOnlySafeReason(t *testing.T) {
	upstream := newTruncatingErrorDiagnosticResponsesUpstream(
		16,
		errorDiagnosticResponsesFailure(http.StatusInternalServerError, "resp-incomplete"),
	)
	h := newErrorDiagnosticResponsesBranchHarness(t, enabledErrorDiagnostics(), enabledErrorDiagnostics(), upstream)

	_, err := h.forward(t, []byte(`{"model":"claude-sonnet-4-5","input":"incomplete read attempt"}`))
	require.Error(t, err)

	attempts := h.recorder.waitForAttempts(t, 1)
	require.Len(t, attempts, 1)
	require.Equal(t, ErrorDiagnosticBodyVerdictIncomplete, attempts[0].BodyVerdict)
	require.False(t, attempts[0].BodyReadComplete, "a partial read must never claim complete")
	require.Empty(t, attempts[0].Body)

	records := h.recorder.recordsSnapshot()
	require.Len(t, records, 1)
	require.Equal(t, ErrorDiagnosticBodyStateSkipped, records[0].BodyState)
	require.Equal(t, ErrorDiagnosticBodySkippedIncompleteRead, records[0].BodyReason)
	require.Empty(t, h.repo.body, "an incomplete body must never be stored as the outbound body")
}

// newErrorDiagnosticOpenAIGatewayHarness 构造只注入诊断接缝与发送替身的 OpenAI 网关。
func newErrorDiagnosticOpenAIGatewayHarness(
	t *testing.T,
	settings ErrorDiagnosticSettings,
	upstream *errorDiagnosticResponsesUpstream,
) (*OpenAIGatewayService, *errorDiagnosticBranchRecorder, *ErrorDiagnosticService) {
	t.Helper()
	repo := newErrorDiagnosticRepoFake()
	cipher := &errorDiagnosticCipherFake{version: 1}
	service := newTestErrorDiagnosticService(t, repo, settings, cipher)
	recorder := newErrorDiagnosticBranchRecorder(service)

	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	// 同 newErrorDiagnosticResponsesBranchHarness：打开门控需要当前版本的书面确认。
	settingService := errorDiagnosticBranchSettingService(&errorDiagnosticBranchSettingRepo{values: map[string]string{
		SettingKeyErrorDiagnostic:                    string(raw),
		SettingKeyErrorDiagnosticRiskAcknowledgement: errorDiagnosticAckJSON(ErrorDiagnosticRiskAcknowledgementVersion),
	}})

	gateway := &OpenAIGatewayService{
		cfg:              &config.Config{},
		httpUpstream:     upstream,
		settingService:   settingService,
		openaiWSResolver: NewOpenAIWSProtocolResolver(&config.Config{}),
	}
	gateway.SetErrorDiagnosticRecorder(recorder)
	return gateway, recorder, service
}
