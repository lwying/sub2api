package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 本文件只验证 openai_gateway_chat_completions.go（Responses→Chat 转换流）的审计
// 完整性：客户端已看到部分输出后上游异常终止（读错误 / 终态前干净 EOF）必须把
// 「响应中途不完整」标记带进 OpenAIForwardResult.StreamIncomplete，由使用记录上的
// 请求审计落成「响应中途不完整」，而不是看起来一次正常收尾的流。
// 客户端主动断开与终端错误事件不属于上游截断，不得置位；正常收到终态事件仍为「完整」。
// 已写出的字节与既有计费语义不变，审计骨架只留类型/序号/体量/指纹，不含 delta 正文。

// openAIChatIncompleteBodySentinel 是上游 SSE data 里的正文标记：任何地方出现该子串
// 都说明模型正文被落进了审计记录。
const openAIChatIncompleteBodySentinel = "OPENAI_CHAT_COMPLETIONS_AUDIT_BODY_SENTINEL"

// openAIChatIncompleteDelta 携带部分 usage：上游已计量，流中断也不得丢。
const openAIChatIncompleteDelta = `{"type":"response.output_text.delta","delta":"` +
	openAIChatIncompleteBodySentinel + `","usage":{"input_tokens":7,"output_tokens":4}}`

const openAIChatIncompleteCreated = `{"type":"response.created","response":{"id":"resp_chat_incomplete","model":"gpt-5.4","status":"in_progress","output":[]}}`

const openAIChatIncompleteCompleted = `{"type":"response.completed","response":{"id":"resp_chat_incomplete","object":"response","model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}}`

// openAIChatPartialStream 是缺少终态事件的上游 SSE：客户端已看到 delta，
// 上游随后干净收尾（或读错误），整段流没有任何 terminal。
// 每个帧都以空行结束，这样读错误发生前最后一条 delta 已完整交给转换逻辑。
func openAIChatPartialStream() string {
	return "data: " + openAIChatIncompleteCreated + "\n\n" +
		"data: " + openAIChatIncompleteDelta + "\n\n"
}

// openAIChatTerminalStream 在同样的前缀后补上终态事件与 [DONE]。
func openAIChatTerminalStream() string {
	return openAIChatPartialStream() +
		"data: " + openAIChatIncompleteCompleted + "\n\n" +
		"data: [DONE]\n\n"
}

type openAIChatIncompleteReadError struct {
	payload  []byte
	err      error
	consumed bool
}

func (r *openAIChatIncompleteReadError) Read(data []byte) (int, error) {
	if !r.consumed {
		r.consumed = true
		return copy(data, r.payload), nil
	}
	return 0, r.err
}

func (r *openAIChatIncompleteReadError) Close() error { return nil }

func runOpenAIChatStreamIncompleteTest(
	t *testing.T,
	body io.ReadCloser,
	cfg *config.Config,
	setup func(*gin.Context),
) (*OpenAIForwardResult, *httptest.ResponseRecorder, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	if cfg == nil {
		cfg = &config.Config{}
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if setup != nil {
		setup(c)
	}

	svc := &OpenAIGatewayService{cfg: cfg}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"x-request-id": []string{"rid_chat_incomplete"},
		},
		Body: body,
	}
	result, err := svc.handleChatStreamingResponse(
		resp,
		c,
		&Account{ID: 1, Name: "openai-oauth", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1},
		"gpt-5.4",
		"gpt-5.4",
		"gpt-5.4",
		time.Now(),
		0,
	)
	return result, recorder, err
}

// requireOpenAIChatAuditSkeletonIsDataFree 断言骨架只留类型/序号/字节/指纹。
func requireOpenAIChatAuditSkeletonIsDataFree(t *testing.T, events []RequestAuditSSEEvent) {
	t.Helper()
	require.NotEmpty(t, events, "partial output must still leave an event skeleton for the audit")
	for _, ev := range events {
		require.Empty(t, ev.Data, "audit skeleton must not retain event text")
		require.Positive(t, ev.Bytes, "audit skeleton keeps the event size")
	}
	for _, skeleton := range buildRequestAuditEventSkeletons(events) {
		require.NotEmpty(t, skeleton.Type, "every skeleton keeps its event type")
		require.NotContains(t, skeleton.Type, openAIChatIncompleteBodySentinel,
			"skeleton types never carry model text")
	}
}

// requireOpenAIChatAuditCompleteness 走真实审计落库路径断言采集完整性，
// 并确认落库记录里没有模型正文。
func requireOpenAIChatAuditCompleteness(
	t *testing.T,
	result *OpenAIForwardResult,
	want string,
	usageLogID int64,
) {
	t.Helper()
	repo := &stubRequestAuditRepo{}
	require.NoError(t, AttachRequestAuditAfterUsageLog(context.Background(), repo, &UsageLog{ID: usageLogID},
		RequestAuditInput{
			SSEEvents:        requestAuditSSEEventsFromOpenAIResult(result),
			ClientDisconnect: result.ClientDisconnect,
			StreamIncomplete: result.StreamIncomplete,
		}))
	require.NotNil(t, repo.created)
	require.Equal(t, want, repo.created.CaptureCompleteness)
	require.NotEmpty(t, repo.created.Events)
	encoded, err := json.Marshal(repo.created)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), openAIChatIncompleteBodySentinel,
		"request audit must not retain the model body")
}

func TestHandleChatStreamingResponse_ReadErrorAfterPartialOutputIsIncomplete(t *testing.T) {
	result, recorder, err := runOpenAIChatStreamIncompleteTest(
		t,
		&openAIChatIncompleteReadError{payload: []byte(openAIChatPartialStream()), err: io.ErrUnexpectedEOF},
		nil,
		nil,
	)

	require.Error(t, err)
	code, message, ok := OpenAIUpstreamStreamReadErrorDetails(err)
	require.True(t, ok, "upstream read failures keep their typed classification")
	require.Equal(t, OpenAIUpstreamStreamReadErrorCode, code)
	require.NotEmpty(t, message)
	require.NotNil(t, result)
	require.True(t, result.StreamIncomplete,
		"an upstream read failure after partial output must mark the audit 响应中途不完整")
	require.False(t, result.ClientDisconnect, "上游读错误不是客户端断开")
	// 上游已计量的部分 usage 与骨架照常带出，供使用记录与审计使用。
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.OutputTokens)
	requireOpenAIChatAuditSkeletonIsDataFree(t, result.SSEEvents)
	require.Len(t, result.SSEEvents, 2, "事件骨架按上游 data 行计数")
	requireOpenAIChatAuditCompleteness(t, result, RequestAuditCaptureIncomplete, 21)
	// 下游已写出的部分输出不被追回，客户端流不被掐断。
	require.Contains(t, recorder.Body.String(), openAIChatIncompleteBodySentinel)
}

func TestHandleChatStreamingResponse_MissingTerminalEventIsIncomplete(t *testing.T) {
	result, recorder, err := runOpenAIChatStreamIncompleteTest(
		t,
		io.NopCloser(strings.NewReader(openAIChatPartialStream())),
		nil,
		nil,
	)

	require.Error(t, err)
	require.Contains(t, err.Error(), "missing terminal event")
	require.NotNil(t, result)
	require.True(t, result.StreamIncomplete,
		"a clean EOF without any terminal event must mark the audit 响应中途不完整")
	require.False(t, result.ClientDisconnect)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.OutputTokens)
	requireOpenAIChatAuditSkeletonIsDataFree(t, result.SSEEvents)
	requireOpenAIChatAuditCompleteness(t, result, RequestAuditCaptureIncomplete, 22)
	require.Contains(t, recorder.Body.String(), openAIChatIncompleteBodySentinel)
}

// keepalive 分支（goroutine + channel）的读错误退出点与同步路径同样语义。
func TestHandleChatStreamingResponse_KeepaliveReadErrorAfterPartialOutputIsIncomplete(t *testing.T) {
	result, recorder, err := runOpenAIChatStreamIncompleteTest(
		t,
		&openAIChatIncompleteReadError{payload: []byte(openAIChatPartialStream()), err: io.ErrUnexpectedEOF},
		&config.Config{Gateway: config.GatewayConfig{StreamKeepaliveInterval: 1}},
		nil,
	)

	require.Error(t, err)
	require.NotNil(t, result)
	require.True(t, result.StreamIncomplete,
		"keepalive 分支的上游读错误同样必须标记响应中途不完整")
	require.False(t, result.ClientDisconnect)
	require.Equal(t, 7, result.Usage.InputTokens)
	requireOpenAIChatAuditSkeletonIsDataFree(t, result.SSEEvents)
	requireOpenAIChatAuditCompleteness(t, result, RequestAuditCaptureIncomplete, 23)
	require.Contains(t, recorder.Body.String(), openAIChatIncompleteBodySentinel)
}

func TestHandleChatStreamingResponse_TerminalEventStaysComplete(t *testing.T) {
	result, recorder, err := runOpenAIChatStreamIncompleteTest(
		t,
		io.NopCloser(strings.NewReader(openAIChatTerminalStream())),
		nil,
		nil,
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.StreamIncomplete, "terminal event followed by EOF is a complete stream")
	require.False(t, result.ClientDisconnect)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.OutputTokens)
	requireOpenAIChatAuditSkeletonIsDataFree(t, result.SSEEvents)
	requireOpenAIChatAuditCompleteness(t, result, RequestAuditCaptureComplete, 24)
	require.Contains(t, recorder.Body.String(), openAIChatIncompleteBodySentinel)
	require.Contains(t, recorder.Body.String(), "data: [DONE]")
}

// 客户端主动断开由 ClientDisconnect 单独标记（审计侧同样是不完整），
// 不伪装成上游截断；已收到 usage 照常保留。
func TestHandleChatStreamingResponse_ClientDisconnectIsNotUpstreamTruncation(t *testing.T) {
	result, _, err := runOpenAIChatStreamIncompleteTest(
		t,
		io.NopCloser(strings.NewReader(openAIChatPartialStream())),
		nil,
		func(c *gin.Context) { c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer} },
	)

	require.Error(t, err)
	require.NotNil(t, result)
	require.True(t, result.ClientDisconnect, "下游写入失败即客户端断开")
	require.False(t, result.StreamIncomplete,
		"客户端主动断开不得伪装成上游截断（由 ClientDisconnect 单独标记）")
	require.Equal(t, 7, result.Usage.InputTokens)
	requireOpenAIChatAuditCompleteness(t, result, RequestAuditCaptureIncomplete, 25)
}

// 终端错误事件（response.failed / error）是上游给出的收尾，不是截断：
// 不得复用「响应中途不完整」标记。
func TestHandleChatStreamingResponse_TerminalErrorEventIsNotUpstreamTruncation(t *testing.T) {
	failedStream := "data: " + openAIChatIncompleteDelta + "\n\n" +
		"event: response.failed\n" +
		`data: {"type":"response.failed","response":{"id":"resp_chat_incomplete","object":"response","model":"gpt-5.4","status":"failed","output":[],"error":{"code":"upstream_error","message":"upstream failed mid stream"}}}` + "\n\n"

	result, recorder, err := runOpenAIChatStreamIncompleteTest(
		t,
		io.NopCloser(strings.NewReader(failedStream)),
		nil,
		nil,
	)

	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream response failed")
	require.NotNil(t, result)
	require.False(t, result.StreamIncomplete, "终端错误事件不是上游截断")
	require.False(t, result.ClientDisconnect)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Contains(t, recorder.Body.String(), "upstream failed mid stream")
}

// 不完整标记必须一路传到 OpenAIForwardResult.StreamIncomplete：使用记录的请求审计
// 从该结果取完整性，漏传就会把截断流记成「完整」。
func TestForwardAsChatCompletions_StreamIncompletePropagatesToForwardResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid-chat-incomplete"}},
		Body:       io.NopCloser(strings.NewReader(openAIChatPartialStream())),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:             3,
		Name:           "openai-oauth",
		Platform:       PlatformOpenAI,
		Type:           AccountTypeOAuth,
		Concurrency:    1,
		Status:         StatusActive,
		Schedulable:    true,
		RateMultiplier: f64p(1),
		Credentials:    map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-acc"},
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.4")

	require.Error(t, err)
	require.NotNil(t, result)
	require.True(t, result.StreamIncomplete,
		"a partial result without a terminal event must carry StreamIncomplete into the linked request audit")
	require.False(t, result.ClientDisconnect)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.NotEmpty(t, result.SSEEvents)
	requireOpenAIChatAuditSkeletonIsDataFree(t, result.SSEEvents)
	require.Equal(
		t,
		RequestAuditCaptureIncomplete,
		requestAuditCaptureCompleteness(RequestAuditInput{
			SSEEvents:        requestAuditSSEEventsFromOpenAIResult(result),
			ClientDisconnect: result.ClientDisconnect,
			StreamIncomplete: result.StreamIncomplete,
		}, buildRequestAuditEventSkeletons(requestAuditSSEEventsFromOpenAIResult(result))),
	)
	require.Contains(t, recorder.Body.String(), openAIChatIncompleteBodySentinel)
}
