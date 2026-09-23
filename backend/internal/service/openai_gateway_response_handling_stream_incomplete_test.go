package service

import (
	"bufio"
	"context"
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

func runOpenAIResponseStreamIncompleteTest(t *testing.T, body io.ReadCloser, gatewayCfg config.GatewayConfig) (*openaiStreamingResult, error, string) {
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
	return result, err, recorder.Body.String()
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

	result, err, clientBody := runOpenAIResponseStreamIncompleteTest(
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

	result, err, clientBody := runOpenAIResponseStreamIncompleteTest(
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

	result, err, _ := runOpenAIResponseStreamIncompleteTest(
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
