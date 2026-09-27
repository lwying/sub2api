package handler

import (
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	// requestAuditValueDetailProtocolUnsupported 是「真实出站形态未知或混合」的哨兵：
	// 它让 BuildRequestAuditValueDetailWrite 记 skipped_unsupported_protocol，且由于它不在
	// 闭集里，明文信封的 protocol 字段会被清空——「不知道是什么协议」如实写成未记录。
	requestAuditValueDetailProtocolUnsupported = "unsupported"
	// requestAuditValueDetailProtocolBedrock 是**已知**的签名上游形态（AWS 签名，本能力无
	// 安全白名单，因此同样只记未支持）。它只能由账号事实或被显式覆盖的尝试得到。
	requestAuditValueDetailProtocolBedrock = "bedrock"
)

// requestAuditValueDetailWireProtocol 从逐次真实尝试与账号事实推导这次逻辑请求的出站 wire 形态。
//
// 空协议与未知/混合协议一律收敛成 unsupported，**绝不冒充 Bedrock**：把「不知道是什么协议」
// 写成 Bedrock 会伪造一个并不成立的签名上游结论（ADR 0007：无安全白名单的形态必须标未支持，
// 而不是借另一个已知形态的名字）。
func requestAuditValueDetailWireProtocol(attempts []service.RequestAuditValueDetailAttempt, bedrockAccount bool) string {
	wireProtocol := service.RequestAuditProtocolAnthropic
	if bedrockAccount {
		wireProtocol = requestAuditValueDetailProtocolBedrock
	}
	unknownWire := false
	for _, attempt := range attempts {
		switch attempt.Protocol {
		case service.RequestAuditProtocolAnthropic:
		case requestAuditValueDetailProtocolBedrock:
			wireProtocol = requestAuditValueDetailProtocolBedrock
		default:
			unknownWire = true
		}
	}
	if unknownWire {
		return requestAuditValueDetailProtocolUnsupported
	}
	return wireProtocol
}

// requestAuditValueDetailInboundRouteMatches 报告这次逻辑请求是否落在值明细覆盖的 Messages 入口上。
//
// 比较用**规范化后的入站路由**，而不是原始 URL path：/antigravity/v1/messages 与 /v1/messages
// 是同一个 Messages 入口（Antigravity 平台的 API-key 账号同样会产生真实逐次上游尝试），只比原始
// path 会让这些请求「有逐次审计、却没有值明细行」。
//
// 规范化会把 /v1/messages/count_tokens 也折叠成 /v1/messages（规格已点名的坑），因此这里再排除
// count_tokens 子路径：token 计数入口从来不是值明细的覆盖对象，也不能因为路径折叠被冒充成 Messages。
func requestAuditValueDetailInboundRouteMatches(c *gin.Context) bool {
	if GetInboundEndpoint(c) != service.RequestAuditValueDetailRouteMessages {
		return false
	}
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return true
	}
	return !strings.Contains(c.Request.URL.Path, "/count_tokens")
}

// Snapshot only after the handler has confirmed the request had real audited
// transport attempts. The worker must never access gin.Context after submit.
func requestAuditValueInputFromTransport(c *gin.Context, enabled bool, route, protocol, model string, startedAt time.Time, metadata service.RequestAuditMetadata, body ...[]byte) service.RequestAuditValueDetailInput {
	if !enabled || c == nil || c.Request == nil {
		return service.RequestAuditValueDetailInput{}
	}
	attempts := service.RequestAuditValueDetailAttemptsFromHTTPMetadata(service.RequestAuditHTTPAttemptMetadata(c))
	if len(attempts) == 0 {
		return service.RequestAuditValueDetailInput{}
	}
	// 入站头值与入站正文标识属于**客户端实际使用的入站协议**，由入站路由决定；
	// 出站形态只从逐次真实尝试的 wire 协议取。转换分支（Chat Completions→Anthropic、
	// Messages→OpenAI）里两者不同，用出站协议解释入站 wire 会把客户端合法头值整份丢弃。
	inboundProtocol, inboundSupported := service.RequestAuditValueDetailInboundProtocolForRoute(route)
	if !inboundSupported {
		return service.RequestAuditValueDetailInput{Route: route, Protocol: requestAuditValueDetailProtocolUnsupported, Attempts: attempts}
	}
	actualProtocol := ""
	for _, attempt := range attempts {
		if attempt.Protocol == "" {
			return service.RequestAuditValueDetailInput{Route: route, Protocol: requestAuditValueDetailProtocolUnsupported, Attempts: attempts}
		}
		if actualProtocol == "" {
			actualProtocol = attempt.Protocol
		} else if actualProtocol != attempt.Protocol {
			// Mixed wire protocols need per-attempt inbound contracts. Never use the
			// final account's protocol to reinterpret an earlier attempt.
			return service.RequestAuditValueDetailInput{Route: route, Protocol: requestAuditValueDetailProtocolUnsupported, Attempts: attempts}
		}
	}
	if protocol != actualProtocol {
		protocol = actualProtocol
	}
	inboundValues, inboundOmission, supported := service.RequestAuditValueDetailInboundHeadersForProtocol(c.Request.Header, inboundProtocol)
	if !supported {
		return service.RequestAuditValueDetailInput{Route: route, Protocol: requestAuditValueDetailProtocolUnsupported, Attempts: attempts}
	}
	input := service.RequestAuditValueDetailInput{
		Route: route, Protocol: protocol, InboundHeaderValues: inboundValues, InboundHeaderOmission: inboundOmission,
		Model: model, StartedAt: startedAt, CompletedAt: time.Now(), Attempts: attempts,
	}
	// Read only the parsed inbound identifier when available. The inbound body is
	// the client's request, so the shape gate is the **inbound** protocol, not the
	// protocol this request happened to be forwarded with. Other handlers pass
	// their bounded, actually-read request-body snapshot explicitly; the sidecar
	// validates parsed components again before persistence.
	if inboundProtocol == service.RequestAuditProtocolAnthropic {
		if len(body) > 0 {
			uid := gjson.GetBytes(body[0], "metadata.user_id")
			if uid.Type == gjson.String && len(uid.String()) <= 256 {
				input.MetadataUserID = uid.String()
			}
		} else if parsed, ok := c.Get("parsed_request"); ok {
			if request, ok := parsed.(*service.ParsedRequest); ok {
				input.MetadataUserID = request.MetadataUserID
			}
		}
	}
	if status, ok := metadata.Status[service.RequestAuditClientResponseKey]; ok {
		input.ClientStatus = status
	}
	return input
}
