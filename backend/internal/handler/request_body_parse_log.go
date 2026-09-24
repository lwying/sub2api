package handler

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"go.uber.org/zap"
)

// parseFailureSnippetLen bounds the head/tail snippets logged on body parse
// failure. 256 bytes is enough to see the structural context (model field,
// first content block / trailing brace) without dumping user payloads.
const parseFailureSnippetLen = 256

// logRequestBodyParseFailure records the real reason a request body failed
// JSON parsing/validation. The client keeps receiving the generic
// "Failed to parse request body"; the sanitized diagnostics (underlying
// error with byte offset, body length, escaped head/tail snippets) land in
// the server log only, so operators can distinguish genuinely invalid JSON
// from a truncated or partially consumed body.
//
// err may be nil for call sites that validate with gjson.ValidBytes directly;
// the diagnostic error is derived from the body in that case.
//
// Sentinel review（ADR 0005／本地规格 error-request-diagnostics-readonly-accounts 的
// 「既有调试／错误日志敏感路径盘点」）：本路径只处理**解析失败**的请求体（该请求不会发往
// 上游，客户端只收到通用错误），输出被限制为固定 256 字节的 head／tail 片段并转义为单行。
// 由于出站正文从未形成，且片段是有界的结构性上下文，本处按仓库既有约定保留行为不变；
// 已知剩余风险是：畸形/被截断的客户端正文里的自由文本（理论上含客户端自行写入的未知密钥）
// 仍可能落在这两个有界片段内。是否进一步净化属规格中尚未决的「旧调试日志敏感信息处置」，
// 不在本次改动范围内，因此不要在此处扩大输出或放宽 parseFailureSnippetLen。
func logRequestBodyParseFailure(reqLog *zap.Logger, body []byte, err error) {
	if reqLog == nil {
		return
	}
	if err == nil {
		err = service.DescribeInvalidJSON(body)
	}

	head := body
	var tail []byte
	if len(body) > parseFailureSnippetLen {
		head = body[:parseFailureSnippetLen]
		tail = body[len(body)-parseFailureSnippetLen:]
	}

	fields := []zap.Field{
		zap.Error(err),
		zap.Int("body_len", len(body)),
		zap.String("body_head", sanitizeBodySnippet(head)),
	}
	if len(tail) > 0 {
		fields = append(fields, zap.String("body_tail", sanitizeBodySnippet(tail)))
	}
	reqLog.Warn("parse request body failed", fields...)
}

// sanitizeBodySnippet escapes control characters and invalid UTF-8 so the
// snippet is always a single printable log line.
func sanitizeBodySnippet(b []byte) string {
	return strconv.Quote(string(b))
}
