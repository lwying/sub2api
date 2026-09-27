//go:build unit

package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/stretchr/testify/require"
)

func TestOpenAIAuditValueDetailNeverUsesClaudeHeaderRules(t *testing.T) {
	inbound, omission, supported := RequestAuditValueDetailInboundHeadersForProtocol(
		http.Header{"Content-Type": {"application/json"}, "Authorization": {"Bearer private-canary"}}, RequestAuditProtocolOpenAIResp)
	require.True(t, supported)
	in := RequestAuditValueDetailInput{
		Route: "/v1/responses", Protocol: RequestAuditProtocolOpenAIResp,
		InboundHeaderValues: inbound, InboundHeaderOmission: omission,
		Model: "gpt-5", Attempts: []RequestAuditValueDetailAttempt{{
			Index: 1, Protocol: RequestAuditProtocolOpenAIResp, Model: "gpt-5",
			RequestHeaderValues:  map[string]any{"Content-Type": "application/json"},
			ResponseHeaderValues: map[string]any{"Retry-After": "3"},
		}},
	}
	write := BuildRequestAuditValueDetailWrite(in, RequestAuditValueDetailGate{CaptureAllowed: true}, time.Now().UTC())
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailStateStored, write.State)
	require.Equal(t, RequestAuditProtocolOpenAIResp, write.Fields.Protocol)
	values, err := DecodeRequestAuditValueDetailValuesForProtocol(write.Payload, RequestAuditProtocolOpenAIResp)
	require.NoError(t, err)
	require.Equal(t, "application/json", values.Inbound.RequestHeaders["Content-Type"][0])
	require.Equal(t, "3", values.Attempts[0].ResponseHeaders["Retry-After"][0])
	require.NotContains(t, string(write.Payload), "private-canary")

	in.Attempts[0].Protocol = "bedrock"
	unsupported := BuildRequestAuditValueDetailWrite(in, RequestAuditValueDetailGate{CaptureAllowed: true}, time.Now().UTC())
	require.NotNil(t, unsupported)
	require.Equal(t, RequestAuditValueDetailSkippedUnsupportedProtocol, unsupported.Reason)
	require.Empty(t, unsupported.Payload)

	in.Attempts[0].Protocol = RequestAuditProtocolAnthropic
	mixed := BuildRequestAuditValueDetailWrite(in, RequestAuditValueDetailGate{CaptureAllowed: true}, time.Now().UTC())
	require.NotNil(t, mixed)
	require.Equal(t, RequestAuditValueDetailSkippedUnsupportedProtocol, mixed.Reason)
	require.Empty(t, mixed.Payload)
}

// 入站是 Messages、真实出站是 OpenAI Responses（Messages→OpenAI 转换）：入站头值必须按
// **入站路由**的闭集校验，逐次尝试的头值才按真实 wire 闭集校验。用出站协议解释入站 wire
// 会让 Messages 客户端的头值被判成「闭集外的客户端头」而整份丢掉。
func TestRequestAuditValueDetailInboundHeadersFollowInboundRouteForMessagesToOpenAI(t *testing.T) {
	in := RequestAuditValueDetailInput{
		Route: RequestAuditValueDetailRouteMessages, Protocol: RequestAuditProtocolOpenAIResp,
		InboundHeaderValues: map[string]any{
			"Anthropic-Beta": "claude-code-20250219",
			"Content-Type":   "application/json",
		},
		Model: "gpt-5",
		Attempts: []RequestAuditValueDetailAttempt{{
			Index: 1, Protocol: RequestAuditProtocolOpenAIResp, Model: "gpt-5",
			RequestHeaderValues:  map[string]any{"Content-Type": "application/json"},
			ResponseHeaderValues: map[string]any{"Retry-After": "3"},
		}},
	}
	write := BuildRequestAuditValueDetailWrite(in, RequestAuditValueDetailGate{CaptureAllowed: true}, time.Now().UTC())
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailStateStored, write.State)
	require.Contains(t, string(write.Payload), `"Anthropic-Beta"`,
		"Messages 入站头值必须按 Messages 闭集留存")
	require.NotContains(t, string(write.Payload), `"truncated":true`,
		"按入站协议采到的头值不是「丢过条目」")
}

// 读侧与采集侧必须用同一套双协议校验：入站头值按入站协议，尝试头值按真实 wire 协议。
// 用出站协议解释入站头值必须整份拒绝，不能静默丢掉一边再返回半份列表。
func TestRequestAuditValueDetailRevealUsesInboundAndWireProtocols(t *testing.T) {
	in := RequestAuditValueDetailInput{
		Route: RequestAuditValueDetailRouteMessages, Protocol: RequestAuditProtocolOpenAIResp,
		InboundHeaderValues: map[string]any{"Anthropic-Beta": "claude-code-20250219"},
		Model:               "gpt-5",
		Attempts: []RequestAuditValueDetailAttempt{{
			Index: 1, Protocol: RequestAuditProtocolOpenAIResp, Model: "gpt-5",
			ResponseHeaderValues: map[string]any{"Retry-After": "3"},
		}},
	}
	write := BuildRequestAuditValueDetailWrite(in, RequestAuditValueDetailGate{CaptureAllowed: true}, time.Now().UTC())
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailStateStored, write.State)

	values, err := DecodeRequestAuditValueDetailValuesForProtocols(write.Payload, RequestAuditProtocolAnthropic, RequestAuditProtocolOpenAIResp)
	require.NoError(t, err)
	require.Equal(t, []string{"claude-code-20250219"}, values.Inbound.RequestHeaders["Anthropic-Beta"])
	require.Equal(t, []string{"3"}, values.Attempts[0].ResponseHeaders["Retry-After"])
	require.False(t, values.Truncated)

	// 用出站协议解释入站头值绝不能被静默当成「客户端只发了这些」：被丢弃的条目必须
	// 从结果里去掉，而且这次读侧丢弃要作为**独立事实**报出（validation_dropped），
	// 不得并进载荷自带的 truncated（那是采集侧的事实），也不得静默消失。
	mismatched, err := DecodeRequestAuditValueDetailValuesForProtocols(write.Payload, RequestAuditProtocolOpenAIResp, RequestAuditProtocolOpenAIResp)
	require.NoError(t, err)
	require.Empty(t, mismatched.Inbound.RequestHeaders,
		"按错误协议读到的入站头值必须被丢弃，不得留在结果里")
	require.False(t, mismatched.Truncated, "读侧丢弃不得冒充采集侧的 truncated")
	require.True(t, mismatched.ValidationDropped, "读侧丢过条目必须作为独立事实报出")

	// 混用协议解释尝试头值（ok=false）与「读侧丢过条目」是两回事：前者必须保持整份拒绝。
	_, err = DecodeRequestAuditValueDetailValuesForProtocols(write.Payload, RequestAuditProtocolAnthropic, RequestAuditProtocolAnthropic)
	require.Error(t, err, "混用协议解释尝试头值必须 fail closed")
}

func TestOpenAIAttemptProtocolIsFrozenAcrossResponseCapture(t *testing.T) {
	counter := httpattempt.NewCounter()
	ctx := httpattempt.WithCounter(httpattempt.WithClaudeHeaderValueCapture(t.Context(), true), counter)
	ctx = httpattempt.WithMetadata(ctx, httpattempt.Metadata{Protocol: RequestAuditProtocolOpenAIResp, AccountID: 5})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://openai.example/v1/responses", nil)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	attempt := httpattempt.StartRequestAttempt(req)
	attempt.SetResponse(429, http.Header{"Retry-After": {"3"}, "Authorization": {"Bearer no"}}, false)
	values := RequestAuditValueDetailAttemptsFromHTTPMetadata(counter.Metadata())
	require.Len(t, values, 1)
	require.Equal(t, RequestAuditProtocolOpenAIResp, values[0].Protocol)
	require.Equal(t, "3", values[0].ResponseHeaderValues["Retry-After"])
	require.NotContains(t, values[0].ResponseHeaderValues, "Authorization")
}
