//go:build unit

package service

// 本文件固定「原生 Anthropic Messages 转发主循环」上错误诊断接缝的行为。
//
// 范围：GatewayService.Forward 对 Anthropic OAuth 账号的 /v1/messages 主循环（含 400 签名
// 整流重试、OAuth 403 同账号重试与强制审计的发送前阻断）。不含 Anthropic API Key 透传分支与
// Bedrock 分支——它们的发送接缝由各自的自有测试固定。
//
// 断言的都是外部可观测事实：
//   - 每次真实发往上游的 4xx 都各自成一条诊断，与同一逻辑请求里后续的成功尝试互不覆盖；
//   - 每条诊断的正文就是**那一次**实际发出的字节：整流重试会改写正文，因此两次尝试的正文
//     必须分别与本次的出站字节逐字节一致，且按尝试序号一一配对；
//   - 尝试序号来自真实发送顺序（请求审计计数），重试不重复占号；
//   - wire 级诊断不是 usage-owned 记录：不挂使用记录，也不伪造关联（ADR 0002／0005）；
//   - 强制审计的发送前阻断语义不变：被拦下的尝试不占序号、不发出任何字节、不产生任何观察。

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

// nativeMessagesRetryThinkingText / nativeMessagesRetryUserText 是合成请求体里的稳定标记：
// 整流重试要把 thinking 正文降级为 text 保留，因此它必须在两次出站字节里都找得到。
const (
	nativeMessagesRetryThinkingText = "native-messages-retry-thinking-sentinel"
	nativeMessagesRetryUserText     = "native-messages-retry-user-sentinel"
	nativeMessagesRetrySignature    = "native-messages-retry-signature-sentinel"
)

// nativeMessagesRetrySignatureErrorBody 是上游对历史 thinking block 签名不合法的典型 400 响应，
// 形态与 Anthropic 的 {"type":"error","error":{...}} 一致，且刻意不提 tool/function，
// 以保证整流只走「降级 thinking」这一阶段。
const nativeMessagesRetrySignatureErrorBody = `{"type":"error","error":{"type":"invalid_request_error",` +
	`"message":"messages.1.content.0.thinking.signature: Field required"}}`

// nativeMessagesRetryEncryptionKey 是夹具用的稳定 AES-256 密钥（64 位 hex = 32 字节）。
// 诊断接缝只有在门控允许**且**这把密钥可用时才 tee 出站正文。
const nativeMessagesRetryEncryptionKey = "5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c"

// nativeMessagesRetryBody 构造一个 Claude Code 形态的合成 Messages 请求：
// 顶层 thinking 打开 + 历史 assistant 消息带签名 thinking block（上游会以 400 拒绝），
// 且不含任何已知结构化凭据或媒体部件，因此诊断正文可留存。
func nativeMessagesRetryBody() []byte {
	return []byte(`{"model":"claude-sonnet-4-5","max_tokens":64,"stream":false,` +
		`"metadata":{"user_id":"{\"device_id\":\"dev-native-retry\",\"account_uuid\":\"acct-native-retry\",\"session_id\":\"sess-native-retry\"}"},` +
		`"thinking":{"type":"enabled","budget_tokens":1024},` +
		`"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"` + nativeMessagesRetryUserText + `"}]},` +
		`{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"` + nativeMessagesRetryThinkingText + `","signature":"` + nativeMessagesRetrySignature + `"},` +
		`{"type":"text","text":"ack"}]},` +
		`{"role":"user","content":[{"type":"text","text":"continue"}]}` +
		`]}`)
}

// nativeMessagesRetryAccount 是 Anthropic OAuth 账号（Claude OAuth）：只有它会走
// 「403 同账号重试」这一分支（见 shouldRetryUpstreamError）。
func nativeMessagesRetryAccount() *Account {
	return &Account{
		ID:          821,
		Name:        "native-messages-retry-oauth",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":  "native-retry-access-token-sentinel",
			"refresh_token": "native-retry-refresh-token-sentinel",
		},
		Status:      StatusActive,
		Schedulable: true,
	}
}

// nativeMessagesRetryAckJSON 造一条覆盖当前语句版本的有效书面风险确认：门控的唯一判定点是
// 「存量布尔值 且 存在覆盖当前语句版本的有效确认」，缺了它即使布尔值为真也不会采集。
func nativeMessagesRetryAckJSON() string {
	payload, err := json.Marshal(ErrorDiagnosticRiskAcknowledgement{
		Version:     ErrorDiagnosticRiskAcknowledgementVersion,
		Phrase:      ErrorDiagnosticRiskAcknowledgementPhraseEN,
		AdminUserID: 7,
		AcceptedAt:  time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		panic(err)
	}
	return string(payload)
}

// nativeMessagesRetrySettingService 构造真实门控读取链路上的 SettingService：
// 诊断门控、书面确认、正文留存密钥与整流器开关都按生产语义给出。
func nativeMessagesRetrySettingService(extra map[string]string) *SettingService {
	values := map[string]string{
		SettingKeyErrorDiagnostic:                    `{"enabled":true,"risk_acknowledged":true,"body_retention_enabled":true}`,
		SettingKeyErrorDiagnosticRiskAcknowledgement: nativeMessagesRetryAckJSON(),
		SettingKeyRectifierSettings:                  `{"enabled":true,"thinking_signature_enabled":true,"thinking_budget_enabled":true}`,
	}
	for key, value := range extra {
		values[key] = value
	}
	return NewSettingService(&contentModerationTestSettingRepo{values: values}, &config.Config{
		Totp: config.TotpConfig{
			EncryptionKey:           nativeMessagesRetryEncryptionKey,
			EncryptionKeyConfigured: true,
		},
	})
}

// nativeMessagesRetryRecorder 是窄写入契约的测试替身；写入是异步的，读取要轮询。
type nativeMessagesRetryRecorder struct {
	mu       sync.Mutex
	attempts []ErrorDiagnosticAttempt
}

func (r *nativeMessagesRetryRecorder) RecordErrorDiagnostic(_ context.Context, attempt ErrorDiagnosticAttempt) (ErrorDiagnosticRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 观察者已把正文复制成自己拥有的字节；这里再复制一次，使断言与接缝的清零完全解耦。
	attempt.Body = append([]byte(nil), attempt.Body...)
	r.attempts = append(r.attempts, attempt)
	return ErrorDiagnosticRecord{ID: strings.Repeat("b", ErrorDiagnosticIDLength)}, nil
}

func (r *nativeMessagesRetryRecorder) snapshot() []ErrorDiagnosticAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ErrorDiagnosticAttempt(nil), r.attempts...)
}

// requireNativeMessagesRetryRecords 等待恰好 want 条诊断写入：少于预期说明接缝没被绑定或没
// 被调用，多于预期说明同一次发送被重复观察。
func (r *nativeMessagesRetryRecorder) requireRecords(t *testing.T, want int) []ErrorDiagnosticAttempt {
	t.Helper()
	require.Eventually(t, func() bool {
		return len(r.snapshot()) >= want
	}, 3*time.Second, 5*time.Millisecond, "expected %d diagnostic writes", want)
	attempts := r.snapshot()
	require.Len(t, attempts, want)
	return attempts
}

// nativeMessagesRetryUpstreamResponse 是一次「上游」应答。
type nativeMessagesRetryUpstreamResponse struct {
	status int
	body   string
}

// nativeMessagesRetryUpstream 忠实复刻 repository/http_upstream.go 的发送接缝顺序：
// 强制审计的发送前门 → 尝试序号 → 出站正文 tee → 真实 RoundTrip → 观察 4xx/5xx。
//
// 顺序本身就是断言对象：被发送前门拦下时，真实传输既不占尝试序号、也不读走一个字节、
// 更不会产生观察结果，本替身必须完全一致，否则「强制审计仍然在发出前阻断」会被测成假象。
type nativeMessagesRetryUpstream struct {
	// responses 按顺序消费；用尽后重复最后一项。
	responses []nativeMessagesRetryUpstreamResponse

	mu             sync.Mutex
	sends          int
	sentBodies     [][]byte
	observerBound  []bool
	preSendBlocked int
}

func newNativeMessagesRetryUpstream(responses ...nativeMessagesRetryUpstreamResponse) *nativeMessagesRetryUpstream {
	return &nativeMessagesRetryUpstream{responses: responses}
}

func (u *nativeMessagesRetryUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.send(req)
}

func (u *nativeMessagesRetryUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.send(req)
}

func (u *nativeMessagesRetryUpstream) send(req *http.Request) (*http.Response, error) {
	_, bound := httpattempt.DiagnosticObserverFromContext(req.Context())
	if err := httpattempt.BeforeRequest(req); err != nil {
		u.mu.Lock()
		u.preSendBlocked++
		u.observerBound = append(u.observerBound, bound)
		u.mu.Unlock()
		return nil, err
	}

	attempt := httpattempt.StartRequestAttempt(req)
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
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}

	u.mu.Lock()
	index := u.sends
	u.sends++
	u.sentBodies = append(u.sentBodies, payload)
	u.observerBound = append(u.observerBound, bound)
	if index >= len(u.responses) {
		index = len(u.responses) - 1
	}
	response := u.responses[index]
	u.mu.Unlock()

	resp := &http.Response{
		StatusCode: response.status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"x-request-id": []string{"rid-native-messages-retry"},
		},
		Body:    http.NoBody,
		Request: request,
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

func (u *nativeMessagesRetryUpstream) sent() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.sentBodies...)
}

func (u *nativeMessagesRetryUpstream) observers() []bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]bool(nil), u.observerBound...)
}

func (u *nativeMessagesRetryUpstream) blockedCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.preSendBlocked
}

func newNativeMessagesRetryGateway(upstream HTTPUpstream, settings *SettingService, recorder ErrorDiagnosticRecorder) *GatewayService {
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	svc := &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     &RateLimitService{},
		deferredService:      &DeferredService{},
		settingService:       settings,
	}
	svc.SetErrorDiagnosticRecorder(recorder)
	return svc
}

// newNativeMessagesRetryContext 造一个 Claude Code 客户端上下文：UA 与 metadata.user_id 同时
// 具备时被判定为真实 Claude Code 流量，OAuth 账号不再进入 mimicry 改写，
// 出站正文因此只由「整流重试」这一步决定，断言才落在被测行为上。
func newNativeMessagesRetryContext(t *testing.T, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("User-Agent", "claude-cli/2.0.31 (external, cli)")
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec
}

func newNativeMessagesRetryParsed(t *testing.T, body []byte) *ParsedRequest {
	t.Helper()
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)
	require.Equal(t, "claude-sonnet-4-5", parsed.Model)
	return parsed
}

// requireNativeMessagesRetryUsageAbsent 固定 wire 级诊断不是 usage-owned 记录：
// 它按「真实发往上游的尝试」成立，不挂使用记录，也不因该逻辑请求最终成功或失败而伪造关联
// （ADR 0002 只把请求审计挂到使用记录上；ADR 0005 让诊断独立于使用记录存在）。
func requireNativeMessagesRetryUsageAbsent(t *testing.T, attempts []ErrorDiagnosticAttempt) {
	t.Helper()
	require.NotEmpty(t, attempts)
	for i, attempt := range attempts {
		require.Zero(t, attempt.UsageLogID, "第 %d 条 wire 诊断不得伪造 usage 关联", i+1)
		require.Equal(t, ErrorDiagnosticStageWire, attempt.Stage, "第 %d 条诊断的阶段是真实发送接缝", i+1)
		require.Equal(t, ErrorDiagnosticProtocolMessages, attempt.Protocol, "协议按入站 /v1/messages 推导")
	}
}

// 400 thinking 签名不合法 → 整流重试：两次失败尝试各自成一条诊断，
// 且每条诊断的正文就是它那一次实际发出的字节（第二次是被改写过的正文）。
func TestNativeMessagesForwardSignatureRetryPairsEachAttemptWithActualOutboundBytes(t *testing.T) {
	body := nativeMessagesRetryBody()
	c, _ := newNativeMessagesRetryContext(t, body)
	recorder := &nativeMessagesRetryRecorder{}
	upstream := newNativeMessagesRetryUpstream(
		nativeMessagesRetryUpstreamResponse{status: http.StatusBadRequest, body: nativeMessagesRetrySignatureErrorBody},
		nativeMessagesRetryUpstreamResponse{status: http.StatusBadRequest, body: nativeMessagesRetrySignatureErrorBody},
	)
	svc := newNativeMessagesRetryGateway(upstream, nativeMessagesRetrySettingService(nil), recorder)

	result, err := svc.Forward(context.Background(), c, nativeMessagesRetryAccount(), newNativeMessagesRetryParsed(t, body))
	require.Error(t, err, "签名整流后仍失败时按原语义返回失败")
	require.Nil(t, result)

	sent := upstream.sent()
	require.Len(t, sent, 2, "400 签名错误触发一次整流重试：共两次真实发送")
	require.Equal(t, []bool{true, true}, upstream.observers(), "每次真实发送都必须显式绑定诊断观察者")

	// 第一次发送：原始正文，仍带签名 thinking block。
	require.Contains(t, string(sent[0]), `"type":"thinking"`)
	require.Contains(t, string(sent[0]), nativeMessagesRetryThinkingText)
	require.Contains(t, string(sent[0]), nativeMessagesRetrySignature)

	// 第二次发送：整流后的正文——顶层 thinking 被移除、thinking block 降级为 text 保留内容。
	require.NotEqual(t, string(sent[0]), string(sent[1]), "整流重试必须改写正文")
	require.NotContains(t, string(sent[1]), `"type":"thinking"`, "整流后不得再回传 thinking block")
	require.NotContains(t, string(sent[1]), nativeMessagesRetrySignature, "整流后不得再回传旧签名")
	require.NotContains(t, string(sent[1]), `"thinking":{"type":"enabled"`, "整流后必须关掉顶层 thinking")
	require.Contains(t, string(sent[1]), nativeMessagesRetryThinkingText,
		"整流只降级形态：thinking 正文必须以 text 保留，而不是被删掉")

	attempts := recorder.requireRecords(t, 2)
	requireNativeMessagesRetryUsageAbsent(t, attempts)

	for i, attempt := range attempts {
		require.Equal(t, i+1, attempt.AttemptIndex, "第 %d 次发送的尝试序号必须是它在真实发送顺序里的位置", i+1)
		require.Equal(t, http.StatusBadRequest, attempt.UpstreamStatusCode)
		require.True(t, attempt.BodyReadComplete)
		require.Equal(t, ErrorDiagnosticBodyVerdictComplete, attempt.BodyVerdict)
		require.Equal(t, string(sent[i]), string(attempt.Body),
			"第 %d 条诊断的正文必须等于该次实际发出的字节", i+1)
	}

	// 诊断保存的是**请求**（真实出站正文），不是上游错误响应。
	require.NotContains(t, string(attempts[0].Body), "invalid_request_error")
	require.NotContains(t, string(attempts[0].Body), "Field required")
}

// OAuth 账号的 403 走同账号重试：每次失败各占一个尝试序号、正文不被改写，
// 后续成功不覆盖先前失败尝试的诊断。
func TestNativeMessagesForwardOAuthForbiddenRetryKeepsOrdinalAndOutboundBytes(t *testing.T) {
	body := nativeMessagesRetryBody()
	c, _ := newNativeMessagesRetryContext(t, body)
	recorder := &nativeMessagesRetryRecorder{}
	successBody := `{"id":"msg_native_retry","type":"message","role":"assistant","model":"claude-sonnet-4-5",` +
		`"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":4}}`
	upstream := newNativeMessagesRetryUpstream(
		nativeMessagesRetryUpstreamResponse{status: http.StatusForbidden, body: `{"type":"error","error":{"type":"permission_error","message":"forbidden"}}`},
		nativeMessagesRetryUpstreamResponse{status: http.StatusForbidden, body: `{"type":"error","error":{"type":"permission_error","message":"forbidden"}}`},
		nativeMessagesRetryUpstreamResponse{status: http.StatusOK, body: successBody},
	)
	svc := newNativeMessagesRetryGateway(upstream, nativeMessagesRetrySettingService(nil), recorder)

	result, err := svc.Forward(context.Background(), c, nativeMessagesRetryAccount(), newNativeMessagesRetryParsed(t, body))
	require.NoError(t, err, "403 是 OAuth 账号唯一会重试的状态码，重试成功即正常返回")
	require.NotNil(t, result)
	require.Equal(t, 4, result.Usage.OutputTokens, "成功那次的上游用量照常带回")

	sent := upstream.sent()
	require.Len(t, sent, 3, "两次 403 各重试一次后成功：共三次真实发送")
	require.Equal(t, []bool{true, true, true}, upstream.observers())
	require.Equal(t, string(sent[0]), string(sent[1]), "403 重试不改写正文")

	attempts := recorder.requireRecords(t, 2)
	requireNativeMessagesRetryUsageAbsent(t, attempts)

	for i, attempt := range attempts {
		require.Equal(t, i+1, attempt.AttemptIndex, "失败尝试各占一个序号，成功的发送不占号")
		require.Equal(t, http.StatusForbidden, attempt.UpstreamStatusCode)
		require.Equal(t, string(sent[i]), string(attempt.Body),
			"第 %d 条诊断的正文必须等于该次实际发出的字节", i+1)
		require.NotContains(t, string(attempt.Body), "permission_error",
			"诊断不得退化成上游错误响应或用量事实")
	}
}

// 强制审计只在发出前阻断：被发送前门拦下的尝试不发出任何字节、不占尝试序号、也不产生观察结果，
// 诊断接缝的绑定不得把「从未发出」变成一条上游失败诊断（ADR 0004）。
func TestNativeMessagesForwardForcedAuditPreSendBlockStaysBlockedWithoutDiagnostics(t *testing.T) {
	body := nativeMessagesRetryBody()
	c, _ := newNativeMessagesRetryContext(t, body)

	settings := nativeMessagesRetrySettingService(map[string]string{
		SettingKeyRequestAuditForceMessages: "true",
	})
	recorder := &nativeMessagesRetryRecorder{}
	upstream := newNativeMessagesRetryUpstream(
		nativeMessagesRetryUpstreamResponse{status: http.StatusBadRequest, body: nativeMessagesRetrySignatureErrorBody},
	)
	svc := newNativeMessagesRetryGateway(upstream, settings, recorder)
	// 强制模式：本次尝试的协议元数据写不进去，就不允许发出。
	svc.requestAuditRepo = &nativeMessagesRetryForcedReservationRepo{reserveErr: errors.New("audit store down")}

	ctx, err := svc.PrepareRequestAudit(c.Request.Context(), c, RequestAuditRouteMessages)
	require.NoError(t, err)
	c.Request = c.Request.WithContext(ctx)

	result, err := svc.Forward(ctx, c, nativeMessagesRetryAccount(), newNativeMessagesRetryParsed(t, body))
	require.Nil(t, result)
	require.True(t, IsRequestAuditRequiredError(err), "强制审计必须仍然在发出前阻断该次发送")

	require.Equal(t, 1, upstream.blockedCount(), "阻断发生在真实发送之前")
	require.Empty(t, upstream.sent(), "被阻断的尝试不得发出任何字节")
	require.Equal(t, []bool{true}, upstream.observers(),
		"观察者确实已绑定在该次尝试上，因此「没有诊断」不是没绑定的假象")
	require.Empty(t, recorder.snapshot(), "从未发出的尝试不得产生诊断")
	require.Zero(t, RequestAuditHTTPAttemptCount(c), "被阻断的尝试不占尝试序号")
}

// nativeMessagesRetryForcedReservationRepo 是强制审计预留仓储的失败替身：
// 只要尝试元数据写不进去，强制模式就必须在发出前拒绝。
type nativeMessagesRetryForcedReservationRepo struct {
	RequestAuditRepository
	reserveErr error
}

func (r *nativeMessagesRetryForcedReservationRepo) ReserveAttempt(context.Context, RequestAuditReservationScope, RequestAuditAttempt) error {
	return r.reserveErr
}

func (r *nativeMessagesRetryForcedReservationRepo) FinalizeReservation(context.Context, string, int64, *RequestAuditRecord) error {
	return nil
}

func (r *nativeMessagesRetryForcedReservationRepo) MarkReservationIncomplete(context.Context, string, int64, string) error {
	return nil
}

func (r *nativeMessagesRetryForcedReservationRepo) DeleteExpiredReservations(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

// 门控与拓扑自检：改写断言所依赖的整流函数确实只降级形态，不会把 thinking 正文丢掉，
// 否则上面的「原正文含 thinking / 重试正文含 text 化正文」会退化成不可解释的字符串比较。
func TestNativeMessagesRetryFixtureRetryFilterKeepsThinkingContentAsText(t *testing.T) {
	body := nativeMessagesRetryBody()
	rewritten := FilterThinkingBlocksForRetry(body, "claude-sonnet-4-5")
	require.NotEqual(t, string(body), string(rewritten))
	require.NotContains(t, string(rewritten), `"type":"thinking"`)
	require.Contains(t, string(rewritten), nativeMessagesRetryThinkingText)
	require.False(t, bytes.Contains(rewritten, []byte(`"signature"`)))

	// 预过滤（发送前的 FilterThinkingBlocks）必须保留带有效签名的历史 thinking block，
	// 否则被测的 400 就不是「签名不合法」而是别的形态。
	prefiltered := FilterThinkingBlocks(body, "claude-sonnet-4-5")
	require.Equal(t, string(body), string(prefiltered))

	require.True(t, json.Valid(rewritten), "整流结果必须是合法 JSON")
	require.True(t, ShouldRectifyThinkingSignatureError("claude-sonnet-4-5"),
		"夹具模型必须落在 anthropic-strict 协议族，否则不会触发签名整流")
	require.True(t, shouldApplyNativeMessagesRetryFingerprint(body))
}

// shouldApplyNativeMessagesRetryFingerprint 报告夹具正文未被诊断分类判为不合格
// （含凭据或媒体部件），否则正文留存的断言会在分类阶段被静默跳过。
func shouldApplyNativeMessagesRetryFingerprint(body []byte) bool {
	classification := ClassifyErrorDiagnosticBody(body)
	return classification.IsTextJSON && !classification.HasKnownCredential && !classification.HasAttachment
}
