//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func forceChatMessagesFallbackAccount() *Account {
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{
		openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceChatCompletions),
	}
	return account
}

// errTailReader yields the given data, then returns err instead of io.EOF,
// simulating an upstream connection that breaks mid-stream.
type errTailReader struct {
	data []byte
	off  int
	err  error
}

func (r *errTailReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

func (r *errTailReader) Close() error { return nil }

func TestForwardAsAnthropic_ForceChatCompletionsPreservesFinalModelReasoningEffort(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		model      string
		mapped     string
		effortJSON string
		wantEffort string
		maxPolicy  string
	}{
		{
			name:       "policy caps converted effort",
			model:      "gpt-5.6-luna",
			mapped:     "gpt-5.6-luna",
			effortJSON: `,"output_config":{"effort":"max"}`,
			wantEffort: "medium",
			maxPolicy:  "medium",
		},
		{
			name:       "GPT56 max",
			model:      "luna",
			mapped:     "gpt-5.6-luna",
			effortJSON: `,"output_config":{"effort":"max"}`,
			wantEffort: "max",
		},
		{
			name:       "old model max",
			model:      "gpt-5.5",
			mapped:     "gpt-5.5",
			effortJSON: `,"output_config":{"effort":"max"}`,
			wantEffort: "xhigh",
		},
		{
			name:       "high remains high",
			model:      "gpt-5.6-luna",
			mapped:     "gpt-5.6-luna",
			effortJSON: `,"output_config":{"effort":"high"}`,
			wantEffort: "high",
		},
		{
			name:       "omitted defaults medium",
			model:      "gpt-5.6-luna",
			mapped:     "gpt-5.6-luna",
			wantEffort: "medium",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			body := `{"model":"` + tt.model + `","max_tokens":16,"messages":[{"role":"user","content":"hello"}]` + tt.effortJSON + `,"stream":false}`
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
			c.Request.Header.Set("Content-Type", "application/json")

			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"chatcmpl_effort","object":"chat.completion","model":"` + tt.mapped + `","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
				)),
			}}
			account := forceChatMessagesFallbackAccount()
			account.Credentials["model_mapping"] = map[string]any{tt.model: tt.mapped}

			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			ctx := context.Background()
			if tt.maxPolicy != "" {
				ctx = WithOpenAIReasoningEffortPolicy(ctx, tt.maxPolicy, nil, "")
			}
			result, err := svc.ForwardAsAnthropic(ctx, c, account, []byte(body), "", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, tt.mapped, gjson.GetBytes(upstream.lastBody, "model").String())
			require.Equal(t, tt.wantEffort, gjson.GetBytes(upstream.lastBody, "reasoning_effort").String())
			require.NotNil(t, result.ReasoningEffort)
			require.Equal(t, tt.wantEffort, *result.ReasoningEffort)
		})
	}
}

func TestForwardAsAnthropic_ForceChatCompletionsNonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_msg_chat_json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl_json","object":"chat.completion","model":"gpt-5.4","service_tier":"priority","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,"prompt_tokens_details":{"cached_tokens":1}}}`,
		)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "http://upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.Equal(t, "hello", gjson.GetBytes(upstream.lastBody, "messages.0.content").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "input").Exists())
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream_options").Exists() == false)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "assistant", gjson.Get(rec.Body.String(), "role").String())
	require.Equal(t, "ok", gjson.Get(rec.Body.String(), "content.0.text").String())
	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Equal(t, 1, result.Usage.CacheReadInputTokens)
	require.Nil(t, result.ServiceTier)
	require.Equal(t, "priority", result.UpstreamResponseServiceTier)
	require.False(t, result.Stream)
}

// Covers the fully-new streaming composition: text block is still open when
// [DONE] arrives, so finalization must close it (content_block_stop) before
// message_delta / message_stop.
func TestForwardAsAnthropic_ForceChatCompletionsStreamingClosesOpenBlockOnDone(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_s","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_s","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"he"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_s","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_s","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"chatcmpl_s","object":"chat.completion.chunk","model":"gpt-5.4","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_chat_stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream_options.include_usage").Bool())

	out := rec.Body.String()
	require.Contains(t, out, "event: message_start")
	require.Contains(t, out, `"text":"he"`)
	require.Contains(t, out, `"text":"llo"`)
	require.Contains(t, out, "event: content_block_stop")
	require.Contains(t, out, `"stop_reason":"end_turn"`)
	require.Contains(t, out, "event: message_stop")

	blockStop := strings.Index(out, "event: content_block_stop")
	msgDelta := strings.Index(out, `"stop_reason":"end_turn"`)
	msgStop := strings.Index(out, "event: message_stop")
	require.Greater(t, msgDelta, blockStop, "content_block_stop must precede message_delta")
	require.Greater(t, msgStop, msgDelta, "message_delta must precede message_stop")

	require.Equal(t, 4, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.True(t, result.Stream)
	require.NotNil(t, result.FirstTokenMs)
}

// Covers multi-chunk tool_call fragments aggregated by index and finalized as
// an Anthropic tool_use block with stop_reason=tool_use.
func TestForwardAsAnthropic_ForceChatCompletionsStreamingToolCallAggregation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"weather in sf?"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_t","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_t","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_t","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"sf\"}"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_t","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		`data: {"id":"chatcmpl_t","object":"chat.completion.chunk","model":"gpt-5.4","choices":[],"usage":{"prompt_tokens":6,"completion_tokens":5,"total_tokens":11}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_chat_tool"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)

	out := rec.Body.String()
	require.Contains(t, out, `"type":"tool_use"`)
	require.Contains(t, out, `"name":"get_weather"`)
	require.Contains(t, out, `"input_json_delta"`)
	require.Contains(t, out, `"stop_reason":"tool_use"`)
	require.Contains(t, out, "event: message_stop")
	require.Equal(t, 6, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
}

// finish_reason=length must survive the double conversion (CC → Responses →
// Anthropic) as stop_reason=max_tokens.
func TestForwardAsAnthropic_ForceChatCompletionsStreamingLengthMapsToMaxTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":8,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_l","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","content":"truncat"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_l","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":4,"completion_tokens":8,"total_tokens":12}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_chat_len"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)

	out := rec.Body.String()
	require.Contains(t, out, `"stop_reason":"max_tokens"`)
	require.Contains(t, out, "event: message_stop")
}

// An upstream that ends immediately with [DONE] must still produce a fully
// framed (message_start → message_delta → message_stop) Anthropic stream.
func TestForwardAsAnthropic_ForceChatCompletionsEmptyStreamStillFramesMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":8,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_chat_empty"}},
		Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)

	out := rec.Body.String()
	require.Contains(t, out, "event: message_start")
	require.Contains(t, out, "event: message_delta")
	require.Contains(t, out, "event: message_stop")
}

// Non-failover 4xx responses must go through the shared compat error handler:
// status-specific Anthropic error type, upstream message preserved, and ops
// upstream-error events recorded (previously this branch bypassed all three).
func TestForwardAsAnthropic_ForceChatCompletionsNonFailover400UsesSharedErrorHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":8,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_msg_chat_400"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"invalid roles","type":"invalid_request_error"}}`)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.Error(t, err)
	require.Nil(t, result)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "error", gjson.Get(rec.Body.String(), "type").String())
	require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.Equal(t, "invalid roles", gjson.Get(rec.Body.String(), "error.message").String())

	statusVal, ok := c.Get(OpsUpstreamStatusCodeKey)
	require.True(t, ok, "shared handler must record the upstream status for ops")
	require.Equal(t, http.StatusBadRequest, statusVal)

	eventsVal, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok, "shared handler must append an ops upstream error event")
	events, castOK := eventsVal.([]*OpsUpstreamErrorEvent)
	require.True(t, castOK)
	require.Len(t, events, 1)
	require.Equal(t, http.StatusBadRequest, events[0].UpstreamStatusCode)
	require.Equal(t, "http_error", events[0].Kind)
	require.Equal(t, "invalid roles", events[0].Message)
}

// A broken upstream read mid-stream must surface an error and must NOT emit a
// synthetic message_stop that would disguise the truncation as a completion.
func TestForwardAsAnthropic_ForceChatCompletionsStreamReadErrorSkipsFinalize(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":8,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	partial := strings.Join([]string{
		`data: {"id":"chatcmpl_e","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_e","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"he"},"finish_reason":null}]}`,
		"",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_chat_err"}},
		Body:       &errTailReader{data: []byte(partial), err: errors.New("simulated upstream read failure")},
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "stream usage incomplete")
	require.NotNil(t, result)
	require.True(t, result.Stream)

	out := rec.Body.String()
	require.Contains(t, out, `"text":"he"`, "delta emitted before the failure must reach the client")
	require.NotContains(t, out, "event: message_stop", "no synthetic completion after a broken read")
}

// ── 请求审计采集完整性：CC 回退路径的流式截断 ──
//
// 两条 CC 回退路径（/v1/messages 与 /v1/responses）共用下面的构造与断言；
// responses 侧的 focused 测试文件复用这里定义的 helper。

// chatFallbackAuditDeltaText 是 CC 回退路径上真实存在的模型正文片段：
// 事件骨架与审计记录的任何位置都不得出现。
const chatFallbackAuditDeltaText = "chat-fallback-audit-delta-7c1"

// chatFallbackAuditChunk 构造一个携带正文的 CC delta chunk（无终止信号）。
func chatFallbackAuditChunk() string {
	return `data: {"id":"chatcmpl_audit","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"` + chatFallbackAuditDeltaText + `"},"finish_reason":null}]}` + "\n\n"
}

// chatFallbackAuditStream 构造最小 CC 上游 SSE。withDone / withFinish / withUsage
// 三个终止信号开关全为 false 时，上游在输出中途干净收尾（缺终止信号 = 截断）。
func chatFallbackAuditStream(withDone, withFinish, withUsage bool) string {
	stream := chatFallbackAuditChunk()
	if withFinish {
		stream += `data: {"id":"chatcmpl_audit","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	}
	if withUsage {
		stream += `data: {"id":"chatcmpl_audit","object":"chat.completion.chunk","model":"gpt-5.4","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}` + "\n\n"
	}
	if withDone {
		stream += "data: [DONE]\n\n"
	}
	return stream
}

// requireChatFallbackAuditCompleteness 把结果按使用记录挂审计的方式落一次库，
// 断言采集完整性，并确认审计记录不含模型正文。
func requireChatFallbackAuditCompleteness(t *testing.T, result *OpenAIForwardResult, want string) {
	t.Helper()
	require.NotNil(t, result)
	repo := &stubRequestAuditRepo{}
	require.NoError(t, AttachRequestAuditAfterUsageLog(context.Background(), repo, &UsageLog{ID: 1}, RequestAuditInput{
		SSEEvents:        requestAuditSSEEventsFromOpenAIResult(result),
		ClientDisconnect: result.ClientDisconnect,
		StreamIncomplete: result.StreamIncomplete,
	}))
	require.NotNil(t, repo.created)
	require.Equal(t, want, repo.created.CaptureCompleteness)
	encoded, err := json.Marshal(repo.created)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), chatFallbackAuditDeltaText, "请求审计不得保留模型正文")
}

// 上游在响应开始后异常终止（读错误，或缺终止信号的干净 EOF）时，已观测的 usage 与
// 事件骨架照常带出，但结果必须带 StreamIncomplete，使使用记录上的请求审计记
// 「响应中途不完整」而不是「完整」。终止信号（[DONE] / finish_reason / usage 帧）
// 任一出现即视为正常收尾——只认 [DONE] 会把「跑完最后一帧就 EOF」的兼容上游误判成截断。
func TestForwardAsAnthropic_ForceChatCompletionsStreamAuditMarksUpstreamTruncationIncomplete(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name             string
		body             io.ReadCloser
		wantErr          string
		wantIncomplete   bool
		wantCompleteness string
		wantEvents       int
	}{
		{
			name:             "upstream read error after partial output is incomplete",
			body:             &errTailReader{data: []byte(chatFallbackAuditStream(false, false, false)), err: errors.New("simulated upstream read failure")},
			wantErr:          "stream usage incomplete",
			wantIncomplete:   true,
			wantCompleteness: RequestAuditCaptureIncomplete,
			wantEvents:       1,
		},
		{
			name:             "clean EOF without any terminal signal is incomplete",
			body:             io.NopCloser(strings.NewReader(chatFallbackAuditStream(false, false, false))),
			wantIncomplete:   true,
			wantCompleteness: RequestAuditCaptureIncomplete,
			wantEvents:       1,
		},
		{
			name:             "finish_reason terminal without done sentinel keeps the audit complete",
			body:             io.NopCloser(strings.NewReader(chatFallbackAuditStream(false, true, false))),
			wantIncomplete:   false,
			wantCompleteness: RequestAuditCaptureComplete,
			wantEvents:       2,
		},
		{
			name:             "usage frame terminal keeps the audit complete",
			body:             io.NopCloser(strings.NewReader(chatFallbackAuditStream(false, false, true))),
			wantIncomplete:   false,
			wantCompleteness: RequestAuditCaptureComplete,
			wantEvents:       2,
		},
		{
			name:             "done sentinel keeps the audit complete",
			body:             io.NopCloser(strings.NewReader(chatFallbackAuditStream(true, true, true))),
			wantIncomplete:   false,
			wantCompleteness: RequestAuditCaptureComplete,
			wantEvents:       4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")

			svc := &OpenAIGatewayService{
				cfg: rawChatCompletionsTestConfig(),
				httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_chat_audit"}},
					Body:       tt.body,
				}},
			}

			result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
			}
			require.NotNil(t, result)
			require.True(t, result.Stream)
			require.Equal(t, tt.wantIncomplete, result.StreamIncomplete,
				"post-start upstream termination must mark the linked request audit incomplete")
			require.False(t, result.ClientDisconnect)

			// 客户端行为不变：失败前已产出的 SSE 仍照常送达。
			require.Contains(t, rec.Body.String(), chatFallbackAuditDeltaText)

			// 采集不得保留正文，也不得丢失已观测的骨架。
			require.Len(t, result.SSEEvents, tt.wantEvents)
			for _, ev := range result.SSEEvents {
				require.Empty(t, ev.Data, "事件骨架不得携带 SSE data 正文")
				require.Positive(t, ev.Bytes)
			}

			requireChatFallbackAuditCompleteness(t, result, tt.wantCompleteness)
		})
	}
}

// 缺终止信号的干净 EOF 不改变客户端行为：不返回错误（与读错误路径不同），
// 客户端仍收到收尾帧；审计靠 StreamIncomplete 区分「完整」与「响应中途不完整」。
func TestForwardAsAnthropic_ForceChatCompletionsStreamAuditMissingTerminalKeepsClientContract(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	svc := &OpenAIGatewayService{
		cfg: rawChatCompletionsTestConfig(),
		httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_chat_audit_eof"}},
			Body:       io.NopCloser(strings.NewReader(chatFallbackAuditStream(false, false, false))),
		}},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.NoError(t, err, "客户端契约不变：缺终止信号不向客户端报错")
	require.NotNil(t, result)
	require.True(t, result.StreamIncomplete)

	out := rec.Body.String()
	require.Contains(t, out, chatFallbackAuditDeltaText)
	require.Contains(t, out, "event: message_stop", "收尾帧仍照常写出")

	requireChatFallbackAuditCompleteness(t, result, RequestAuditCaptureIncomplete)
}

// 客户端主动断开/取消由 ClientDisconnect 单独标记（审计同样不完整），
// 不得伪装成上游截断。
func TestForwardAsAnthropic_ForceChatCompletionsStreamAuditKeepsClientDisconnectOutOfStreamIncomplete(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer}

	svc := &OpenAIGatewayService{
		cfg: rawChatCompletionsTestConfig(),
		httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_chat_audit_dc"}},
			Body:       io.NopCloser(strings.NewReader(chatFallbackAuditStream(true, true, true))),
		}},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.ClientDisconnect, "写失败须标记客户端断开")
	require.False(t, result.StreamIncomplete, "客户端主动取消不得伪装成上游截断")
	require.Len(t, result.SSEEvents, 4, "断开后仍继续采集上游事件骨架")

	requireChatFallbackAuditCompleteness(t, result, RequestAuditCaptureIncomplete)
}

// Gate regression: an API-key account whose upstream is confirmed to support
// the Responses API must keep using /v1/responses, never the CC fallback.
func TestForwardAsAnthropic_ResponsesSupportedAccountStillUsesResponsesEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"output_config":{"effort":"high"},"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "third-party-client/1.0.0")
	c.Request.Header.Set("originator", "opencode")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_native","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_native"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{
		openai_compat.ExtraKeyResponsesMode:      string(openai_compat.ResponsesSupportModeAuto),
		openai_compat.ExtraKeyResponsesSupported: true,
	}

	ctx := WithOpenAIReasoningEffortPolicy(context.Background(), "medium", nil, "")
	result, err := svc.ForwardAsAnthropic(ctx, c, account, body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, strings.HasSuffix(upstream.lastReq.URL.Path, "/responses"),
		"responses-capable account must stay on /v1/responses, got %s", upstream.lastReq.URL.String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "input").Exists())
	require.Equal(t, "medium", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
	require.NotNil(t, result.ReasoningEffort)
	require.Equal(t, "medium", *result.ReasoningEffort)
	require.False(t, gjson.GetBytes(upstream.lastBody, "messages").Exists())
	require.Equal(t, "third-party-client/1.0.0", upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, "opencode", upstream.lastReq.Header.Get("originator"))
	require.Empty(t, upstream.lastReq.Header.Get("version"))
	require.Empty(t, upstream.lastReq.Header.Get("OpenAI-Beta"))
	require.Equal(t, "ok", gjson.Get(rec.Body.String(), "content.0.text").String())
}
