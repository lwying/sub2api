package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 本文件只验证 openai_gateway_passthrough.go 的流式审计完整性：
// 上游在客户端已看到部分输出后异常终止（读错误 / 干净 EOF 缺终态）必须标记
// 「响应中途不完整」（OpenAIForwardResult.StreamIncomplete），把标记传给请求
// 审计，并保留无 data 的事件骨架与已观测 usage；正常收到终态事件仍为「完整」。
// 下游已写出的字节与既有计费/错误语义不变。

// openAIPassthroughIncompleteBodySentinel 是上游 SSE data 里的正文标记：审计骨架
// 只保留类型/序号/字节/指纹，任何地方出现该子串都说明正文被落进了审计记录。
const openAIPassthroughIncompleteBodySentinel = "OPENAI_PASSTHROUGH_AUDIT_BODY_SENTINEL"

// openAIPassthroughIncompleteDelta 携带部分 usage：上游已计量，流中断也不得丢。
const openAIPassthroughIncompleteDelta = `{"type":"response.output_text.delta","delta":"` +
	openAIPassthroughIncompleteBodySentinel + `","usage":{"input_tokens":7,"output_tokens":4}}`

const openAIPassthroughIncompleteCreated = `{"type":"response.created","response":{"id":"resp_passthrough_incomplete","model":"gpt-5.4","status":"in_progress","output":[]}}`

const openAIPassthroughIncompleteCompleted = `{"type":"response.completed","response":{"id":"resp_passthrough_incomplete","object":"response","model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}}`

// openAIPassthroughPartialStream 是缺少终态事件的上游 SSE：客户端已看到 delta，
// 上游随后干净收尾（或读错误），整段流没有任何 terminal。
func openAIPassthroughPartialStream() string {
	return strings.Join([]string{
		"data: " + openAIPassthroughIncompleteCreated,
		"",
		"data: " + openAIPassthroughIncompleteDelta,
		"",
	}, "\n")
}

// openAIPassthroughTerminalStream 在同样的前缀后补上终态事件与 [DONE]。
func openAIPassthroughTerminalStream() string {
	return openAIPassthroughPartialStream() + strings.Join([]string{
		"data: " + openAIPassthroughIncompleteCompleted,
		"",
		"data: [DONE]",
		"",
	}, "\n")
}

type openAIPassthroughIncompleteReadError struct {
	payload  []byte
	err      error
	consumed bool
}

func (r *openAIPassthroughIncompleteReadError) Read(data []byte) (int, error) {
	if !r.consumed {
		r.consumed = true
		return copy(data, r.payload), nil
	}
	return 0, r.err
}

func (r *openAIPassthroughIncompleteReadError) Close() error { return nil }

func runOpenAIPassthroughIncompleteTest(
	t *testing.T,
	ctx context.Context,
	body io.ReadCloser,
	setup func(*gin.Context),
) (*openaiStreamingResultPassthrough, *httptest.ResponseRecorder, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if setup != nil {
		setup(c)
	}

	svc := &OpenAIGatewayService{cfg: &config.Config{
		Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize},
	}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"x-request-id": []string{"rid_passthrough_incomplete"},
		},
		Body: body,
	}
	result, err := svc.handleStreamingResponsePassthrough(
		ctx,
		resp,
		c,
		&Account{ID: 1, Platform: PlatformOpenAI, Name: "passthrough-incomplete"},
		time.Now(),
		"gpt-5.4",
		"gpt-5.4",
	)
	return result, recorder, err
}

// requireOpenAIPassthroughAuditSkeletonIsDataFree 断言骨架只留类型/序号/字节/指纹。
func requireOpenAIPassthroughAuditSkeletonIsDataFree(t *testing.T, events []RequestAuditSSEEvent) {
	t.Helper()
	require.NotEmpty(t, events, "partial output must still leave an event skeleton for the audit")
	for _, ev := range events {
		require.Empty(t, ev.Data, "audit skeleton must not retain event text")
	}
	for _, skeleton := range buildRequestAuditEventSkeletons(events) {
		require.NotEmpty(t, skeleton.Type, "every skeleton keeps its event type")
		require.NotContains(t, skeleton.Type, openAIPassthroughIncompleteBodySentinel,
			"skeleton types never carry model text")
	}
}

// requireOpenAIPassthroughAuditCompleteness 走真实审计落库路径断言采集完整性，
// 并确认落库记录里没有模型正文。
func requireOpenAIPassthroughAuditCompleteness(
	t *testing.T,
	result *openaiStreamingResultPassthrough,
	want string,
	usageLogID int64,
) {
	t.Helper()
	repo := &stubRequestAuditRepo{}
	require.NoError(t, AttachRequestAuditAfterUsageLog(context.Background(), repo, &UsageLog{ID: usageLogID},
		RequestAuditInput{
			SSEEvents:        result.sseEvents,
			ClientDisconnect: result.clientDisconnect,
			StreamIncomplete: result.streamIncomplete,
		}))
	require.NotNil(t, repo.created)
	require.Equal(t, want, repo.created.CaptureCompleteness)
	require.NotEmpty(t, repo.created.Events)
	encoded, err := json.Marshal(repo.created)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), openAIPassthroughIncompleteBodySentinel,
		"request audit must not retain the model body")
}

func TestOpenAIPassthroughStreamIncomplete_ReadErrorAfterPartialOutputIsIncomplete(t *testing.T) {
	partial := openAIPassthroughPartialStream()

	result, recorder, err := runOpenAIPassthroughIncompleteTest(
		t,
		context.Background(),
		&openAIPassthroughIncompleteReadError{payload: []byte(partial), err: io.ErrUnexpectedEOF},
		nil,
	)

	require.Error(t, err)
	require.Contains(t, err.Error(), "stream read error")
	require.NotNil(t, result)
	require.True(t, result.streamIncomplete,
		"an upstream read failure after partial output must mark the audit 响应中途不完整")
	// 上游已计量的部分 usage 与骨架照常带出，供使用记录与审计使用。
	require.Equal(t, 7, result.usage.InputTokens)
	require.Equal(t, 4, result.usage.OutputTokens)
	requireOpenAIPassthroughAuditSkeletonIsDataFree(t, result.sseEvents)
	requireOpenAIPassthroughAuditCompleteness(t, result, RequestAuditCaptureIncomplete, 11)
	// 下游已写出的部分输出不被追回，客户端流不被掐断。
	require.Contains(t, recorder.Body.String(), openAIPassthroughIncompleteBodySentinel)
}

func TestOpenAIPassthroughStreamIncomplete_MissingTerminalEventIsIncomplete(t *testing.T) {
	result, recorder, err := runOpenAIPassthroughIncompleteTest(
		t,
		context.Background(),
		io.NopCloser(strings.NewReader(openAIPassthroughPartialStream())),
		nil,
	)

	require.Error(t, err)
	require.Contains(t, err.Error(), "missing terminal event")
	require.NotNil(t, result)
	require.True(t, result.streamIncomplete,
		"a clean EOF without any terminal event must mark the audit 响应中途不完整")
	require.Equal(t, 7, result.usage.InputTokens)
	requireOpenAIPassthroughAuditSkeletonIsDataFree(t, result.sseEvents)
	requireOpenAIPassthroughAuditCompleteness(t, result, RequestAuditCaptureIncomplete, 12)
	require.Contains(t, recorder.Body.String(), openAIPassthroughIncompleteBodySentinel)
}

// 干净 EOF 缺终态 + 请求上下文已结束（如客户端断开后排水）时，执行流按既有语义
// 成功返回部分结果；此时使用记录上的请求审计不得记成「完整」。
func TestOpenAIPassthroughStreamIncomplete_NonterminalCleanEOFFallthroughIsIncomplete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, recorder, err := runOpenAIPassthroughIncompleteTest(
		t,
		ctx,
		io.NopCloser(strings.NewReader(openAIPassthroughPartialStream())),
		nil,
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.streamIncomplete,
		"a nonterminal clean EOF returned as a partial result must not be audited as complete")
	require.False(t, result.clientDisconnect, "上游缺终态不等于客户端主动断开")
	require.Equal(t, 7, result.usage.InputTokens)
	requireOpenAIPassthroughAuditSkeletonIsDataFree(t, result.sseEvents)
	requireOpenAIPassthroughAuditCompleteness(t, result, RequestAuditCaptureIncomplete, 13)
	require.Contains(t, recorder.Body.String(), openAIPassthroughIncompleteBodySentinel)
}

// 客户端主动断开由 ClientDisconnect 单独标记（审计侧同样是不完整），
// 不伪装成上游截断。
func TestOpenAIPassthroughStreamIncomplete_ClientDisconnectIsNotUpstreamTruncation(t *testing.T) {
	result, _, err := runOpenAIPassthroughIncompleteTest(
		t,
		context.Background(),
		io.NopCloser(strings.NewReader(openAIPassthroughPartialStream())),
		func(c *gin.Context) { c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer} },
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.clientDisconnect, "下游写入失败即客户端断开")
	require.False(t, result.streamIncomplete,
		"客户端主动断开不得伪装成上游截断（由 ClientDisconnect 单独标记）")
	requireOpenAIPassthroughAuditCompleteness(t, result, RequestAuditCaptureIncomplete, 14)
}

func TestOpenAIPassthroughStreamIncomplete_TerminalEventStaysComplete(t *testing.T) {
	result, recorder, err := runOpenAIPassthroughIncompleteTest(
		t,
		context.Background(),
		io.NopCloser(strings.NewReader(openAIPassthroughTerminalStream())),
		nil,
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.streamIncomplete, "terminal event followed by EOF is a complete stream")
	require.Equal(t, 7, result.usage.InputTokens)
	requireOpenAIPassthroughAuditSkeletonIsDataFree(t, result.sseEvents)
	requireOpenAIPassthroughAuditCompleteness(t, result, RequestAuditCaptureComplete, 15)
	// 已刷出的终态事件足以结束流；上游随后发送的 [DONE] 不必等待。
	require.Contains(t, recorder.Body.String(), openAIPassthroughIncompleteBodySentinel)
	require.Contains(t, recorder.Body.String(), "data: "+openAIPassthroughIncompleteCompleted)
}

// 不完整标记必须一路传到 OpenAIForwardResult.StreamIncomplete：使用记录的请求
// 审计从该结果取完整性，漏传就会把截断流记成「完整」。
func TestOpenAIPassthroughStreamIncomplete_PropagatesToForwardResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.1.0")
	ctx, cancel := context.WithCancel(c.Request.Context())
	cancel()
	c.Request = c.Request.WithContext(ctx)

	body := []byte(`{"model":"gpt-5.4","stream":true,"input":[{"type":"text","text":"hi"}]}`)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid-passthrough-incomplete"}},
		Body:       io.NopCloser(strings.NewReader(openAIPassthroughPartialStream())),
	}}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:             7,
		Name:           "passthrough-incomplete",
		Platform:       PlatformOpenAI,
		Type:           AccountTypeOAuth,
		Concurrency:    1,
		Credentials:    map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-acc"},
		Extra:          map[string]any{"openai_passthrough": true},
		Status:         StatusActive,
		Schedulable:    true,
		RateMultiplier: f64p(1),
	}

	result, err := svc.forwardOpenAIPassthrough(
		ctx, c, account, body, body, "gpt-5.4", false, nil, true, time.Now(),
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.StreamIncomplete,
		"a partial result without a terminal event must carry StreamIncomplete into the linked request audit")
	require.False(t, result.ClientDisconnect)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.NotEmpty(t, result.SSEEvents)
	requireOpenAIPassthroughAuditSkeletonIsDataFree(t, result.SSEEvents)
	require.Equal(
		t,
		RequestAuditCaptureIncomplete,
		requestAuditCaptureCompleteness(RequestAuditInput{
			SSEEvents:        requestAuditSSEEventsFromOpenAIResult(result),
			ClientDisconnect: result.ClientDisconnect,
			StreamIncomplete: result.StreamIncomplete,
		}, buildRequestAuditEventSkeletons(requestAuditSSEEventsFromOpenAIResult(result))),
	)
	// 下游透传输出不受影响。
	require.Contains(t, recorder.Body.String(), openAIPassthroughIncompleteBodySentinel)
}

// openAIPassthroughStalledBody 复现「上游在完整刷出终态事件后拖延关闭连接」：正文只交付
// 一次，之后的读取一直阻塞，既不返回 EOF 也不返回错误，直到测试收尾放行。
type openAIPassthroughStalledBody struct {
	payload []byte
	release chan struct{}
	once    sync.Once
	served  atomic.Bool
}

func newOpenAIPassthroughStalledBody(payload string) *openAIPassthroughStalledBody {
	return &openAIPassthroughStalledBody{payload: []byte(payload), release: make(chan struct{})}
}

func (b *openAIPassthroughStalledBody) Read(data []byte) (int, error) {
	if b.served.CompareAndSwap(false, true) {
		return copy(data, b.payload), nil
	}
	<-b.release
	return 0, io.EOF
}

func (b *openAIPassthroughStalledBody) Close() error { return nil }

func (b *openAIPassthroughStalledBody) unblock() {
	b.once.Do(func() { close(b.release) })
}

// runOpenAIPassthroughAttemptReadTest 在请求审计计数器上按生产顺序登记最近一次上游尝试
// （状态/头/有正文 → 读取尚未完成），并把正文接到该尝试上，复现「提前 break → 调用方
// 随后 Close 正文」的顺序。
func runOpenAIPassthroughAttemptReadTest(
	t *testing.T,
	stalled *openAIPassthroughStalledBody,
) (*openaiStreamingResultPassthrough, *httpattempt.Counter, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	counterCtx := WithRequestAuditHTTPAttemptCounter(c.Request.Context(), c)
	attempt := httpattempt.StartAttempt(httpattempt.WithMetadata(counterCtx, httpattempt.Metadata{
		AccountID: 1,
		Model:     "gpt-5.4",
		Protocol:  RequestAuditProtocolOpenAIResp,
	}))
	require.NotNil(t, attempt)
	attempt.SetResponse(http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}}, true)
	counter, ok := httpattempt.FromContext(counterCtx)
	require.True(t, ok, "请求审计计数器必须挂在 gin 上下文上")

	svc := &OpenAIGatewayService{cfg: &config.Config{
		Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize},
	}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"x-request-id": []string{"rid_passthrough_attempt_read"},
		},
		Body: httpattempt.NewResponseBody(stalled, attempt),
	}
	result, err := svc.handleStreamingResponsePassthrough(
		context.Background(),
		resp,
		c,
		&Account{ID: 1, Platform: PlatformOpenAI, Name: "passthrough-attempt-read"},
		time.Now(),
		"gpt-5.4",
		"gpt-5.4",
	)
	// 生产路径由 forwardOpenAIPassthrough 的 defer 在 handler 返回后关闭上游正文。
	require.NoError(t, resp.Body.Close())
	return result, counter, err
}

// 上游在完整刷出终态事件后不发送 EOF 时，透传路径提前结束读取；最近一次尝试必须记成
// 「已完整读取」，随后的正文 Close 不得把它降级成读取不完整。
func TestOpenAIPassthroughStreamIncomplete_TerminalWithoutEOFKeepsAttemptReadComplete(t *testing.T) {
	stalled := newOpenAIPassthroughStalledBody(openAIPassthroughTerminalStream())
	t.Cleanup(stalled.unblock)

	result, counter, err := runOpenAIPassthroughAttemptReadTest(t, stalled)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.streamIncomplete, "终态事件已完整刷出即视为完整流")
	require.Equal(t, 7, result.usage.InputTokens)

	got := counter.Metadata()
	require.Len(t, got, 1)
	require.NotNil(t, got[0].ResponseReadComplete)
	require.True(t, *got[0].ResponseReadComplete,
		"终态事件后的提前 Close 属于正常收尾，不得记成读取不完整")
	require.Equal(t, int64(len(openAIPassthroughTerminalStream())), *got[0].ResponseBytes,
		"服务层只补记读取结论，不碰体量")
}

// 缺终态事件的截断流不得补记读取结论：那条路径上正文是被截断的，不是正常收尾。
func TestOpenAIPassthroughStreamIncomplete_MissingTerminalDoesNotMarkAttemptReadComplete(t *testing.T) {
	var counter *httpattempt.Counter
	result, _, err := runOpenAIPassthroughIncompleteTest(
		t,
		context.Background(),
		io.NopCloser(strings.NewReader(openAIPassthroughPartialStream())),
		func(c *gin.Context) {
			counterCtx := WithRequestAuditHTTPAttemptCounter(c.Request.Context(), c)
			attempt := httpattempt.StartAttempt(httpattempt.WithMetadata(counterCtx, httpattempt.Metadata{
				AccountID: 1,
				Model:     "gpt-5.4",
				Protocol:  RequestAuditProtocolOpenAIResp,
			}))
			require.NotNil(t, attempt)
			attempt.SetResponse(http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}}, true)
			counter, _ = httpattempt.FromContext(counterCtx)
		},
	)

	require.Error(t, err)
	require.Contains(t, err.Error(), "missing terminal event")
	require.NotNil(t, result)
	require.True(t, result.streamIncomplete)
	require.NotNil(t, counter)
	got := counter.Metadata()
	require.Len(t, got, 1)
	require.NotNil(t, got[0].ResponseReadComplete)
	require.False(t, *got[0].ResponseReadComplete, "截断流不得补记成已完整读取")
}
