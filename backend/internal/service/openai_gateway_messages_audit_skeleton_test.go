//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// openAIMessagesAuditBodySentinel 是上游 SSE data 中的正文标记：审计骨架只能
// 保留类型/序号/字节/指纹，任何地方出现该子串都说明正文被落进了审计记录。
const openAIMessagesAuditBodySentinel = "OPENAI_MESSAGES_AUDIT_BODY_SENTINEL"

// openAIMessagesAuditResponsesStream 构造 /v1/messages 经 Responses -> Anthropic
// 转换路径看到的 Responses SSE。terminal=false 时上游在输出中途干净收尾，
// 缺少 response.completed。
func openAIMessagesAuditResponsesStream(terminal bool) string {
	lines := []string{
		`data: {"type":"response.created","response":{"id":"resp_audit","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"` + openAIMessagesAuditBodySentinel + `"}`,
		``,
	}
	if terminal {
		lines = append(lines,
			`data: {"type":"response.completed","response":{"id":"resp_audit","object":"response","model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}}}`,
			``,
			`data: [DONE]`,
			``,
		)
	}
	return strings.Join(lines, "\n")
}

func openAIMessagesAuditInputFromResult(result *OpenAIForwardResult) RequestAuditInput {
	return RequestAuditInput{
		SSEEvents:        requestAuditSSEEventsFromOpenAIResult(result),
		ClientDisconnect: result != nil && result.ClientDisconnect,
		StreamIncomplete: result != nil && result.StreamIncomplete,
	}
}

func TestHandleAnthropicStreamingResponseRequestAuditSkeleton(t *testing.T) {
	gin.SetMode(gin.TestMode)

	terminalStream := openAIMessagesAuditResponsesStream(true)
	partialStream := openAIMessagesAuditResponsesStream(false)

	tests := []struct {
		name             string
		body             io.ReadCloser
		wantDeadEvents   []string
		wantIncomplete   bool
		wantCompleteness string
		wantErr          string
	}{
		{
			name:             "terminal event marks the skeleton complete",
			body:             io.NopCloser(strings.NewReader(terminalStream)),
			wantDeadEvents:   []string{"response.created", "response.output_text.delta", "response.completed"},
			wantIncomplete:   false,
			wantCompleteness: RequestAuditCaptureComplete,
		},
		{
			name:             "clean EOF without terminal event is incomplete",
			body:             io.NopCloser(strings.NewReader(partialStream)),
			wantDeadEvents:   []string{"response.created", "response.output_text.delta"},
			wantIncomplete:   true,
			wantCompleteness: RequestAuditCaptureIncomplete,
			wantErr:          "missing terminal event",
		},
		{
			name:             "upstream read error after partial events is incomplete",
			body:             &streamReadCloser{payload: []byte(partialStream + "\n"), err: io.ErrUnexpectedEOF},
			wantDeadEvents:   []string{"response.created", "response.output_text.delta"},
			wantIncomplete:   true,
			wantCompleteness: RequestAuditCaptureIncomplete,
			wantErr:          "stream usage incomplete",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			resp := &http.Response{
				Header: http.Header{"x-request-id": []string{"rid_messages_audit"}},
				Body:   tt.body,
			}

			result, err := (&OpenAIGatewayService{}).handleAnthropicStreamingResponse(
				resp, c, nil, "claude-sonnet-4.5", "gpt-5.4", "gpt-5.4", time.Now(),
			)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
			}
			require.NotNil(t, result)

			// 下游 payload 不受影响：转换后的 Anthropic SSE 仍照常写出。
			require.Contains(t, rec.Body.String(), "event: message_start")

			require.Equal(t, tt.wantIncomplete, result.StreamIncomplete, "post-start upstream termination must mark the audit incomplete")
			require.Len(t, result.SSEEvents, len(tt.wantDeadEvents))
			for i, event := range result.SSEEvents {
				require.Equal(t, tt.wantDeadEvents[i], event.Type, "skeleton keeps upstream event type and order")
				require.Positive(t, event.Bytes, "skeleton keeps the wire byte count")
				require.Empty(t, event.Data, "skeleton must not retain SSE data")
			}

			repo := &stubRequestAuditRepo{}
			require.NoError(t, AttachRequestAuditAfterUsageLog(context.Background(), repo, &UsageLog{ID: 1}, openAIMessagesAuditInputFromResult(result)))
			require.NotNil(t, repo.created)
			require.Equal(t, tt.wantCompleteness, repo.created.CaptureCompleteness)
			require.NotEmpty(t, repo.created.Events)

			encoded, marshalErr := json.Marshal(repo.created)
			require.NoError(t, marshalErr)
			require.NotContains(t, string(encoded), openAIMessagesAuditBodySentinel, "request audit must not retain the model body")
		})
	}
}

func TestHandleAnthropicStreamingResponseRequestAuditSkeletonTruncatesAtEventCap(t *testing.T) {
	gin.SetMode(gin.TestMode)

	lines := []string{
		`data: {"type":"response.created","response":{"id":"resp_audit_truncated","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		``,
	}
	for i := 0; i < requestAuditMaxSSEEvents+1; i++ {
		lines = append(lines,
			fmt.Sprintf(`data: {"type":"response.output_text.delta","delta":"%s-%d"}`, openAIMessagesAuditBodySentinel, i),
			``,
		)
	}
	lines = append(lines,
		`data: {"type":"response.completed","response":{"id":"resp_audit_truncated","object":"response","model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}}}`,
		``,
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{
		Header: http.Header{"x-request-id": []string{"rid_messages_audit_truncated"}},
		Body:   io.NopCloser(strings.NewReader(strings.Join(lines, "\n"))),
	}

	result, err := (&OpenAIGatewayService{}).handleAnthropicStreamingResponse(
		resp, c, nil, "claude-sonnet-4.5", "gpt-5.4", "gpt-5.4", time.Now(),
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.StreamIncomplete, "a truncated capture is a capture state, not a failure")

	// 硬顶包含截断标记自身：kept 事件 + 1 条 truncated 骨架。
	require.Equal(t, requestAuditMaxSSEEvents, len(result.SSEEvents), "kept events plus the truncated marker")
	marker := result.SSEEvents[len(result.SSEEvents)-1]
	require.Equal(t, "truncated", marker.Type)

	repo := &stubRequestAuditRepo{}
	require.NoError(t, AttachRequestAuditAfterUsageLog(context.Background(), repo, &UsageLog{ID: 2}, openAIMessagesAuditInputFromResult(result)))
	require.NotNil(t, repo.created)
	require.Equal(t, RequestAuditCaptureTruncated, repo.created.CaptureCompleteness)

	var truncated *RequestAuditEventSkeleton
	for i := range repo.created.Events {
		if repo.created.Events[i].Type == "truncated" {
			truncated = &repo.created.Events[i]
			break
		}
	}
	require.NotNil(t, truncated, "truncation must be recorded as a skeleton")
	require.True(t, truncated.Truncated)
	// 上游总事件数：response.created + 超限的 delta 各一条 + response.completed。
	upstreamEvents := 1 + requestAuditMaxSSEEvents + 1 + 1
	require.Equal(t, upstreamEvents, truncated.Original)
	require.Equal(t, requestAuditMaxSSEEvents-1, truncated.Kept, "the truncated marker itself occupies one slot of the cap")
	require.Equal(t, upstreamEvents-truncated.Kept, truncated.Dropped)
	require.Equal(t, "max_events", truncated.Reason)

	encoded, marshalErr := json.Marshal(repo.created)
	require.NoError(t, marshalErr)
	require.NotContains(t, string(encoded), openAIMessagesAuditBodySentinel, "request audit must not retain the model body")
}
