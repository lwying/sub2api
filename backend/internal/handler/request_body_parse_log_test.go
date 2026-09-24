//go:build unit

package handler

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func newObservedLogger(t *testing.T) (*zap.Logger, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.WarnLevel)
	return zap.New(core), logs
}

func loggedFields(t *testing.T, logs *observer.ObservedLogs) map[string]any {
	t.Helper()
	entries := logs.All()
	require.Len(t, entries, 1)
	fields := map[string]any{}
	for _, f := range entries[0].Context {
		switch f.Key {
		case "body_len":
			fields[f.Key] = int(f.Integer)
		case "error":
			fields[f.Key] = f.Interface.(error).Error()
		default:
			fields[f.Key] = f.String
		}
	}
	return fields
}

func TestLogRequestBodyParseFailure_DerivesErrorWhenNil(t *testing.T) {
	log, logs := newObservedLogger(t)
	body := []byte(`{"model": bad}`)

	logRequestBodyParseFailure(log, body, nil)

	fields := loggedFields(t, logs)
	require.Equal(t, len(body), fields["body_len"])
	require.Contains(t, fields["error"], "invalid json")
	require.Contains(t, fields["error"], "offset=11")
}

func TestLogRequestBodyParseFailure_ShortBodyHasNoTail(t *testing.T) {
	log, logs := newObservedLogger(t)
	body := []byte(`{"broken":`)

	logRequestBodyParseFailure(log, body, nil)

	fields := loggedFields(t, logs)
	require.Contains(t, fields, "body_head")
	require.NotContains(t, fields, "body_tail")
	require.Contains(t, fields["body_head"].(string), `{\"broken\":`)
}

func TestLogRequestBodyParseFailure_LargeBodyBoundedSnippets(t *testing.T) {
	log, logs := newObservedLogger(t)
	// ~1MB body: head must show the structural prefix, tail the trailing bytes,
	// and neither snippet may exceed the configured bound (plus quoting overhead).
	body := []byte(`{"model":"claude-sonnet-4-6","big":"` + strings.Repeat("A", 1<<20) + `"`)

	logRequestBodyParseFailure(log, body, nil)

	fields := loggedFields(t, logs)
	require.Equal(t, len(body), fields["body_len"])
	head := fields["body_head"].(string)
	tail := fields["body_tail"].(string)
	require.Contains(t, head, "claude-sonnet-4-6")
	require.Contains(t, tail, "AAA")
	require.NotContains(t, tail, "claude-sonnet-4-6")
	// strconv.Quote adds surrounding quotes and escapes; 4x is a generous cap.
	require.LessOrEqual(t, len(head), parseFailureSnippetLen*4)
	require.LessOrEqual(t, len(tail), parseFailureSnippetLen*4)
}

func TestLogRequestBodyParseFailure_EscapesControlCharacters(t *testing.T) {
	log, logs := newObservedLogger(t)
	body := []byte("{\"model\":\x01\n\"x\"}")

	logRequestBodyParseFailure(log, body, nil)

	fields := loggedFields(t, logs)
	head := fields["body_head"].(string)
	require.NotContains(t, head, "\n")
	require.NotContains(t, head, "\x01")
	require.Contains(t, head, `\n`)
	require.Contains(t, head, `\x01`)
}

func TestLogRequestBodyParseFailure_NilLoggerNoPanic(t *testing.T) {
	require.NotPanics(t, func() {
		logRequestBodyParseFailure(nil, []byte(`{`), nil)
	})
}

// Sentinel review（见 request_body_parse_log.go 顶部注释）：本路径保留既有的有界片段
// 行为，因此哨兵断言的是「暴露面有界」这一不变式——位于 head/tail 有界窗口之外的内容
// （合成哨兵，非真实凭据）不得进入日志。反过来说，落在首/尾 256 字节窗口内的内容仍会
// 被记录，这是注释中已声明的剩余风险。
func TestLogRequestBodyParseFailure_SentinelOutsideSnippetWindowNotLogged(t *testing.T) {
	log, logs := newObservedLogger(t)
	const sentinelCredential = "CANARY_MIDDLE_CREDENTIAL_5f1e"
	body := []byte(`{"model":"claude-sonnet-4-6","pad":"` + strings.Repeat("A", 2048) +
		`","fallback_credit_token":"` + sentinelCredential + `","pad2":"` + strings.Repeat("B", 2048) + `"}`)

	logRequestBodyParseFailure(log, body, nil)

	fields := loggedFields(t, logs)
	head, headOK := fields["body_head"].(string)
	tail, tailOK := fields["body_tail"].(string)
	require.True(t, headOK)
	require.True(t, tailOK)
	require.NotContains(t, head, sentinelCredential)
	require.NotContains(t, tail, sentinelCredential)
	// 日志始终是单行（转义后），不得因正文内容产生伪造行。
	require.NotContains(t, head, "\n")
	require.NotContains(t, tail, "\n")
}
