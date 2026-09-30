package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 下游测试请求的本地 mock 回复。回复文本完全来自管理员配置的规则，
// 不使用语言猜测；协议结构按下游入口适配，用量一律为零，且带本地标识头。
// 这里不产生使用记录，也不产生上游尝试。

// gatewayMockMarkLocalReply 打出"这是本地返回"的标识头。
func gatewayMockMarkLocalReply(c *gin.Context) {
	c.Header(service.GatewayMockReplyHeader, service.GatewayMockReplyHeaderValue)
}

// sendGatewayMockReply 按入站协议写出本地 mock 回复。
func sendGatewayMockReply(c *gin.Context, protocol service.GatewayMockProtocol, model string, match GatewayMockMatch) {
	gatewayMockMarkLocalReply(c)
	switch protocol {
	case service.GatewayMockProtocolMessages:
		sendGatewayMockMessages(c, model, match)
	case service.GatewayMockProtocolChatCompletions:
		sendGatewayMockChatCompletions(c, model, match)
	case service.GatewayMockProtocolResponses:
		sendGatewayMockResponses(c, model, match)
	default:
		// 未适配的协议不得写出半成品响应：直接拒绝是唯一安全结论。
		c.AbortWithStatus(http.StatusNotImplemented)
	}
}

// gatewayMockUsage 返回零用量块：本地 mock 没有发生真实推理。
func gatewayMockUsage() gin.H {
	return gin.H{
		"input_tokens":                0,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
		"cache_creation": gin.H{
			"ephemeral_5m_input_tokens": 0,
			"ephemeral_1h_input_tokens": 0,
		},
		"output_tokens": 0,
	}
}

// sendGatewayMockMessages 写出 Anthropic Messages 形态的本地回复。
func sendGatewayMockMessages(c *gin.Context, model string, match GatewayMockMatch) {
	msgID := generateRealisticMsgID()
	if gatewayMockStreamRequested(c) {
		setGatewayMockSSEHeaders(c)
		start := gin.H{
			"type": "message_start",
			"message": gin.H{
				"model": model, "id": msgID, "type": "message", "role": "assistant",
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "stop_details": nil,
				"usage": gatewayMockUsage(),
			},
		}
		gatewayMockWriteSSE(c, "message_start", start)
		gatewayMockWriteSSE(c, "content_block_start", gin.H{
			"type": "content_block_start", "index": 0,
			"content_block": gin.H{"type": "text", "text": ""},
		})
		gatewayMockWriteSSE(c, "content_block_delta", gin.H{
			"type": "content_block_delta", "index": 0,
			"delta": gin.H{"type": "text_delta", "text": match.Reply},
		})
		gatewayMockWriteSSE(c, "content_block_stop", gin.H{"type": "content_block_stop", "index": 0})
		gatewayMockWriteSSE(c, "message_delta", gin.H{
			"type":  "message_delta",
			"delta": gin.H{"stop_reason": "end_turn", "stop_sequence": nil, "stop_details": nil},
			"usage": gin.H{"output_tokens": 0},
		})
		gatewayMockWriteSSE(c, "message_stop", gin.H{"type": "message_stop"})
		c.Writer.Flush()
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"model":         model,
		"id":            msgID,
		"type":          "message",
		"role":          "assistant",
		"content":       []gin.H{{"type": "text", "text": match.Reply}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"stop_details":  nil,
		"usage":         gatewayMockUsage(),
	})
}

// sendGatewayMockChatCompletions 写出 OpenAI Chat Completions 形态的本地回复。
func sendGatewayMockChatCompletions(c *gin.Context, model string, match GatewayMockMatch) {
	completionID := "chatcmpl-" + strings.TrimPrefix(generateRealisticMsgID(), "msg_01")
	created := time.Now().Unix()
	if gatewayMockStreamRequested(c) {
		setGatewayMockSSEHeaders(c)
		gatewayMockWriteSSEData(c, gin.H{
			"id": completionID, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []gin.H{{"index": 0, "delta": gin.H{"role": "assistant", "content": match.Reply}, "finish_reason": nil}},
		})
		gatewayMockWriteSSEData(c, gin.H{
			"id": completionID, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []gin.H{{"index": 0, "delta": gin.H{}, "finish_reason": "stop"}},
			"usage":   gin.H{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		})
		_, _ = c.Writer.WriteString("data: [DONE]\n\n")
		c.Writer.Flush()
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":      completionID,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []gin.H{{
			"index":         0,
			"message":       gin.H{"role": "assistant", "content": match.Reply},
			"finish_reason": "stop",
		}},
		"usage": gin.H{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	})
}

// sendGatewayMockResponses 写出 OpenAI Responses 形态的本地回复。
func sendGatewayMockResponses(c *gin.Context, model string, match GatewayMockMatch) {
	responseID := "resp_" + strings.TrimPrefix(generateRealisticMsgID(), "msg_01")
	created := time.Now().Unix()
	completed := gin.H{
		"id": responseID, "object": "response", "created_at": created, "model": model,
		"status": "completed",
		"output": []gin.H{{
			"type": "message", "id": responseID, "status": "completed", "role": "assistant",
			"content": []gin.H{{"type": "output_text", "text": match.Reply, "annotations": []any{}}},
		}},
		"usage": gin.H{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
	}
	if gatewayMockStreamRequested(c) {
		setGatewayMockSSEHeaders(c)
		gatewayMockWriteSSE(c, "response.created", gin.H{
			"type": "response.created",
			"response": gin.H{
				"id": responseID, "object": "response", "created_at": created, "model": model,
				"status": "in_progress", "output": []any{},
			},
		})
		gatewayMockWriteSSE(c, "response.completed", gin.H{"type": "response.completed", "response": completed})
		c.Writer.Flush()
		return
	}
	c.JSON(http.StatusOK, completed)
}

// gatewayMockStreamRequested 报告本次入站请求是否要求流式响应。
// 只读取已解析上下文里的结论，不重复解析请求体。
func gatewayMockStreamRequested(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value, ok := c.Get(gatewayMockStreamContextKey)
	if !ok {
		return false
	}
	stream, _ := value.(bool)
	return stream
}

// gatewayMockStreamContextKey 由入口在解析请求体后写入，供 mock 发送器判定响应形态。
const gatewayMockStreamContextKey = "gateway_mock_stream"

// markGatewayMockStream 记录入站请求的流式意图。
func markGatewayMockStream(c *gin.Context, stream bool) {
	if c == nil {
		return
	}
	c.Set(gatewayMockStreamContextKey, stream)
}

// setGatewayMockSSEHeaders 与真实流式路径保持一致的事件流响应头。
func setGatewayMockSSEHeaders(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
}

// gatewayMockWriteSSE 写出一条带 event 名的 SSE 事件。
func gatewayMockWriteSSE(c *gin.Context, event string, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = c.Writer.WriteString("event: " + event + "\ndata: " + string(encoded) + "\n\n")
}

// gatewayMockWriteSSEData 写出一条只有 data 的 SSE 事件。
func gatewayMockWriteSSEData(c *gin.Context, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = c.Writer.WriteString("data: " + string(encoded) + "\n\n")
}
