//go:build unit

package service

// 入站 POST /v1/messages 走国产供应商原生 Anthropic 直通（api_protocol=anthropic）
// 的流式泵此前只解析 usage、逐行透传 SSE，从不 appendRequestAuditSSEEvent，
// 返回结果也没有 SSEEvents / StreamIncomplete：使用记录上的请求审计记录因此
// 看不出流式事件的类型/序号/体量/指纹，也无法区分「完整」「截断」「响应中途不完整」
// （采集完整性只能是「完整但空」）。
//
// 本文件断言外部可观测行为：
//   - 成功流：结果带事件骨架（类型/序号/体量），骨架与审计记录都不含 delta 正文；
//     客户端仍收到完整 SSE 中继（含 delta 文本）。
//   - 缺终态事件 / 上游读错误：结果带 StreamIncomplete，审计完整性为不完整。
//   - 客户端主动断开：由 ClientDisconnect 标记（审计完整性仍为不完整），
//     不得伪装成「上游截断」。
//   - 超 2000 条事件：先到先截断，审计完整性为截断且计数齐全。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// nativeAuditDeltaText 是被采集路径上真实存在的模型正文片段：任何审计输出都不得出现。
const nativeAuditDeltaText = "audit-delta-9f2c"

const nativeAuditDeltaEvent = `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + nativeAuditDeltaText + `"}}

`

// nativeAnthropicAuditSSEStream 返回一段最小可解析的原生 Anthropic 事件流；
// withTerminal=false 时省略 message_stop，模拟上游中断（截断）。
func nativeAnthropicAuditSSEStream(withTerminal bool) string {
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_audit","type":"message","role":"assistant","model":"k3","content":[],"usage":{"input_tokens":12,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		nativeAuditDeltaEvent +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n"
	if withTerminal {
		stream += "event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n"
	}
	return stream
}

func newNativeAnthropicAuditTestContext(t *testing.T, writer gin.ResponseWriter) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if writer != nil {
		c.Writer = writer
	}
	return c, rec
}

func nativeAnthropicAuditResponse(body io.Reader) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"x-request-id": []string{"rid-native-anthropic-audit"},
		},
		Body: io.NopCloser(body),
	}
}

func recordNativeAnthropicAudit(t *testing.T, result *OpenAIForwardResult) *RequestAuditRecord {
	t.Helper()
	require.NotNil(t, result)
	record := BuildRequestAuditRecord(RequestAuditInput{
		SSEEvents:        result.SSEEvents,
		ClientDisconnect: result.ClientDisconnect,
		StreamIncomplete: result.StreamIncomplete,
	})
	require.NotNil(t, record)
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), nativeAuditDeltaText, "审计记录不得落 SSE delta 正文")
	return record
}

func TestNativeAnthropicStreamAuditCapturesSkeletonsOnSuccess(t *testing.T) {
	svc := newNativeAnthropicHangTestService(5)
	c, rec := newNativeAnthropicAuditTestContext(t, nil)

	result, err := svc.handleNativeAnthropicStreamingResponse(
		context.Background(),
		nativeAnthropicAuditResponse(strings.NewReader(nativeAnthropicAuditSSEStream(true))),
		c, nativeAnthropicTestAccount(), "k3", "k3", "k3", nil, time.Now(),
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.False(t, result.StreamIncomplete, "观测到终态事件的正常流不得标记为截断")
	require.False(t, result.ClientDisconnect)

	// 审计采集不得改变向客户端的字节级中继。
	require.Contains(t, rec.Body.String(), nativeAuditDeltaText, "客户端仍须收到原始 SSE 中继")

	require.Len(t, result.SSEEvents, 6)
	wantTypes := []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	for i, ev := range result.SSEEvents {
		require.Equal(t, wantTypes[i], ev.Type, "事件骨架须记录解析出的事件类型")
		require.Empty(t, ev.Data, "事件骨架不得携带 SSE data 正文")
		require.Greater(t, ev.Bytes, 0, "事件骨架须记录体量")
		require.NotContains(t, ev.Type, nativeAuditDeltaText)
	}

	record := recordNativeAnthropicAudit(t, result)
	require.Equal(t, RequestAuditCaptureComplete, record.CaptureCompleteness)
	require.Len(t, record.Events, len(result.SSEEvents))
	require.Equal(t, 0, record.Events[0].Index)
	require.Equal(t, "content_block_delta", record.Events[2].Type)
	require.Equal(t, 2, record.Events[2].Index)
	require.False(t, record.Events[0].Truncated)
}

func TestNativeAnthropicStreamAuditMarksMissingTerminalIncomplete(t *testing.T) {
	svc := newNativeAnthropicHangTestService(5)
	c, rec := newNativeAnthropicAuditTestContext(t, nil)

	partial := nativeAnthropicAuditSSEStream(false)
	result, err := svc.handleNativeAnthropicStreamingResponse(
		context.Background(),
		nativeAnthropicAuditResponse(strings.NewReader(partial)),
		c, nativeAnthropicTestAccount(), "k3", "k3", "k3", nil, time.Now(),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing terminal event")
	require.NotNil(t, result)
	require.True(t, result.StreamIncomplete, "缺终态事件必须标记流不完整")
	require.False(t, result.ClientDisconnect)

	// 截断前已产出的 SSE 仍须完整送达客户端。
	require.Contains(t, rec.Body.String(), nativeAuditDeltaText, "截断前已产出的 SSE 仍须送达客户端")

	require.Len(t, result.SSEEvents, 5)
	for _, ev := range result.SSEEvents {
		require.Empty(t, ev.Data)
	}

	record := recordNativeAnthropicAudit(t, result)
	require.Equal(t, RequestAuditCaptureIncomplete, record.CaptureCompleteness)
	require.Len(t, record.Events, len(result.SSEEvents))
}

func TestNativeAnthropicStreamAuditMarksReadErrorIncomplete(t *testing.T) {
	svc := newNativeAnthropicHangTestService(5)
	c, rec := newNativeAnthropicAuditTestContext(t, nil)

	resp := nativeAnthropicAuditResponse(&streamReadCloser{
		payload: []byte(nativeAnthropicAuditSSEStream(false)),
		err:     io.ErrUnexpectedEOF,
	})
	result, err := svc.handleNativeAnthropicStreamingResponse(
		context.Background(), resp, c, nativeAnthropicTestAccount(), "k3", "k3", "k3", nil, time.Now(),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "stream read error")
	require.NotNil(t, result)
	require.True(t, result.StreamIncomplete, "上游读错误必须标记流不完整")
	require.False(t, result.ClientDisconnect)

	require.NotEmpty(t, result.SSEEvents, "读错误前已观测的事件骨架须保留")
	for _, ev := range result.SSEEvents {
		require.Empty(t, ev.Data)
	}
	require.Contains(t, rec.Body.String(), nativeAuditDeltaText)

	record := recordNativeAnthropicAudit(t, result)
	require.Equal(t, RequestAuditCaptureIncomplete, record.CaptureCompleteness)
}

func TestNativeAnthropicStreamAuditKeepsClientDisconnectOutOfStreamIncomplete(t *testing.T) {
	svc := newNativeAnthropicHangTestService(5)
	rec := httptest.NewRecorder()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer}

	result, err := svc.handleNativeAnthropicStreamingResponse(
		context.Background(),
		nativeAnthropicAuditResponse(strings.NewReader(nativeAnthropicAuditSSEStream(true))),
		c, nativeAnthropicTestAccount(), "k3", "k3", "k3", nil, time.Now(),
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.ClientDisconnect, "写失败须标记客户端断开")
	require.False(t, result.StreamIncomplete, "客户端主动取消不得伪装成上游截断")

	require.NotEmpty(t, result.SSEEvents, "客户端断开后仍需继续采集上游事件骨架")
	for _, ev := range result.SSEEvents {
		require.Empty(t, ev.Data)
	}

	record := recordNativeAnthropicAudit(t, result)
	require.Equal(t, RequestAuditCaptureIncomplete, record.CaptureCompleteness, "客户端断开在审计侧同为不完整")
}

func TestNativeAnthropicStreamAuditTruncatesEventSkeletonsAtCap(t *testing.T) {
	svc := newNativeAnthropicHangTestService(5)
	c, _ := newNativeAnthropicAuditTestContext(t, nil)

	delta := `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + nativeAuditDeltaText + `"}}

`
	var stream strings.Builder
	overflow := requestAuditMaxSSEEvents + 5
	for i := 0; i < overflow; i++ {
		stream.WriteString(delta)
	}
	stream.WriteString("event: message_stop\n")
	stream.WriteString(`data: {"type":"message_stop"}` + "\n\n")

	result, err := svc.handleNativeAnthropicStreamingResponse(
		context.Background(),
		nativeAnthropicAuditResponse(strings.NewReader(stream.String())),
		c, nativeAnthropicTestAccount(), "k3", "k3", "k3", nil, time.Now(),
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.StreamIncomplete, "观测到终态事件即不标记流不完整")

	for _, ev := range result.SSEEvents {
		require.Empty(t, ev.Data, "截断也不得落事件正文")
	}

	record := recordNativeAnthropicAudit(t, result)
	require.Equal(t, RequestAuditCaptureTruncated, record.CaptureCompleteness)
	require.Len(t, result.SSEEvents, requestAuditMaxSSEEvents, "先到先截断：事件上限内含一条 truncated 骨架")
	require.Len(t, record.Events, requestAuditMaxSSEEvents, "骨架 JSON 不得超过事件上限")
	marker := record.Events[len(record.Events)-1]
	require.Equal(t, "truncated", marker.Type)
	require.True(t, marker.Truncated)
	require.Equal(t, overflow+1, marker.Original)
	require.Equal(t, len(record.Events)-1, marker.Kept)
	require.Equal(t, marker.Original-marker.Kept, marker.Dropped)
	require.Greater(t, marker.Dropped, 0)
	require.Equal(t, "max_events", marker.Reason)
}
