package service

import (
	"bufio"
	"context"
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

// 本文件只验证 openai_gateway_response_handling.go 的流式审计完整性：
// 上游在客户端已看到部分输出后异常终止（单行超长 / 读取失败）必须标记
// 「响应中途不完整」，并保留无 data 的事件骨架；正常终止仍为「完整」。

const openAIResponseIncompleteDelta = `{"type":"response.output_text.delta","delta":"partial output","usage":{"input_tokens":7,"output_tokens":4}}`

type openAIResponseStreamIncompleteReadError struct {
	payload  []byte
	err      error
	consumed bool
}

func (r *openAIResponseStreamIncompleteReadError) Read(data []byte) (int, error) {
	if !r.consumed {
		r.consumed = true
		return copy(data, r.payload), nil
	}
	return 0, r.err
}

func (r *openAIResponseStreamIncompleteReadError) Close() error { return nil }

func runOpenAIResponseStreamIncompleteTest(t *testing.T, body io.ReadCloser, gatewayCfg config.GatewayConfig) (*openaiStreamingResult, string, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	svc := &OpenAIGatewayService{
		cfg:           &config.Config{Gateway: gatewayCfg},
		toolCorrector: NewCodexToolCorrector(),
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	result, err := svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "gpt-5", "gpt-5")
	return result, recorder.Body.String(), err
}

func requireOpenAIResponseAuditSSESkeletonIsDataFree(t *testing.T, events []RequestAuditSSEEvent) {
	t.Helper()
	require.NotEmpty(t, events, "partial output must still leave an event skeleton for the audit")
	for _, ev := range events {
		require.Empty(t, ev.Data, "audit skeleton must not retain event text")
	}
	for _, skeleton := range buildRequestAuditEventSkeletons(events) {
		require.NotEmpty(t, skeleton.Type, "every skeleton keeps its event type")
		require.NotContains(t, skeleton.Type, "partial output", "skeleton types never carry model text")
	}
}

func TestOpenAIResponseStreamIncomplete_TooLongLineAfterOutputMarksAuditIncomplete(t *testing.T) {
	delta := "data: " + openAIResponseIncompleteDelta + "\n\n"
	// 扫描缓冲为 64 KiB；单行超过 MaxLineSize 且无换行即触发 bufio.ErrTooLong。
	tooLong := "data: " + strings.Repeat("a", 128*1024) + "\n"

	result, clientBody, err := runOpenAIResponseStreamIncompleteTest(
		t,
		io.NopCloser(strings.NewReader(delta+tooLong)),
		config.GatewayConfig{MaxLineSize: 64 * 1024},
	)

	require.ErrorIs(t, err, bufio.ErrTooLong)
	require.NotNil(t, result)
	require.True(t, result.streamIncomplete,
		"an over-long upstream line after partial output must mark the audit 响应中途不完整")
	// 已观测到的部分用量与骨架仍要送达审计
	require.Equal(t, 7, result.usage.InputTokens)
	require.Equal(t, 4, result.usage.OutputTokens)
	requireOpenAIResponseAuditSSESkeletonIsDataFree(t, result.sseEvents)

	skeletons := buildRequestAuditEventSkeletons(result.sseEvents)
	require.Equal(
		t,
		RequestAuditCaptureIncomplete,
		requestAuditCaptureCompleteness(RequestAuditInput{
			SSEEvents:        result.sseEvents,
			StreamIncomplete: result.streamIncomplete,
		}, skeletons),
	)
	// 客户端已收到部分输出，并收到协议错误事件收尾；客户端流不被掐断。
	require.Contains(t, clientBody, "partial output")
	require.Contains(t, clientBody, "response_too_large")
}

func TestOpenAIResponseStreamIncomplete_ReadErrorAfterOutputMarksAuditIncomplete(t *testing.T) {
	delta := "data: " + openAIResponseIncompleteDelta + "\n\n"

	result, clientBody, err := runOpenAIResponseStreamIncompleteTest(
		t,
		&openAIResponseStreamIncompleteReadError{payload: []byte(delta), err: io.ErrUnexpectedEOF},
		config.GatewayConfig{MaxLineSize: defaultMaxLineSize},
	)

	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.NotNil(t, result)
	require.True(t, result.streamIncomplete,
		"an upstream read failure after partial output must mark the audit 响应中途不完整")
	require.Equal(t, 7, result.usage.InputTokens)
	requireOpenAIResponseAuditSSESkeletonIsDataFree(t, result.sseEvents)
	require.Contains(t, clientBody, "partial output")
}

func TestOpenAIResponseStreamIncomplete_TerminalEventStaysAuditComplete(t *testing.T) {
	delta := "data: " + openAIResponseIncompleteDelta + "\n\n"
	terminal := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[]," +
		"\"usage\":{\"input_tokens\":7,\"output_tokens\":4}}}\n\n"

	result, _, err := runOpenAIResponseStreamIncompleteTest(
		t,
		io.NopCloser(strings.NewReader(delta+terminal)),
		config.GatewayConfig{MaxLineSize: 64 * 1024},
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.streamIncomplete, "terminal event followed by EOF is a complete stream")
	require.Equal(t, 7, result.usage.InputTokens)
	requireOpenAIResponseAuditSSESkeletonIsDataFree(t, result.sseEvents)
	require.Equal(
		t,
		RequestAuditCaptureComplete,
		requestAuditCaptureCompleteness(RequestAuditInput{
			SSEEvents:        result.sseEvents,
			StreamIncomplete: result.streamIncomplete,
		}, buildRequestAuditEventSkeletons(result.sseEvents)),
	)
}

const openAIResponseStreamIncompleteTerminal = `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],` +
	`"usage":{"input_tokens":7,"output_tokens":4}}}`

const openAIResponseStreamIncompleteFailed = `{"type":"response.failed","response":{"id":"resp_1","status":"failed","output":[],` +
	`"error":{"code":"upstream_error","message":"boom"}}}`

// openAIResponseStreamStalledBody 复现「上游在完整刷出终态事件后拖延关闭连接」：正文只
// 交付一次，之后的读取一直阻塞，既不返回 EOF 也不返回错误，直到测试收尾放行。
type openAIResponseStreamStalledBody struct {
	payload []byte
	release chan struct{}
	once    sync.Once
	served  atomic.Bool
}

func newOpenAIResponseStreamStalledBody(payload string) *openAIResponseStreamStalledBody {
	return &openAIResponseStreamStalledBody{payload: []byte(payload), release: make(chan struct{})}
}

func (b *openAIResponseStreamStalledBody) Read(data []byte) (int, error) {
	if b.served.CompareAndSwap(false, true) {
		return copy(data, b.payload), nil
	}
	<-b.release
	return 0, io.EOF
}

func (b *openAIResponseStreamStalledBody) Close() error { return nil }

func (b *openAIResponseStreamStalledBody) unblock() {
	b.once.Do(func() { close(b.release) })
}

// seedOpenAIResponseStreamAttempt 按生产顺序登记最近一次上游尝试：传输层先记状态/头/
// 有正文（此时读取尚未完成），服务层随后才可能补记终态读取结论。
func seedOpenAIResponseStreamAttempt(t *testing.T, c *gin.Context) (*httpattempt.Counter, *httpattempt.Attempt) {
	t.Helper()
	counterCtx := WithRequestAuditHTTPAttemptCounter(c.Request.Context(), c)
	attempt := httpattempt.StartAttempt(httpattempt.WithMetadata(counterCtx, httpattempt.Metadata{
		AccountID: 1,
		Model:     "gpt-5",
		Protocol:  RequestAuditProtocolOpenAIResp,
	}))
	require.NotNil(t, attempt)
	attempt.SetResponse(http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}}, true)
	counter, ok := httpattempt.FromContext(counterCtx)
	require.True(t, ok, "请求审计计数器必须挂在 gin 上下文上")
	return counter, attempt
}

// runOpenAIResponseStreamAttemptReadTest 在请求审计计数器上登记最近一次上游尝试后执行流式
// 处理。countedBody 为 true 时正文接到该尝试上（生产形态）：读到 EOF 记「已完整读取」，
// EOF 之前 Close 记「读取不完整」，因此能区分终态收尾与截断。
func runOpenAIResponseStreamAttemptReadTest(
	t *testing.T,
	respBody io.ReadCloser,
	gatewayCfg config.GatewayConfig,
	countedBody bool,
) (*openaiStreamingResult, *httpattempt.Counter, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	counter, attempt := seedOpenAIResponseStreamAttempt(t, c)
	if countedBody {
		respBody = httpattempt.NewResponseBody(respBody, attempt)
	}

	svc := &OpenAIGatewayService{
		cfg:           &config.Config{Gateway: gatewayCfg},
		toolCorrector: NewCodexToolCorrector(),
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       respBody,
	}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	result, err := svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "gpt-5", "gpt-5")
	// 生产路径由调用方在 handler 返回后关闭上游正文；这里同样补上，复现
	// 「提前收尾 → 随后的 Close」顺序。
	require.NoError(t, resp.Body.Close())
	return result, counter, err
}

// 上游在完整刷出 response.completed 后不发送 EOF（连接保持打开）时，服务层会提前停止读取
// 并 Close 正文；最近一次尝试仍必须记成「已完整读取」，审计不能把完整响应显示成读取不完整。
// 超时/keepalive 全为 0 时走同步扫描，否则走独立读取循环，两条路径都要补记终态。
func TestOpenAIResponseStreamIncomplete_TerminalWithoutEOFKeepsAttemptReadComplete(t *testing.T) {
	cases := map[string]config.GatewayConfig{
		"同步扫描":   {MaxLineSize: 64 * 1024},
		"异步读取循环": {MaxLineSize: 64 * 1024, StreamKeepaliveInterval: 1},
	}

	for name, gatewayCfg := range cases {
		t.Run(name, func(t *testing.T) {
			payload := "data: " + openAIResponseIncompleteDelta + "\n\n" +
				"data: " + openAIResponseStreamIncompleteTerminal + "\n\n"
			body := newOpenAIResponseStreamStalledBody(payload)
			t.Cleanup(body.unblock)

			result, counter, err := runOpenAIResponseStreamAttemptReadTest(t, body, gatewayCfg, true)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, result.streamIncomplete, "终态事件已完整刷出即视为完整流")
			require.Equal(t, 7, result.usage.InputTokens)

			got := counter.Metadata()
			require.Len(t, got, 1)
			require.NotNil(t, got[0].ResponseReadComplete)
			require.True(t, *got[0].ResponseReadComplete,
				"终态事件后的提前 Close 属于正常收尾，不得记成读取不完整")
			require.Equal(t, int64(len(payload)), *got[0].ResponseBytes, "服务层只补记读取结论，不碰体量")
		})
	}
}

// 整段上游流都没有终态事件时不得补记读取结论：那是截断，不是正常收尾。
func TestOpenAIResponseStreamIncomplete_MissingTerminalDoesNotMarkAttemptReadComplete(t *testing.T) {
	result, counter, err := runOpenAIResponseStreamAttemptReadTest(
		t,
		io.NopCloser(strings.NewReader("data: "+openAIResponseIncompleteDelta+"\n\n")),
		config.GatewayConfig{MaxLineSize: defaultMaxLineSize},
		false,
	)

	require.Error(t, err)
	require.NotNil(t, result)
	require.True(t, result.streamIncomplete, "缺终态事件的干净 EOF 仍是响应中途不完整")
	got := counter.Metadata()
	require.Len(t, got, 1)
	require.NotNil(t, got[0].ResponseReadComplete)
	require.False(t, *got[0].ResponseReadComplete, "截断流不得补记成已完整读取")
}

// Codex bare error 由服务层补写 response.failed（服务自己的辅助发送），不是成功收尾，
// 同样不得补记成已完整读取。
func TestOpenAIResponseStreamIncomplete_BareErrorTerminalDoesNotMarkAttemptReadComplete(t *testing.T) {
	payload := "data: " + openAIResponseIncompleteDelta + "\n\n" +
		"data: {\"type\":\"error\",\"code\":\"upstream_error\",\"message\":\"boom\"}\n\n"

	result, counter, _ := runOpenAIResponseStreamAttemptReadTest(
		t,
		io.NopCloser(strings.NewReader(payload)),
		config.GatewayConfig{MaxLineSize: defaultMaxLineSize},
		false,
	)

	require.NotNil(t, result)
	got := counter.Metadata()
	require.Len(t, got, 1)
	require.NotNil(t, got[0].ResponseReadComplete)
	require.False(t, *got[0].ResponseReadComplete,
		"bare error 的辅助发送属于断流，不得补记成已完整读取")
}

// 失败终态（response.failed）的提前结束是断流，同样不得补记成已完整读取。
func TestOpenAIResponseStreamIncomplete_FailedTerminalDoesNotMarkAttemptReadComplete(t *testing.T) {
	payload := "data: " + openAIResponseIncompleteDelta + "\n\n" +
		"data: " + openAIResponseStreamIncompleteFailed + "\n\n"

	result, counter, err := runOpenAIResponseStreamAttemptReadTest(
		t,
		io.NopCloser(strings.NewReader(payload)),
		config.GatewayConfig{MaxLineSize: defaultMaxLineSize},
		false,
	)

	require.Error(t, err)
	require.NotNil(t, result)
	got := counter.Metadata()
	require.Len(t, got, 1)
	require.NotNil(t, got[0].ResponseReadComplete)
	require.False(t, *got[0].ResponseReadComplete,
		"失败终态属于断流，不得补记成已完整读取")
}
