//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 本文件固定 Messages 分支错误诊断接缝的行为（票据 01／02）：
//   - 每条真实发送路径在请求审计绑定之后显式绑定诊断观察者，一次 RoundTrip 只观察一次；
//   - 观察者带回的正是该次发送实际发往上游的字节与状态，与重试后的成功互不覆盖；
//   - 门控（Enabled + RiskAcknowledged）未开启时根本不绑定；
//   - 写入失败、积压或缺请求审计计数都不改变原上游／客户端结果，也不丢尝试序号；
//   - 跨路由共享的原始 Chat 发送器按调用方逐路由传入的协议记账，不得串号。

const messagesDiagnosticBodySentinel = "MESSAGES_DIAGNOSTIC_CANARY_PAYLOAD"

// errorDiagnosticRecordedAttempt 记录接缝交给诊断服务的一次尝试。
type errorDiagnosticRecordedAttempt struct {
	attempt ErrorDiagnosticAttempt
	body    []byte
}

// errorDiagnosticRecorderStub 是窄写入契约的测试替身；写入是异步的，读取要轮询。
type errorDiagnosticRecorderStub struct {
	mu       sync.Mutex
	recorded []errorDiagnosticRecordedAttempt
	err      error
	calls    int
}

func (r *errorDiagnosticRecorderStub) RecordErrorDiagnostic(_ context.Context, attempt ErrorDiagnosticAttempt) (ErrorDiagnosticRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.recorded = append(r.recorded, errorDiagnosticRecordedAttempt{
		attempt: attempt,
		// 刻意不再复制：观察者必须自己拥有这份字节，接缝随后清零自己的缓冲不得影响它。
		body: attempt.Body,
	})
	return ErrorDiagnosticRecord{ID: strings.Repeat("a", ErrorDiagnosticIDLength)}, r.err
}

func (r *errorDiagnosticRecorderStub) snapshot() []errorDiagnosticRecordedAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]errorDiagnosticRecordedAttempt(nil), r.recorded...)
}

func (r *errorDiagnosticRecorderStub) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// errorDiagnosticSettingsRepoStub 只实现诊断门控读取用到的 GetValue。
type errorDiagnosticSettingsRepoStub struct {
	SettingRepository
	value string
	err   error
}

// GetValue 对确认键固定回一条当前版本的书面确认。
//
// 采集门控的唯一判定点是「存量布尔值 且 存在覆盖当前语句版本的有效书面确认」
// （ApplyErrorDiagnosticRiskAcknowledgement），所以夹具必须在确认键上给出有效记录，
// 否则布尔值为真的用例也会永远走拒绝分支（假通过）。关闭态用例不受影响：
// CaptureAllowed 为假时判定点根本不读确认键，本分支不会被走到。
func (r errorDiagnosticSettingsRepoStub) GetValue(_ context.Context, key string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	if key == SettingKeyErrorDiagnosticRiskAcknowledgement {
		return errorDiagnosticAckJSON(ErrorDiagnosticRiskAcknowledgementVersion), nil
	}
	return r.value, nil
}

func errorDiagnosticSettingsJSON(enabled, acknowledged, bodyRetention bool) string {
	payload, _ := json.Marshal(ErrorDiagnosticSettings{
		Enabled:              enabled,
		RiskAcknowledged:     acknowledged,
		BodyRetentionEnabled: bodyRetention,
	})
	return string(payload)
}

// errorDiagnosticStableEncryptionKey 是夹具用的稳定 AES-256 密钥（64 位 hex = 32 字节）。
//
// 接缝只有在门控允许**且**这把密钥可用时才会 tee 出站正文，因此需要正文的用例必须带上它，
// 否则「观察者带回真实字节」的断言会静默退化成未采正文。
const errorDiagnosticStableEncryptionKey = "2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b"

// newMessagesDiagnosticSettingService 构造一个真实门控读取链路上的 SettingService。
func newMessagesDiagnosticSettingService(value string, err error) *SettingService {
	return &SettingService{
		settingRepo: errorDiagnosticSettingsRepoStub{value: value, err: err},
		// 照一个有稳定密钥的部署建模：正文留存的 tee 取决于它，而不是只取决于门控布尔值。
		cfg: &config.Config{Totp: config.TotpConfig{
			EncryptionKey:           errorDiagnosticStableEncryptionKey,
			EncryptionKeyConfigured: true,
		}},
	}
}

// messagesDiagnosticUpstreamStub 是忠实复刻传输接缝的 HTTPUpstream 替身：
// 它与 repository/http_upstream.go 的 roundTripAttempt 做同样的事——
// 开启一次尝试、按显式 opt-in 限量观察出站正文、在真实 RoundTrip 之后才上报观察结果。
// 因此它既能证明服务侧确实绑定了观察者，也能证明服务侧拿到的正文就是上游收到的字节。
type messagesDiagnosticUpstreamStub struct {
	// responses 按顺序消费；用尽后重复最后一项。
	responses []messagesDiagnosticUpstreamResponse
	// skipAttemptRecording 模拟「没有请求审计计数」的传输：不产生 Attempt，
	// 用来验证尝试序号仍有兜底来源。
	skipAttemptRecording bool
	// onSend 在每次「发送」时被调用，供测试模拟客户端在等上游回复时断开。
	onSend func()
	// readLimit > 0 时只读走这么多出站字节就停下，模拟发送中途失败（正文未读到 EOF）。
	readLimit int

	mu            sync.Mutex
	sentBodies    [][]byte
	observerBound []bool
	// tees 记录每次「发送」时传输层是否真的复制了出站明文（非 nil 的 diagnostic capture）。
	// 没有稳定密钥时必须是全 false：这些字节注定被判为 skipped_encryption_unavailable。
	tees  []bool
	sends int
}

type messagesDiagnosticUpstreamResponse struct {
	status int
	body   string
	header http.Header
}

func newMessagesDiagnosticUpstreamStub(responses ...messagesDiagnosticUpstreamResponse) *messagesDiagnosticUpstreamStub {
	return &messagesDiagnosticUpstreamStub{responses: responses}
}

func (u *messagesDiagnosticUpstreamStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.roundTrip(req)
}

func (u *messagesDiagnosticUpstreamStub) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.roundTrip(req)
}

func (u *messagesDiagnosticUpstreamStub) roundTrip(req *http.Request) (*http.Response, error) {
	if u.onSend != nil {
		u.onSend()
	}
	var attempt *httpattempt.Attempt
	if !u.skipAttemptRecording {
		attempt = httpattempt.StartRequestAttempt(req)
	}
	capture := httpattempt.NewDiagnosticBodyCapture(req)
	defer capture.Release()

	request := req
	if (attempt != nil || capture != nil) && req.Body != nil && req.Body != http.NoBody {
		request = req.Clone(req.Context())
		request.Body = &httpattempt.CountingReadCloser{
			ReadCloser: req.Body,
			OnRead:     attempt.AddRequestBytes,
			Capture:    capture,
		}
	}
	reader := io.Reader(request.Body)
	if u.readLimit > 0 {
		reader = io.LimitReader(request.Body, int64(u.readLimit))
	}
	payload, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	u.mu.Lock()
	index := u.sends
	u.sends++
	u.sentBodies = append(u.sentBodies, payload)
	_, observed := httpattempt.DiagnosticObserverFromContext(req.Context())
	u.observerBound = append(u.observerBound, observed)
	// nil capture 就是真实传输层的 tee 门：这次发送一个出站明文字节都没有被复制。
	u.tees = append(u.tees, capture != nil)
	if index >= len(u.responses) {
		index = len(u.responses) - 1
	}
	response := u.responses[index]
	u.mu.Unlock()

	header := response.header
	if header == nil {
		header = http.Header{"Content-Type": []string{"application/json"}}
	}
	resp := &http.Response{
		StatusCode: response.status,
		Header:     header,
		Body:       http.NoBody,
		Request:    request,
	}
	if response.body != "" {
		resp.Body = io.NopCloser(strings.NewReader(response.body))
	}
	if attempt != nil {
		attempt.SetResponse(resp.StatusCode, resp.Header, resp.Body != nil && resp.Body != http.NoBody)
	}
	// 与真实传输一致：只在 RoundTrip 真的拿到响应之后才上报观察结果。
	httpattempt.ObserveUpstreamError(req, attempt, resp, capture)
	return resp, nil
}

func (u *messagesDiagnosticUpstreamStub) sent() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.sentBodies...)
}

func (u *messagesDiagnosticUpstreamStub) observers() []bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]bool(nil), u.observerBound...)
}

// teedBodies 报告每次发送是否真的复制了出站明文。
func (u *messagesDiagnosticUpstreamStub) teedBodies() []bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]bool(nil), u.tees...)
}

func messagesDiagnosticNativeAccount() *Account {
	return &Account{
		ID:          731,
		Name:        "messages-diagnostic-native",
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":      "sk-native-diagnostic",
			"api_protocol": APIProtocolAnthropic,
			"api_base_urls": map[string]any{
				APIProtocolAnthropic: "http://anthropic.example",
			},
		},
	}
}

func messagesDiagnosticNativeService(t *testing.T, settings *SettingService, recorder ErrorDiagnosticRecorder, upstream HTTPUpstream) *OpenAIGatewayService {
	t.Helper()
	svc := &OpenAIGatewayService{
		cfg:            rawChatCompletionsTestConfig(),
		httpUpstream:   upstream,
		settingService: settings,
	}
	svc.SetErrorDiagnosticRecorder(recorder)
	return svc
}

func messagesDiagnosticJSONBody() []byte {
	return []byte(`{"model":"k3","max_tokens":32,"stream":false,"messages":[{"role":"user","content":"` +
		messagesDiagnosticBodySentinel + `"}]}`)
}

func requireMessagesDiagnosticRecords(t *testing.T, recorder *errorDiagnosticRecorderStub, want int) []errorDiagnosticRecordedAttempt {
	t.Helper()
	require.Eventually(t, func() bool {
		return len(recorder.snapshot()) >= want
	}, 3*time.Second, 5*time.Millisecond, "expected %d diagnostic writes", want)
	records := recorder.snapshot()
	require.Len(t, records, want)
	return records
}

// 核心接缝：网关 → 模拟上游 → 诊断服务。
// 上游先 500 后成功，管理员能查到先前那次失败尝试的独立元数据，
// 且诊断正文与上游实际收到的字节逐字节一致。
func TestMessagesErrorDiagnosticObservesRealUpstreamFailureWithActualOutboundBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := newMessagesDiagnosticSettingService(
		errorDiagnosticSettingsJSON(true, true, true), nil,
	)
	recorder := &errorDiagnosticRecorderStub{}
	upstream := newMessagesDiagnosticUpstreamStub(
		messagesDiagnosticUpstreamResponse{status: http.StatusInternalServerError, body: `{"error":{"message":"upstream failed"}}`},
	)
	svc := messagesDiagnosticNativeService(t, settings, recorder, upstream)

	body := messagesDiagnosticJSONBody()
	_, err := svc.ForwardAsAnthropic(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), messagesDiagnosticNativeAccount(), body, "", "")
	require.Error(t, err, "上游 5xx 仍按原语义向上层返回失败")

	records := requireMessagesDiagnosticRecords(t, recorder, 1)
	record := records[0]
	require.Equal(t, ErrorDiagnosticProtocolMessages, record.attempt.Protocol, "协议按入站 /v1/messages 推导")
	require.Equal(t, ErrorDiagnosticStageWire, record.attempt.Stage)
	require.Equal(t, 1, record.attempt.AttemptIndex, "第一次真实发送就是第 1 次尝试")
	require.Equal(t, http.StatusInternalServerError, record.attempt.UpstreamStatusCode)
	require.Equal(t, int64(0), record.attempt.UsageLogID, "wire 接缝没有 usage 记录，不得伪造关联")
	require.True(t, record.attempt.BodyReadComplete, "传输层读完了出站正文")
	require.Equal(t, ErrorDiagnosticBodyVerdictComplete, record.attempt.BodyVerdict)
	require.Contains(t, string(record.body), messagesDiagnosticBodySentinel)

	sent := upstream.sent()
	require.Len(t, sent, 1)
	require.Equal(t, string(sent[0]), string(record.body), "诊断正文必须等于上游实际收到的字节")
	require.Equal(t, messagesDiagnosticJSONBody(), sent[0])
}

// 未开启门控（或运维未做风险确认）时不得绑定观察者，也不产生任何写入。
func TestMessagesErrorDiagnosticStaysOffUnlessEnabledAndRiskAcknowledged(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name   string
		value  string
		readEr bool
	}{
		{name: "settings missing", value: ""},
		{name: "enabled but risk not acknowledged", value: errorDiagnosticSettingsJSON(true, false, true)},
		{name: "acknowledged but disabled", value: errorDiagnosticSettingsJSON(false, true, true)},
		{name: "settings read fails", readEr: true},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var readErr error
			if tt.readEr {
				readErr = context.DeadlineExceeded
			}
			settings := newMessagesDiagnosticSettingService(tt.value, readErr)
			recorder := &errorDiagnosticRecorderStub{}
			upstream := newMessagesDiagnosticUpstreamStub(
				messagesDiagnosticUpstreamResponse{status: http.StatusInternalServerError, body: `{"error":{"message":"upstream failed"}}`},
			)
			svc := messagesDiagnosticNativeService(t, settings, recorder, upstream)

			body := messagesDiagnosticJSONBody()
			_, err := svc.ForwardAsAnthropic(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), messagesDiagnosticNativeAccount(), body, "", "")
			require.Error(t, err)

			require.Equal(t, []bool{false}, upstream.observers(), "门控未开启时请求上下文不得出现诊断观察者")
			require.Empty(t, recorder.snapshot())
			require.Zero(t, recorder.callCount())
		})
	}
}

// 正文留存是分阶段 opt-in：只开启元数据时观察者仍在，但不带走正文。
func TestMessagesErrorDiagnosticKeepsMetadataOnlyWithoutBodyRetention(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := newMessagesDiagnosticSettingService(
		errorDiagnosticSettingsJSON(true, true, false), nil,
	)
	recorder := &errorDiagnosticRecorderStub{}
	upstream := newMessagesDiagnosticUpstreamStub(
		messagesDiagnosticUpstreamResponse{status: http.StatusBadGateway, body: `{"error":{"message":"bad gateway"}}`},
	)
	svc := messagesDiagnosticNativeService(t, settings, recorder, upstream)

	body := messagesDiagnosticJSONBody()
	_, err := svc.ForwardAsAnthropic(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), messagesDiagnosticNativeAccount(), body, "", "")
	require.Error(t, err)

	records := requireMessagesDiagnosticRecords(t, recorder, 1)
	require.Equal(t, []bool{true}, upstream.observers())
	require.Equal(t, http.StatusBadGateway, records[0].attempt.UpstreamStatusCode)
	require.Empty(t, records[0].body, "未 opt-in 正文留存时不得带走正文")
	require.False(t, records[0].attempt.BodyReadComplete)
	require.Equal(t, ErrorDiagnosticBodyVerdictNotRequested, records[0].attempt.BodyVerdict)
}

// 没有请求审计计数时，尝试序号仍要有真实来源（本逻辑请求内的发送顺序），不能写 0。
func TestMessagesErrorDiagnosticKeepsAttemptOrdinalWithoutRequestAuditCounter(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := newMessagesDiagnosticSettingService(
		errorDiagnosticSettingsJSON(true, true, true), nil,
	)
	recorder := &errorDiagnosticRecorderStub{}
	upstream := newMessagesDiagnosticUpstreamStub(
		messagesDiagnosticUpstreamResponse{status: http.StatusServiceUnavailable, body: `{"error":{"message":"unavailable"}}`},
	)
	upstream.skipAttemptRecording = true
	svc := messagesDiagnosticNativeService(t, settings, recorder, upstream)

	body := messagesDiagnosticJSONBody()
	_, err := svc.ForwardAsAnthropic(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), messagesDiagnosticNativeAccount(), body, "", "")
	require.Error(t, err)

	records := requireMessagesDiagnosticRecords(t, recorder, 1)
	require.Equal(t, 1, records[0].attempt.AttemptIndex, "缺少审计计数时用本请求内的发送序号兜底")
}

// 诊断写入失败必须 fail-open：不改变上游结果，也不向调用方冒泡。
func TestMessagesErrorDiagnosticWriteFailureDoesNotChangeUpstreamResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := newMessagesDiagnosticSettingService(
		errorDiagnosticSettingsJSON(true, true, true), nil,
	)
	recorder := &errorDiagnosticRecorderStub{err: ErrErrorDiagnosticUnavailable}
	upstream := newMessagesDiagnosticUpstreamStub(
		messagesDiagnosticUpstreamResponse{status: http.StatusInternalServerError, body: `{"error":{"message":"upstream failed"}}`},
	)
	svc := messagesDiagnosticNativeService(t, settings, recorder, upstream)

	body := messagesDiagnosticJSONBody()
	_, err := svc.ForwardAsAnthropic(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), messagesDiagnosticNativeAccount(), body, "", "")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrErrorDiagnosticUnavailable, "诊断写入失败不得改变原错误语义")

	requireMessagesDiagnosticRecords(t, recorder, 1)
}

// Anthropic API Key 透传分支同样按真实发送逐次采集：重试成功不覆盖先前的失败尝试。
func TestMessagesErrorDiagnosticPassthroughKeepsEarlierFailureAfterRetrySucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := newMessagesDiagnosticSettingService(
		errorDiagnosticSettingsJSON(true, true, true), nil,
	)
	recorder := &errorDiagnosticRecorderStub{}
	successBody := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-7-sonnet-20250219",` +
		`"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":3}}`
	upstream := newMessagesDiagnosticUpstreamStub(
		messagesDiagnosticUpstreamResponse{status: http.StatusServiceUnavailable, body: `{"error":{"message":"unavailable"}}`},
		messagesDiagnosticUpstreamResponse{status: http.StatusOK, body: successBody},
	)

	cfg := rawChatCompletionsTestConfig()
	svc := &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     &RateLimitService{},
		deferredService:      &DeferredService{},
		settingService:       settings,
	}
	svc.SetErrorDiagnosticRecorder(recorder)

	body := []byte(`{"model":"claude-3-7-sonnet-20250219","stream":false,"messages":[{"role":"user","content":"` +
		messagesDiagnosticBodySentinel + `"}]}`)
	parsed := &ParsedRequest{
		Body:  NewRequestBodyRef(body),
		Model: "claude-3-7-sonnet-20250219",
	}
	account := newAnthropicAPIKeyAccountForTest()
	account.Credentials["api_key"] = "upstream-anthropic-key"
	// API Key 账号默认不重试任何状态码；显式配置自定义错误码集合（不含 503）后，
	// 503 才会走同账号重试，从而验证「重试成功不覆盖先前的失败尝试」。
	account.Credentials["custom_error_codes_enabled"] = true
	account.Credentials["custom_error_codes"] = []any{float64(http.StatusInternalServerError)}

	result, err := svc.Forward(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), account, parsed)
	require.NoError(t, err)
	require.NotNil(t, result)

	records := requireMessagesDiagnosticRecords(t, recorder, 1)
	require.Equal(t, http.StatusServiceUnavailable, records[0].attempt.UpstreamStatusCode)
	require.Equal(t, 1, records[0].attempt.AttemptIndex)
	require.Equal(t, ErrorDiagnosticProtocolMessages, records[0].attempt.Protocol)

	sent := upstream.sent()
	require.Len(t, sent, 2, "第一次失败后重试一次")
	require.Equal(t, string(sent[0]), string(records[0].body), "诊断正文对应的是失败的那一次发送")
	require.Equal(t, sent[0], sent[1], "透传重试不改写正文")
}

// 覆盖边界：/v1/messages 的原始 Chat Completions 兜底走共享发送器，
// 该发送器由调用方逐路由传入协议字面量（sendCCUpstreamRequest 的 diagnosticProtocol 参数），
// 因此这里必须记成 messages，而不是靠路径猜出来或被别的路由改写。
// 反例（同一发送器的 chat_completions／responses 调用方不得串号）由对应分支的自有测试固定。
func TestMessagesErrorDiagnosticCoversRawChatFallbackUnderItsOwnProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := newMessagesDiagnosticSettingService(
		errorDiagnosticSettingsJSON(true, true, true), nil,
	)
	recorder := &errorDiagnosticRecorderStub{}
	upstream := newMessagesDiagnosticUpstreamStub(
		messagesDiagnosticUpstreamResponse{status: http.StatusInternalServerError, body: `{"error":{"message":"upstream failed"}}`},
	)
	svc := messagesDiagnosticNativeService(t, settings, recorder, upstream)

	// 显式关闭 Responses 支持：/v1/messages 走原始 Chat Completions 兜底。
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: false}

	body := messagesDiagnosticJSONBody()
	_, err := svc.ForwardAsAnthropic(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), account, body, "", "")
	require.Error(t, err)

	require.Equal(t, []bool{true}, upstream.observers(), "共享发送器由本分支显式绑定观察者")

	records := requireMessagesDiagnosticRecords(t, recorder, 1)
	require.Equal(t, ErrorDiagnosticProtocolMessages, records[0].attempt.Protocol,
		"共享发送器必须按调用方给出的入站协议记账")
	require.Equal(t, http.StatusInternalServerError, records[0].attempt.UpstreamStatusCode)

	sent := upstream.sent()
	require.Len(t, sent, 1)
	require.Contains(t, string(sent[0]), messagesDiagnosticBodySentinel, "上游收到的是转换后的 Chat Completions 正文")
	require.Equal(t, string(sent[0]), string(records[0].body), "诊断正文必须等于该次发送实际发出的字节")
}

// 未注入接缝时不得改变任何行为，也不得在请求上下文留下值。
func TestMessagesErrorDiagnosticWithoutRecorderLeavesRequestUntouched(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := newMessagesDiagnosticUpstreamStub(
		messagesDiagnosticUpstreamResponse{status: http.StatusInternalServerError, body: `{"error":{"message":"upstream failed"}}`},
	)
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	body := messagesDiagnosticJSONBody()
	_, err := svc.ForwardAsAnthropic(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), messagesDiagnosticNativeAccount(), body, "", "")
	require.Error(t, err)
	require.Equal(t, []bool{false}, upstream.observers())
}

// 观察者只观察真实 4xx/5xx：成功响应不产生任何写入，也不改变上游结果。
func TestMessagesErrorDiagnosticIgnoresSuccessfulUpstreamResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := newMessagesDiagnosticSettingService(
		errorDiagnosticSettingsJSON(true, true, true), nil,
	)
	recorder := &errorDiagnosticRecorderStub{}
	upstream := newMessagesDiagnosticUpstreamStub(messagesDiagnosticUpstreamResponse{
		status: http.StatusOK,
		body: `{"id":"msg_1","type":"message","role":"assistant","model":"k3",` +
			`"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":3}}`,
	})
	svc := messagesDiagnosticNativeService(t, settings, recorder, upstream)

	body := messagesDiagnosticJSONBody()
	result, err := svc.ForwardAsAnthropic(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), messagesDiagnosticNativeAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, []bool{true}, upstream.observers(), "观察者已绑定但不会被成功响应触发")

	time.Sleep(50 * time.Millisecond)
	require.Empty(t, recorder.snapshot())
}

// 诊断写入必须脱离客户端取消：客户端断开也要照常落库。
func TestMessagesErrorDiagnosticWriteSurvivesClientCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := newMessagesDiagnosticSettingService(
		errorDiagnosticSettingsJSON(true, true, true), nil,
	)
	recorder := &errorDiagnosticRecorderStub{}
	upstream := newMessagesDiagnosticUpstreamStub(
		messagesDiagnosticUpstreamResponse{status: http.StatusInternalServerError, body: `{"error":{"message":"upstream failed"}}`},
	)
	ctx, cancel := context.WithCancel(context.Background())
	// 客户端在上游回复之前就断开：诊断写入必须仍然落库，且不改变原错误语义。
	upstream.onSend = cancel
	svc := messagesDiagnosticNativeService(t, settings, recorder, upstream)

	body := messagesDiagnosticJSONBody()
	c := adaptiveProtocolTestContext("/v1/messages", body)
	c.Request = c.Request.WithContext(ctx)
	_, err := svc.ForwardAsAnthropic(ctx, c, messagesDiagnosticNativeAccount(), body, "", "")
	require.Error(t, err)

	requireMessagesDiagnosticRecords(t, recorder, 1)
	require.Equal(t, http.StatusInternalServerError, recorder.snapshot()[0].attempt.UpstreamStatusCode)
}

// 保证测试用的常量与生产判定一致：门控必须同时启用才能采集。
func TestMessagesErrorDiagnosticSettingsGateSemantics(t *testing.T) {
	require.True(t, ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true}.CaptureAllowed())
	require.False(t, ErrorDiagnosticSettings{Enabled: true}.CaptureAllowed())
	require.False(t, ErrorDiagnosticSettings{RiskAcknowledged: true}.CaptureAllowed())
	require.False(t, ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true}.BodyCaptureAllowed())
	require.True(t, ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true}.BodyCaptureAllowed())
}

// 兜底序号在缺少审计计数时按请求内的发送顺序推进，每个协议各自计数、互不串号。
func TestErrorDiagnosticSendOrdinalFallbackIsPerProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c := adaptiveProtocolTestContext("/v1/messages", nil)
	messagesKey := errorDiagnosticOrdinalContextKey(ErrorDiagnosticProtocolMessages)
	chatKey := errorDiagnosticOrdinalContextKey(ErrorDiagnosticProtocolChatCompletions)

	require.Equal(t, 1, nextErrorDiagnosticOrdinal(c, messagesKey))
	require.Equal(t, 2, nextErrorDiagnosticOrdinal(c, messagesKey))
	require.Equal(t, 1, nextErrorDiagnosticOrdinal(c, chatKey), "不同协议的序号互不影响")
	require.Equal(t, 3, nextErrorDiagnosticOrdinal(c, messagesKey))
	require.Empty(t, errorDiagnosticOrdinalContextKey("gemini"), "未覆盖协议没有可采集的序号 key")
}

// 未知协议必须 fail closed：即使门控开启也不绑定观察者。
func TestErrorDiagnosticBinderRejectsUncoveredProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := newMessagesDiagnosticSettingService(
		errorDiagnosticSettingsJSON(true, true, true), nil,
	)
	recorder := &errorDiagnosticRecorderStub{}
	upstream := newMessagesDiagnosticUpstreamStub(
		messagesDiagnosticUpstreamResponse{status: http.StatusInternalServerError, body: `{"error":{"message":"upstream failed"}}`},
	)
	svc := messagesDiagnosticNativeService(t, settings, recorder, upstream)

	body := messagesDiagnosticJSONBody()
	c := adaptiveProtocolTestContext("/v1/messages", body)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://upstream.example/v1/messages", bytes.NewReader(body))
	require.NoError(t, err)
	require.Same(t, req, svc.bindErrorDiagnosticObserver(req, c, "gemini"))
	require.False(t, messagesDiagnosticObserverBound(req), "未覆盖协议不得绑定观察者")
}

// 已覆盖协议在门控允许时会绑定观察者，且不影响其它协议。
func TestErrorDiagnosticBinderBindsCoveredProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := newMessagesDiagnosticSettingService(
		errorDiagnosticSettingsJSON(true, true, true), nil,
	)
	svc := messagesDiagnosticNativeService(t, settings, &errorDiagnosticRecorderStub{}, newMessagesDiagnosticUpstreamStub(
		messagesDiagnosticUpstreamResponse{status: http.StatusOK},
	))

	c := adaptiveProtocolTestContext("/v1/messages", nil)
	for _, protocol := range []string{
		ErrorDiagnosticProtocolMessages,
		ErrorDiagnosticProtocolChatCompletions,
		ErrorDiagnosticProtocolResponses,
	} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://upstream.example/v1/messages", nil)
		require.NoError(t, err)
		bound := svc.bindErrorDiagnosticObserver(req, c, protocol)
		require.True(t, messagesDiagnosticObserverBound(bound), "协议 %s 应被绑定", protocol)
	}
}

// 门控布尔值为真但没有稳定密钥时，不得为出站正文做 tee：
// 那些字节注定被判为 skipped_encryption_unavailable，传输层不该为它们读走并复制明文。
// 元数据采集不受影响：观察者照常绑定，只是不带正文。
func TestErrorDiagnosticBinderDoesNotTeeBodyWithoutStableKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settingsJSON := errorDiagnosticSettingsJSON(true, true, true)

	cases := []struct {
		name     string
		settings *SettingService
		wantTee  bool
	}{
		{
			name:     "no key configured",
			settings: &SettingService{settingRepo: errorDiagnosticSettingsRepoStub{value: settingsJSON}},
			wantTee:  false,
		},
		{
			// 启动时随机生成的密钥每次重启都会变，已留存密文将不可解，因此不算稳定密钥。
			name: "auto-generated key is not stable",
			settings: &SettingService{
				settingRepo: errorDiagnosticSettingsRepoStub{value: settingsJSON},
				cfg: &config.Config{Totp: config.TotpConfig{
					EncryptionKey:           errorDiagnosticStableEncryptionKey,
					EncryptionKeyConfigured: false,
				}},
			},
			wantTee: false,
		},
		{
			name:     "configured stable key",
			settings: newMessagesDiagnosticSettingService(settingsJSON, nil),
			wantTee:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := messagesDiagnosticNativeService(t, tc.settings, &errorDiagnosticRecorderStub{}, newMessagesDiagnosticUpstreamStub(
				messagesDiagnosticUpstreamResponse{status: http.StatusOK},
			))
			c := adaptiveProtocolTestContext("/v1/messages", nil)
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://upstream.example/v1/messages", nil)
			require.NoError(t, err)

			bound := svc.bindErrorDiagnosticObserver(req, c, ErrorDiagnosticProtocolMessages)
			observer, ok := httpattempt.DiagnosticObserverFromContext(bound.Context())
			require.True(t, ok, "密钥只影响正文：元数据采集仍应绑定观察者")
			require.Equal(t, tc.wantTee, observer.CaptureRequestBody,
				"只有配置了稳定密钥的部署才允许 tee 出站正文")

			// 真实传输层的 tee 门：nil 表示这次发送一个出站明文字节都不会被复制。
			if tc.wantTee {
				require.NotNil(t, httpattempt.NewDiagnosticBodyCapture(bound), "有稳定密钥时才 tee")
			} else {
				require.Nil(t, httpattempt.NewDiagnosticBodyCapture(bound),
					"没有稳定密钥时零明文复制")
			}
		})
	}
}

// 门控与书面确认都到位、但部署没有稳定密钥时：元数据照常采集，正文一次都不 tee，
// 且该次尝试必须带上「要求过留存、做不到」的封闭抑制结论。
//
// 这锁定「有效正文留存」的门槛在读取侧就被收窄，而不是等写入时逐行跳过——
// 否则传输层会先复制一份注定被丢弃的明文。封闭抑制结论则由
// TestMessagesErrorDiagnosticReportsClosedSuppressionReasonWithoutStableKey 端到端锁定。
func TestMessagesErrorDiagnosticKeepsMetadataOnlyWithoutStableKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := &SettingService{
		settingRepo: errorDiagnosticSettingsRepoStub{
			value: errorDiagnosticSettingsJSON(true, true, true),
		},
		// 刻意不给 cfg：等同启动时自动生成密钥的部署。
	}
	recorder := &errorDiagnosticRecorderStub{}
	upstream := newMessagesDiagnosticUpstreamStub(
		messagesDiagnosticUpstreamResponse{status: http.StatusBadGateway, body: `{"error":{"message":"bad gateway"}}`},
	)
	svc := messagesDiagnosticNativeService(t, settings, recorder, upstream)

	body := messagesDiagnosticJSONBody()
	_, err := svc.ForwardAsAnthropic(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), messagesDiagnosticNativeAccount(), body, "", "")
	require.Error(t, err)

	records := requireMessagesDiagnosticRecords(t, recorder, 1)
	require.Equal(t, []bool{true}, upstream.observers(), "元数据采集不受密钥影响：观察者仍应绑定")
	require.Equal(t, http.StatusBadGateway, records[0].attempt.UpstreamStatusCode)
	require.Empty(t, records[0].body, "没有稳定密钥时不得 tee 出站正文")
	require.Equal(t, []bool{false}, upstream.teedBodies(), "传输层不得复制注定被丢弃的明文")
	require.False(t, records[0].attempt.BodyReadComplete)
	require.Equal(t, ErrorDiagnosticBodyVerdictSuppressedEncryptionUnavailable, records[0].attempt.BodyVerdict,
		"要求过留存而密钥缺席：必须报封闭抑制，不得伪装成已读取，也不得退化成 not_observed")
}

// 存量门控**要求**留存正文、书面确认覆盖当前语句，但部署此刻拿不出稳定密钥：
// 这是「启用后密钥被移除／不再重启稳定」的形态——管理端本来就拒绝在缺密钥时打开正文留存
// （ERROR_DIAGNOSTIC_BODY_KEY_UNAVAILABLE），所以只能在密钥失效后造出这份存量值。
//
// 票据 02 的验收标准要求「缺密钥……仅有安全元数据及稳定原因码」，因此这一行必须同时满足：
//   - 传输层一个出站明文字节都不 tee（绝不复制注定被丢弃的正文）；
//   - 落库的元数据留下 skipped_encryption_unavailable，而不是 not_observed。
//
// not_observed 的语义是「本次没有 opt-in 正文采集」，用它描述缺密钥会把配置故障显示成
// 正常运行，并与运维界面的 key-unavailable 状态自相矛盾。这里走真实的 Messages 观察者
// 与真实的 Record 服务（含真实门控读取链路），不复用任何替身决定。
func TestMessagesErrorDiagnosticReportsClosedSuppressionReasonWithoutStableKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := &SettingService{
		settingRepo: errorDiagnosticSettingsRepoStub{
			// 存量：采集开启、正文留存开启；确认键由桩回当前版本的有效书面确认。
			value: errorDiagnosticSettingsJSON(true, true, true),
		},
		// 刻意不给 cfg：等同「密钥已从部署里移除／不再稳定」。
	}

	repo := newErrorDiagnosticRepoFake()
	cipher := &errorDiagnosticCipherFake{}
	diagnostics := NewErrorDiagnosticService(repo, settings, cipher)
	upstream := newMessagesDiagnosticUpstreamStub(
		messagesDiagnosticUpstreamResponse{status: http.StatusBadGateway, body: `{"error":{"message":"bad gateway"}}`},
	)
	svc := messagesDiagnosticNativeService(t, settings, diagnostics, upstream)

	body := messagesDiagnosticJSONBody()
	_, err := svc.ForwardAsAnthropic(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), messagesDiagnosticNativeAccount(), body, "", "")
	require.Error(t, err)

	// 场景本身必须成立：这次发送确实带上了哨兵明文，否则「没 tee」是假事实。
	// 传输层的事实先断言：ForwardAsAnthropic 返回时发送已全部结束，因此它们是确定的。
	sent := upstream.sent()
	require.Len(t, sent, 1)
	require.Contains(t, string(sent[0]), messagesDiagnosticBodySentinel)
	require.Equal(t, []bool{true}, upstream.observers(), "元数据采集不受密钥影响：观察者仍应绑定")
	require.Equal(t, []bool{false}, upstream.teedBodies(), "缺密钥时传输层不得为出站正文做 tee")

	require.Eventually(t, func() bool {
		return len(repo.created) == 1
	}, 3*time.Second, 5*time.Millisecond, "缺密钥只影响正文：元数据仍应落库")
	require.Empty(t, cipher.encrypted, "缺密钥时不得有任何明文进入加密路径")

	write := repo.created[0]
	require.Empty(t, write.Attempt.Body, "没有 tee 的发送不得夹带任何出站字节")
	require.Equal(t, ErrorDiagnosticBodyVerdictSuppressedEncryptionUnavailable, write.Attempt.BodyVerdict,
		"封闭抑制必须由显式 verdict 表达，否则只能退化成 not_observed")
	require.False(t, write.Attempt.BodyReadComplete)
	require.Equal(t, ErrorDiagnosticBodyStateSkipped, write.BodyState)
	require.Equal(t, ErrorDiagnosticBodySkippedEncryptionUnavailable, write.BodyReason,
		"票据 02：缺密钥时留下稳定原因码，而不是 not_observed")
}

// errorDiagnosticDetachedContextKey 是「下游派生上下文」用的命名键：
// 匿名 struct 作 context key 会与其它包冲突（SA1029），命名类型才是可与本包共存的键。
type errorDiagnosticDetachedContextKey struct{}

// 分支级上下文绑定：观察者随 ctx 值传递到后续任意出站请求，协议由分支决定，
// 各分支互不串号（Responses/Chat Completions 分支用这个入口）。
func TestErrorDiagnosticBranchContextBindsProtocolAndPropagatesToOutboundRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := newMessagesDiagnosticSettingService(
		errorDiagnosticSettingsJSON(true, true, true), nil,
	)
	recorder := &errorDiagnosticRecorderStub{}
	upstream := newMessagesDiagnosticUpstreamStub(
		messagesDiagnosticUpstreamResponse{status: http.StatusBadGateway, body: `{"error":{"message":"bad gateway"}}`},
	)
	svc := messagesDiagnosticNativeService(t, settings, recorder, upstream)

	c := adaptiveProtocolTestContext("/v1/responses", nil)
	ctx := svc.bindResponsesErrorDiagnosticBranch(context.Background(), c)

	// 模拟下游「脱离客户端取消地」派生请求上下文：值必须保留。
	detached, cancel := context.WithCancel(context.WithValue(ctx, errorDiagnosticDetachedContextKey{}, "downstream"))
	defer cancel()
	req, err := http.NewRequestWithContext(detached, http.MethodPost, "http://upstream.example/v1/responses", strings.NewReader(`{"model":"gpt-5.4"}`))
	require.NoError(t, err)
	require.True(t, messagesDiagnosticObserverBound(req), "分支上下文上的观察者必须传递到出站请求")

	resp, err := upstream.Do(req, "", 1, 1)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	records := requireMessagesDiagnosticRecords(t, recorder, 1)
	require.Equal(t, ErrorDiagnosticProtocolResponses, records[0].attempt.Protocol,
		"协议来自分支参数，不随共享实现漂移成 messages")
	require.Equal(t, http.StatusBadGateway, records[0].attempt.UpstreamStatusCode)

	// 同一分支上下文的第二次发送：兜底序号按协议各推进一次。
	require.Equal(t, 2, nextErrorDiagnosticOrdinal(c, errorDiagnosticOrdinalContextKey(ErrorDiagnosticProtocolResponses)))
}

// 传输层扣住字节的两种结论必须显式上报，不能被折叠成「未观察到正文」。
func TestMessagesErrorDiagnosticForwardsTransportBodyVerdict(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("oversized outbound body is reported as too_large", func(t *testing.T) {
		settings := newMessagesDiagnosticSettingService(errorDiagnosticSettingsJSON(true, true, true), nil)
		recorder := &errorDiagnosticRecorderStub{}
		upstream := newMessagesDiagnosticUpstreamStub(
			messagesDiagnosticUpstreamResponse{status: http.StatusInternalServerError, body: `{"error":{"message":"upstream failed"}}`},
		)
		svc := messagesDiagnosticNativeService(t, settings, recorder, upstream)

		// 超过 1 MiB 留存上限：观察者不得带走字节，但必须说清是「超限」而不是「没采」。
		padding := strings.Repeat("x", int(ErrorDiagnosticMaxBodyBytes)+1024)
		body := []byte(`{"model":"k3","max_tokens":32,"stream":false,"messages":[{"role":"user","content":"` + padding + `"}]}`)
		_, err := svc.ForwardAsAnthropic(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), messagesDiagnosticNativeAccount(), body, "", "")
		require.Error(t, err)

		records := requireMessagesDiagnosticRecords(t, recorder, 1)
		require.Equal(t, ErrorDiagnosticBodyVerdictTooLarge, records[0].attempt.BodyVerdict)
		require.Empty(t, records[0].body)
		// verdict 优先：超限不得在元数据里留下「已完整读取」的假象。
		require.False(t, records[0].attempt.BodyReadComplete,
			"超限只由 verdict 表达，BodyReadComplete 不得报完整")
		require.Equal(t, http.StatusInternalServerError, records[0].attempt.UpstreamStatusCode)
	})

	t.Run("partially sent outbound body is reported as incomplete", func(t *testing.T) {
		settings := newMessagesDiagnosticSettingService(errorDiagnosticSettingsJSON(true, true, true), nil)
		recorder := &errorDiagnosticRecorderStub{}
		upstream := newMessagesDiagnosticUpstreamStub(
			messagesDiagnosticUpstreamResponse{status: http.StatusServiceUnavailable, body: `{"error":{"message":"unavailable"}}`},
		)
		upstream.readLimit = 8
		svc := messagesDiagnosticNativeService(t, settings, recorder, upstream)

		body := messagesDiagnosticJSONBody()
		_, err := svc.ForwardAsAnthropic(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), messagesDiagnosticNativeAccount(), body, "", "")
		require.Error(t, err)

		records := requireMessagesDiagnosticRecords(t, recorder, 1)
		require.Equal(t, ErrorDiagnosticBodyVerdictIncomplete, records[0].attempt.BodyVerdict)
		require.Empty(t, records[0].body, "未读完整不得把半截字节当成出站正文")
		require.False(t, records[0].attempt.BodyReadComplete)
	})
}

// Bedrock Messages 分支覆盖：GatewayService.Forward 在 Bedrock 处提前返回，
// 早于通用绑定点，因此 Bedrock 的真实发送必须在自己的发送接缝上按 messages 协议绑定。
func TestMessagesErrorDiagnosticCoversBedrockMessagesBranch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := newMessagesDiagnosticSettingService(
		errorDiagnosticSettingsJSON(true, true, true), nil,
	)
	recorder := &errorDiagnosticRecorderStub{}
	upstream := newMessagesDiagnosticUpstreamStub(
		messagesDiagnosticUpstreamResponse{status: http.StatusInternalServerError, body: `{"message":"bedrock upstream failed"}`},
	)
	cfg := rawChatCompletionsTestConfig()
	svc := &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     &RateLimitService{},
		deferredService:      &DeferredService{},
		settingService:       settings,
	}
	svc.SetErrorDiagnosticRecorder(recorder)

	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":32,"stream":false,"messages":[{"role":"user","content":"` +
		messagesDiagnosticBodySentinel + `"}]}`)
	parsed := &ParsedRequest{Body: NewRequestBodyRef(body), Model: "claude-sonnet-4-5"}
	account := &Account{
		ID:          733,
		Name:        "bedrock-diagnostic",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeBedrock,
		Concurrency: 1,
		Credentials: map[string]any{
			"aws_region": "us-east-1",
			"auth_mode":  "apikey",
			"api_key":    "bedrock-sentinel-key",
		},
	}

	_, _ = svc.Forward(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), account, parsed)

	records := requireMessagesDiagnosticRecords(t, recorder, 1)
	require.Equal(t, ErrorDiagnosticProtocolMessages, records[0].attempt.Protocol,
		"Bedrock 入站同样是 /v1/messages")
	require.Equal(t, http.StatusInternalServerError, records[0].attempt.UpstreamStatusCode)

	sent := upstream.sent()
	require.Len(t, sent, 1, "Bedrock 5xx 对 API Key 账号不重试")
	require.Contains(t, string(sent[0]), messagesDiagnosticBodySentinel)
	require.Equal(t, string(sent[0]), string(records[0].body),
		"诊断正文必须等于 Bedrock 该次真实发送的字节")
}

// messagesDiagnosticObserverBound 报告请求上下文里是否存在诊断观察者。
func messagesDiagnosticObserverBound(req *http.Request) bool {
	if req == nil {
		return false
	}
	_, ok := httpattempt.DiagnosticObserverFromContext(req.Context())
	return ok
}
