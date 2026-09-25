//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/stretchr/testify/require"
)

const valueDetailTestMetadataUserID = `{"device_id":"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2","account_uuid":"550e8400-e29b-41d4-a716-446655440000","session_id":"123e4567-e89b-12d3-a456-426614174000"}`

func valueDetailOpenGate() RequestAuditValueDetailGate {
	return RequestAuditValueDetailGate{CaptureAllowed: true, EncryptionAvailable: true}
}

func valueDetailInboundHeaders() map[string]any {
	in := http.Header{}
	in.Set("User-Agent", "claude-cli/2.1.78 (darwin)")
	in.Set("X-Stainless-Retry-Count", "0")
	in.Set("Host", "api.anthropic.com")
	return httpattempt.SanitizeClaudeRequestHeaderValues(in)
}

func valueDetailScopeInput() RequestAuditValueDetailInput {
	return RequestAuditValueDetailInput{
		Route:               RequestAuditValueDetailRouteMessages,
		Protocol:            RequestAuditProtocolAnthropic,
		InboundHeaderValues: valueDetailInboundHeaders(),
		MetadataUserID:      valueDetailTestMetadataUserID,
		Model:               "claude-sonnet-4-5",
		ClientStatus:        200,
		StartedAt:           time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
		CompletedAt:         time.Date(2026, 9, 24, 10, 0, 5, 0, time.UTC),
		Attempts: []RequestAuditValueDetailAttempt{{
			Index:          1,
			AccountID:      42,
			Model:          "claude-sonnet-4-5",
			MetadataUserID: valueDetailTestMetadataUserID,
			UpstreamStatus: intPtrValueDetail(200),
			LatencyMillis:  int64PtrValueDetail(1234),
			ProxyID:        7,
			RequestHeaderValues: func() map[string]any {
				h := http.Header{}
				h.Set("X-Stainless-Retry-Count", "0")
				h.Set("Host", "api.anthropic.com")
				return httpattempt.SanitizeClaudeRequestHeaderValues(h)
			}(),
			ResponseHeaderValues: func() map[string]any {
				h := http.Header{}
				h.Set("Content-Type", "text/event-stream")
				h.Set("Request-Id", "req_abc123")
				h.Set("Anthropic-Ratelimit-Requests-Remaining", "9")
				return httpattempt.SanitizeClaudeResponseHeaderValues(h)
			}(),
		}},
	}
}

func intPtrValueDetail(v int) *int       { return &v }
func int64PtrValueDetail(v int64) *int64 { return &v }

// 默认关闭时不落任何行；只有 /v1/messages 入站才可能落行。
func TestBuildRequestAuditValueDetailWriteScopeAndGate(t *testing.T) {
	now := time.Now().UTC()

	require.Nil(t, BuildRequestAuditValueDetailWrite(RequestAuditValueDetailInput{
		Route: "/v1/chat/completions", Protocol: RequestAuditProtocolOpenAIChat,
	}, valueDetailOpenGate(), now), "非 /v1/messages 路由必须按未采集处理，不落空壳行")

	closed := BuildRequestAuditValueDetailWrite(valueDetailScopeInput(), RequestAuditValueDetailGate{}, now)
	require.NotNil(t, closed)
	require.Equal(t, RequestAuditValueDetailStateSkipped, closed.State)
	require.Equal(t, RequestAuditValueDetailSkippedRetentionDisabled, closed.Reason)
	require.False(t, closed.Retained())

	// 范围内路由但真实上游不是 Anthropic 形态：明确记不在范围，而不是未采集。
	outOfScope := BuildRequestAuditValueDetailWrite(func() RequestAuditValueDetailInput {
		in := valueDetailScopeInput()
		in.Protocol = RequestAuditProtocolOpenAIChat
		return in
	}(), valueDetailOpenGate(), now)
	require.NotNil(t, outOfScope)
	require.Equal(t, RequestAuditValueDetailSkippedOutOfScope, outOfScope.Reason)
}

// Bedrock 形态的真实上游不在本能力范围内：记 skipped_out_of_scope，一个值都不留。
// 即使调用方把完整的值（含 metadata.user_id、模型名与尝试级头值）交进来，也不得编码成载荷，
// 更不得把 UID／模型名带进任何明文列——「不在范围」必须与「存过值」可区分。
func TestBuildRequestAuditValueDetailWriteBedrockOutOfScopeRetainsNothing(t *testing.T) {
	now := time.Now().UTC()
	require.False(t, RequestAuditValueDetailScopeApplies(RequestAuditValueDetailRouteMessages, "bedrock"))
	require.True(t, RequestAuditValueDetailScopeApplies(RequestAuditValueDetailRouteMessages, RequestAuditProtocolAnthropic))

	in := valueDetailScopeInput()
	in.Protocol = "bedrock"
	in.InboundHeaderOmission = httpattempt.ClaudeHeaderValueOmission{OmittedNames: 3}
	write := BuildRequestAuditValueDetailWrite(in, valueDetailOpenGate(), now)
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailStateSkipped, write.State)
	require.Equal(t, RequestAuditValueDetailSkippedOutOfScope, write.Reason)
	require.False(t, write.Retained())
	require.Empty(t, write.Payload)
	require.Zero(t, write.EntryCount)
	require.Zero(t, write.AttemptCount)
	// 明文信封只记「这次真实上游是什么形态」，不记任何值。
	require.Equal(t, "bedrock", write.Fields.Protocol)

	raw, err := json.Marshal(write.Fields)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "a1b2c3d4e5f6")
	require.NotContains(t, string(raw), "claude-sonnet")
	require.NotContains(t, string(raw), "api.anthropic.com")

	// 对外披露：原因是闭集里的「不在范围」，绝不显示成「已留存」。
	require.Equal(t, RequestAuditValueDetailSkippedOutOfScope, requestAuditValueDetailDiscloseReason(write.Reason))
	require.Equal(t, RequestAuditValueDetailStateSkipped,
		DescribeRequestAuditValueDetailState(RequestAuditValueDetail{Reason: write.Reason}, now))
}

func TestBuildRequestAuditValueDetailWriteRetainsEncryptablePayload(t *testing.T) {
	now := time.Now().UTC()
	write := BuildRequestAuditValueDetailWrite(valueDetailScopeInput(), valueDetailOpenGate(), now)
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailStateStored, write.State)
	require.Equal(t, RequestAuditValueDetailRetained, write.Reason)
	require.True(t, write.Retained())
	require.Equal(t, now.Add(RequestAuditValueDetailRetention), write.ExpiresAt)
	require.Equal(t, "/v1/messages", write.Fields.Route)
	require.Equal(t, 200, write.Fields.ClientStatus)
	require.Equal(t, 1, write.AttemptCount)

	// 明文信封里没有模型名：它只在密文载荷里。
	raw, err := json.Marshal(write.Fields)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "claude-sonnet")

	values, err := DecodeRequestAuditValueDetailValues(write.Payload)
	require.NoError(t, err)
	require.Equal(t, "claude-sonnet-4-5", values.Model)
	require.Equal(t, "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", values.Inbound.DeviceID)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440000", values.Inbound.AccountUUID)
	require.Equal(t, "123e4567-e89b-12d3-a456-426614174000", values.Inbound.SessionID)
	require.Len(t, values.Attempts, 1)
	require.Equal(t, int64(1234), *values.Attempts[0].LatencyMillis)
	require.Equal(t, int64(7), values.Attempts[0].ProxyID)
	require.Equal(t, []string{"api.anthropic.com"}, values.Attempts[0].RequestHeaders["Host"])
	require.Equal(t, []string{"text/event-stream"}, values.Attempts[0].ResponseHeaders["Content-Type"])
	require.False(t, values.Truncated)
}

// 一条事实都没有时按「未采集」落库，绝不写成已留存。
func TestBuildRequestAuditValueDetailWriteEmptySnapshotIsNotObserved(t *testing.T) {
	now := time.Now().UTC()
	in := valueDetailScopeInput()
	in.InboundHeaderValues = nil
	in.MetadataUserID = ""
	in.Model = ""
	in.Attempts = nil
	write := BuildRequestAuditValueDetailWrite(in, valueDetailOpenGate(), now)
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailStateNotObserved, write.State)
	require.Equal(t, RequestAuditValueDetailNotObserved, write.Reason)
	require.False(t, write.Retained())
}

// 只有模型名也算留存了一条事实：它本身就在密文里，不需要头值陪衬。
func TestBuildRequestAuditValueDetailWriteRetainsModelOnly(t *testing.T) {
	now := time.Now().UTC()
	in := valueDetailScopeInput()
	in.InboundHeaderValues = nil
	in.MetadataUserID = ""
	in.Attempts = nil
	write := BuildRequestAuditValueDetailWrite(in, valueDetailOpenGate(), now)
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailStateStored, write.State)
	require.Equal(t, 1, write.EntryCount)

	values, err := DecodeRequestAuditValueDetailValues(write.Payload)
	require.NoError(t, err)
	require.Equal(t, "claude-sonnet-4-5", values.Model)
	require.Empty(t, values.Attempts)
}

// 开关开着但没有稳定密钥：留下稳定原因码，而不是退化成未采集，也绝不明文落库。
func TestBuildRequestAuditValueDetailWriteEncryptionUnavailable(t *testing.T) {
	now := time.Now().UTC()
	write := BuildRequestAuditValueDetailWrite(valueDetailScopeInput(), RequestAuditValueDetailGate{CaptureAllowed: true}, now)
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailStateSkipped, write.State)
	require.Equal(t, RequestAuditValueDetailSkippedEncryptionUnavailable, write.Reason)
	require.Empty(t, write.Payload)
	require.NotZero(t, write.EntryCount)
}

// 尝试数超上限时整份不采：不把前 8 次冒充成「全部尝试」。
func TestBuildRequestAuditValueDetailWriteTooManyAttempts(t *testing.T) {
	now := time.Now().UTC()
	in := valueDetailScopeInput()
	in.Attempts = make([]RequestAuditValueDetailAttempt, RequestAuditValueDetailMaxAttempts+1)
	for i := range in.Attempts {
		in.Attempts[i] = RequestAuditValueDetailAttempt{Index: i + 1, LatencyMillis: int64PtrValueDetail(int64(i))}
	}
	write := BuildRequestAuditValueDetailWrite(in, valueDetailOpenGate(), now)
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailSkippedTooManyAttempts, write.Reason)
	require.Empty(t, write.Payload)
}

// 尝试数超上限时整份不采，但那一行**必须写得进去**：attempt_count 是有界观察值，
// 17、40 次都不得越出存储层的检查约束（迁移 253 的 attempt_count <= 16），
// 也不得因此让整条 skipped_too_many_attempts 事实被数据库拒收。
func TestBuildRequestAuditValueDetailWriteBoundsTooManyAttemptsCount(t *testing.T) {
	now := time.Now().UTC()
	for _, attemptCount := range []int{
		RequestAuditValueDetailMaxAttempts + 1,
		RequestAuditValueDetailMaxStoredAttemptCount + 1,
		RequestAuditValueDetailMaxStoredAttemptCount * 3,
	} {
		in := valueDetailScopeInput()
		in.Attempts = make([]RequestAuditValueDetailAttempt, attemptCount)
		for i := range in.Attempts {
			in.Attempts[i] = RequestAuditValueDetailAttempt{Index: i + 1, LatencyMillis: int64PtrValueDetail(int64(i))}
		}
		write := BuildRequestAuditValueDetailWrite(in, valueDetailOpenGate(), now)
		require.NotNil(t, write)
		require.Equal(t, RequestAuditValueDetailSkippedTooManyAttempts, write.Reason)
		require.Empty(t, write.Payload)
		require.LessOrEqual(t, write.AttemptCount, RequestAuditValueDetailMaxStoredAttemptCount,
			"越界的计数会让整行被检查约束拒收，%d 次尝试时必须饱和", attemptCount)
		// 边界内是**精确**的实际尝试数：9..16 次不会被冒充成上限 8。
		require.Equal(t, minIntValueDetail(attemptCount, RequestAuditValueDetailMaxStoredAttemptCount), write.AttemptCount)
	}
}

func minIntValueDetail(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// DB 契约：服务层的存储上限必须与迁移 253 的检查约束逐值一致。
// 迁移只固定存储形状，服务层负责不越界；两者漂移的那一天，这一行会先失败。
func TestRequestAuditValueDetailStorageBoundMatchesMigrationCheck(t *testing.T) {
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "253_request_audit_value_details.sql"))
	require.NoError(t, err)
	sql := string(migration)
	require.Contains(t, sql, "attempt_count <= "+strconv.Itoa(RequestAuditValueDetailMaxStoredAttemptCount),
		"服务层存储上限必须与迁移的检查约束一致")
	require.LessOrEqual(t, RequestAuditValueDetailMaxAttempts, RequestAuditValueDetailMaxStoredAttemptCount,
		"采集上限不得宽于存储上限")
	// 越界的计数不是被拒绝就是被静默改写成别的数字：饱和是唯一诚实的收敛方式。
	require.Equal(t, 0, requestAuditValueDetailBoundedAttemptCount(-1))
	require.Equal(t, RequestAuditValueDetailMaxStoredAttemptCount,
		requestAuditValueDetailBoundedAttemptCount(RequestAuditValueDetailMaxStoredAttemptCount+1))
}

// ---- 省略摘要（truncated）的入口 ----

// 服务层看到的头值已被净化器筛过：未知头名与没通过校验的取值在那里根本不存在，
// 因此「有条目没被收下」只能由采集侧带进来，否则载荷里的 truncated 永远是 false。
func TestBuildRequestAuditValueDetailWriteMarksTruncatedFromIntakeOmission(t *testing.T) {
	now := time.Now().UTC()

	in := valueDetailScopeInput()
	in.InboundHeaderOmission = httpattempt.ClaudeHeaderValueOmission{OmittedNames: 1}
	write := BuildRequestAuditValueDetailWrite(in, valueDetailOpenGate(), now)
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailStateStored, write.State)
	values, err := DecodeRequestAuditValueDetailValues(write.Payload)
	require.NoError(t, err)
	require.True(t, values.Truncated, "入站阶段丢过条目，载荷必须如实标记")

	// 只有尝试级的省略时同样要标记。
	in = valueDetailScopeInput()
	in.Attempts[0].HeaderOmission = httpattempt.ClaudeHeaderValueOmission{RejectedValues: 1}
	write = BuildRequestAuditValueDetailWrite(in, valueDetailOpenGate(), now)
	require.NotNil(t, write)
	values, err = DecodeRequestAuditValueDetailValues(write.Payload)
	require.NoError(t, err)
	require.True(t, values.Truncated)

	// 没有观察到任何省略时，载荷不得凭空标成不完整。
	write = BuildRequestAuditValueDetailWrite(valueDetailScopeInput(), valueDetailOpenGate(), now)
	require.NotNil(t, write)
	values, err = DecodeRequestAuditValueDetailValues(write.Payload)
	require.NoError(t, err)
	require.False(t, values.Truncated)
}

// 入站采集接缝：快照与省略摘要成对产出，且摘要里没有名字与取值。
func TestRequestAuditValueDetailInboundHeadersPairsSnapshotWithOmission(t *testing.T) {
	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/2.1.258 (external, cli)")
	headers.Set("X-Unknown-Private", "private-unknown-value")
	headers.Set("Authorization", "Bearer top-secret")

	values, omission := RequestAuditValueDetailInboundHeaders(headers)
	require.Equal(t, "claude-cli/2.1.258 (external, cli)", values["User-Agent"])
	require.Equal(t, map[string]any{"present": true}, values["Authorization"])
	require.Equal(t, httpattempt.ClaudeHeaderValueOmission{OmittedNames: 1}, omission)
	encoded, err := json.Marshal(omission)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private")
	require.NotContains(t, string(encoded), "Unknown")
}

// 尝试级省略摘要来自传输层元数据，并且请求与响应合并成一个结论；
// 只有省略、没有任何取值的尝试也要留下——那正是「客户端发了我们没存的东西」的证据。
func TestRequestAuditValueDetailAttemptsCarryHeaderOmission(t *testing.T) {
	attempts := RequestAuditValueDetailAttemptsFromHTTPMetadata([]httpattempt.Metadata{{
		AccountID:                   1,
		Model:                       "claude-sonnet-4-5",
		Protocol:                    RequestAuditProtocolAnthropic,
		RequestHeaderValueOmission:  httpattempt.ClaudeHeaderValueOmission{OmittedNames: 2},
		ResponseHeaderValueOmission: httpattempt.ClaudeHeaderValueOmission{RejectedValues: 1},
	}})
	require.Len(t, attempts, 1)
	require.Equal(t, httpattempt.ClaudeHeaderValueOmission{OmittedNames: 2, RejectedValues: 1}, attempts[0].HeaderOmission)
	require.True(t, attempts[0].HeaderOmission.Any())

	withoutOmission := RequestAuditValueDetailAttemptsFromHTTPMetadata([]httpattempt.Metadata{{
		AccountID:     2,
		LatencyMillis: int64PtrValueDetail(5),
	}})
	require.Len(t, withoutOmission, 1)
	require.False(t, withoutOmission[0].HeaderOmission.Any())
}

// 未知头名由净化器丢弃，不让整份快照失败；凭据类头只留存在性，值一律不落库。
func TestNormalizeRequestAuditValueDetailHeaderValuesDropsUnknownAndCredentials(t *testing.T) {
	raw := http.Header{}
	raw.Set("X-Totally-Unknown", "some client value")
	raw.Set("Authorization", "Bearer sk-ant-super-secret")
	raw.Set("X-Api-Key", "sk-ant-super-secret")
	raw.Set("Cookie", "session=abc")
	raw.Set("Host", "api.anthropic.com")
	sanitized := httpattempt.SanitizeClaudeRequestHeaderValues(raw)

	values, ok := NormalizeRequestAuditValueDetailValues(RequestAuditValueDetailValues{
		Inbound: RequestAuditValueDetailInboundValues{
			RequestHeaders: requestAuditValueDetailHeaderStrings(sanitized),
		},
	})
	require.True(t, ok)
	require.Equal(t, map[string][]string{"Host": {"api.anthropic.com"}}, values.Inbound.RequestHeaders)
	require.False(t, values.Truncated, "未知头被净化器丢弃不算丢过条目")

	encoded, err := json.Marshal(values)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "sk-ant")
	require.NotContains(t, string(encoded), "session=abc")
	require.NotContains(t, string(encoded), "Bearer")
	require.NotContains(t, string(encoded), "X-Totally-Unknown")
}

// 多值头原样保留（有界），单值头是单元素列表，不取第一行也不静默截断。
func TestNormalizeRequestAuditValueDetailHeaderValuesKeepsBoundedLists(t *testing.T) {
	values, ok := NormalizeRequestAuditValueDetailValues(RequestAuditValueDetailValues{
		Inbound: RequestAuditValueDetailInboundValues{
			RequestHeaders: map[string][]string{
				"User-Agent": {"claude-cli/2.1.78 (darwin)", "claude-cli/2.1.79 (linux)"},
				"Host":       {"api.anthropic.com"},
			},
		},
	})
	require.True(t, ok)
	require.Equal(t, []string{"claude-cli/2.1.78 (darwin)", "claude-cli/2.1.79 (linux)"}, values.Inbound.RequestHeaders["User-Agent"])
	require.Equal(t, []string{"api.anthropic.com"}, values.Inbound.RequestHeaders["Host"])
	require.False(t, values.Truncated)
}

// 超出净化器取值个数上限的形状无法唯一表示：整份判不合格，绝不部分写入。
func TestNormalizeRequestAuditValueDetailHeaderValuesRejectsUnrepresentableShapes(t *testing.T) {
	_, ok := NormalizeRequestAuditValueDetailValues(RequestAuditValueDetailValues{
		Inbound: RequestAuditValueDetailInboundValues{
			RequestHeaders: map[string][]string{"Host": {"a", "b", "c", "d", "e"}},
		},
	})
	require.False(t, ok)

	_, ok = NormalizeRequestAuditValueDetailValues(RequestAuditValueDetailValues{
		Inbound: RequestAuditValueDetailInboundValues{
			RequestHeaders: map[string][]string{"Host": {}},
		},
	})
	require.False(t, ok)

	// 头值路径**不**做值内凭据形态的模糊扫描：闭集外的名字由净化器丢弃即可，
	// 不得因为它顺带承载了像凭据的文本就把整份快照判失败。
	values, ok := NormalizeRequestAuditValueDetailValues(RequestAuditValueDetailValues{
		Inbound: RequestAuditValueDetailInboundValues{
			RequestHeaders: map[string][]string{
				"Host":              {"api.anthropic.com"},
				"api.anthropic.com": {"Bearer sk-live-token"},
			},
		},
	})
	require.True(t, ok)
	require.Equal(t, map[string][]string{"Host": {"api.anthropic.com"}}, values.Inbound.RequestHeaders)
	require.True(t, values.Truncated, "闭集外的名字丢掉了，必须如实标记")
}

// 有界多值列表（2 个取值）是合法形状，原样保留。
func TestRequestAuditValueDetailHeaderStringsKeepsTwoValues(t *testing.T) {
	converted := requestAuditValueDetailHeaderStrings(map[string]any{
		"User-Agent": []any{"claude-cli/2.1.78 (darwin)", "claude-cli/2.1.79 (linux)"},
	})
	values, ok := NormalizeRequestAuditValueDetailValues(RequestAuditValueDetailValues{
		Inbound: RequestAuditValueDetailInboundValues{RequestHeaders: converted},
	})
	require.True(t, ok)
	require.Len(t, values.Inbound.RequestHeaders["User-Agent"], 2)
}

// 无法唯一表示的形状（取值个数超上限、元素非字符串）用哨兵标记整份失败，
// 而不是被静默截断。
func TestRequestAuditValueDetailHeaderStringsMarksInvalidShape(t *testing.T) {
	for _, raw := range []map[string]any{
		{"Host": []any{"a", "b", "c", "d", "e"}},
		{"Host": []any{"a", 1}},
		{"Host": []any{}},
	} {
		converted := requestAuditValueDetailHeaderStrings(raw)
		_, ok := NormalizeRequestAuditValueDetailValues(RequestAuditValueDetailValues{
			Inbound: RequestAuditValueDetailInboundValues{RequestHeaders: converted},
		})
		require.False(t, ok, "形状不合格必须整份拒绝：%v", raw)
	}
}

// 被净化器丢弃的条目必须在载荷上留下 truncated，不能藏起来。
func TestNormalizeRequestAuditValueDetailValuesMarksTruncated(t *testing.T) {
	values, ok := NormalizeRequestAuditValueDetailValues(RequestAuditValueDetailValues{
		Inbound: RequestAuditValueDetailInboundValues{
			RequestHeaders: map[string][]string{
				"Host":            {"api.anthropic.com"},
				"X-Totally-Bogus": {"whatever"},
			},
		},
	})
	require.True(t, ok)
	require.True(t, values.Truncated)
	require.Equal(t, map[string][]string{"Host": {"api.anthropic.com"}}, values.Inbound.RequestHeaders)
}

func TestNormalizeRequestAuditValueDetailIdentifierAndModelShapes(t *testing.T) {
	_, ok := NormalizeRequestAuditValueDetailValues(RequestAuditValueDetailValues{
		Inbound: RequestAuditValueDetailInboundValues{DeviceID: "has space"},
	})
	require.False(t, ok)

	_, ok = NormalizeRequestAuditValueDetailValues(RequestAuditValueDetailValues{
		Inbound: RequestAuditValueDetailInboundValues{SessionID: "Bearer sk-x"},
	})
	require.False(t, ok)

	// 不透明客户端标识碰巧含 key／secret 之类的**通用词**不算凭据，不得被整份拒绝：
	// 只有无歧义的凭据前缀才拒绝。误伤一个合法标识比多留一条普通标识危险得多。
	for _, id := range []string{"api-key-device-01", "public-key-session", "secret-santa-session"} {
		values, ok := NormalizeRequestAuditValueDetailValues(RequestAuditValueDetailValues{
			Inbound: RequestAuditValueDetailInboundValues{DeviceID: id},
		})
		require.True(t, ok, "标识 %q 不得被当成凭据", id)
		require.Equal(t, id, values.Inbound.DeviceID)
	}

	values, ok := NormalizeRequestAuditValueDetailValues(RequestAuditValueDetailValues{
		Model:   "claude-sonnet-4-5",
		Inbound: RequestAuditValueDetailInboundValues{AccountUUID: ""},
	})
	require.True(t, ok)
	require.Equal(t, "claude-sonnet-4-5", values.Model)
}

// 真实 Claude 客户端的 anthropic-beta 取值必须原样留存。
//
// 这是对「值内凭据形态模糊扫描」的回归测试：`token-counting-2024-11-01` 这类特性名
// 一旦被子串匹配误判，整份快照就会被判为不合格，管理员会看到「没有留存」而不是这些值。
func TestNormalizeRequestAuditValueDetailValuesKeepsRealClaudeBetaHeader(t *testing.T) {
	raw := http.Header{}
	raw.Set("Anthropic-Beta", claude.DefaultBetaHeader+","+claude.BetaTokenCounting)
	sanitized := httpattempt.SanitizeClaudeRequestHeaderValues(raw)
	require.NotEmpty(t, sanitized["Anthropic-Beta"], "净化器必须保留 anthropic-beta 的值")

	values, ok := NormalizeRequestAuditValueDetailValues(RequestAuditValueDetailValues{
		Inbound: RequestAuditValueDetailInboundValues{
			RequestHeaders: requestAuditValueDetailHeaderStrings(sanitized),
		},
	})
	require.True(t, ok, "真实 beta 取值不得让整份快照失败")
	require.False(t, values.Truncated)
	beta := strings.Join(values.Inbound.RequestHeaders["Anthropic-Beta"], ",")
	require.Contains(t, beta, claude.BetaTokenCounting)
	require.Contains(t, beta, claude.BetaClaudeCode)

	// 只含 token-counting 的单个取值同样必须留下。
	single := http.Header{}
	single.Set("Anthropic-Beta", claude.BetaTokenCounting)
	singleValues, ok := NormalizeRequestAuditValueDetailValues(RequestAuditValueDetailValues{
		Inbound: RequestAuditValueDetailInboundValues{
			RequestHeaders: requestAuditValueDetailHeaderStrings(httpattempt.SanitizeClaudeRequestHeaderValues(single)),
		},
	})
	require.True(t, ok)
	require.Equal(t, []string{claude.BetaTokenCounting}, singleValues.Inbound.RequestHeaders["Anthropic-Beta"])
}

// 凭据类头的值永远不落库：即使调用方把原始 http.Header 交给形状收敛，也只能得到
// 存在性标记，而存在性不是值。
func TestRequestAuditValueDetailNeverPersistsCredentialHeaderValues(t *testing.T) {
	raw := http.Header{}
	raw.Set("Authorization", "Bearer sk-ant-super-secret")
	raw.Set("X-Api-Key", "sk-ant-super-secret")
	raw.Set("Cookie", "session=xyz")
	raw.Set("Proxy-Authorization", "Basic abc")
	converted := requestAuditValueDetailHeaderStrings(httpattempt.SanitizeClaudeRequestHeaderValues(raw))
	require.Empty(t, converted)

	values, ok := NormalizeRequestAuditValueDetailValues(RequestAuditValueDetailValues{
		Inbound: RequestAuditValueDetailInboundValues{RequestHeaders: converted},
	})
	require.True(t, ok)
	require.Empty(t, values.Inbound.RequestHeaders)
	require.False(t, values.Truncated, "凭据头的存在性标记不算丢过条目")
}

// 解密成功不等于内容可信：未知字段、越界载荷与到期后的读取都必须失败。
func TestDecodeRequestAuditValueDetailValuesRejectsTamperedPayloads(t *testing.T) {
	_, err := DecodeRequestAuditValueDetailValues([]byte(`{"model":"m","unknown":1}`))
	require.Error(t, err)

	_, err = DecodeRequestAuditValueDetailValues([]byte(`{"model":"m"} trailing`))
	require.Error(t, err)

	_, err = DecodeRequestAuditValueDetailValues(nil)
	require.Error(t, err)

	_, err = DecodeRequestAuditValueDetailValues(make([]byte, RequestAuditValueDetailMaxReadBytes+1))
	require.Error(t, err)

	valid, err := EncodeRequestAuditValueDetailValues(RequestAuditValueDetailValues{Model: "claude-sonnet-4-5"})
	require.NoError(t, err)
	decoded, err := DecodeRequestAuditValueDetailValues(valid)
	require.NoError(t, err)
	require.Equal(t, "claude-sonnet-4-5", decoded.Model)
}

// 确定性：同一份值必须产生同一份载荷，否则「以密文对比重复」会成为旁路。
func TestEncodeRequestAuditValueDetailValuesIsDeterministic(t *testing.T) {
	values := RequestAuditValueDetailValues{
		Inbound: RequestAuditValueDetailInboundValues{
			RequestHeaders: map[string][]string{"Host": {"api.anthropic.com"}, "Accept": {"application/json"}},
		},
	}
	first, err := EncodeRequestAuditValueDetailValues(values)
	require.NoError(t, err)
	second, err := EncodeRequestAuditValueDetailValues(values)
	require.NoError(t, err)
	require.Equal(t, string(first), string(second))
}

func TestDescribeRequestAuditValueDetailState(t *testing.T) {
	now := time.Now().UTC()
	stored := RequestAuditValueDetail{Stored: true, Reason: RequestAuditValueDetailRetained, ExpiresAt: now.Add(time.Hour)}
	require.Equal(t, RequestAuditValueDetailStateStored, DescribeRequestAuditValueDetailState(stored, now))

	expired := stored
	expired.ExpiresAt = now.Add(-time.Second)
	require.Equal(t, RequestAuditValueDetailStateExpired, DescribeRequestAuditValueDetailState(expired, now))

	purged := RequestAuditValueDetail{Reason: RequestAuditValueDetailRetained}
	require.Equal(t, RequestAuditValueDetailStatePurged, DescribeRequestAuditValueDetailState(purged, now))

	skipped := RequestAuditValueDetail{Reason: RequestAuditValueDetailSkippedRetentionDisabled}
	require.Equal(t, RequestAuditValueDetailStateSkipped, DescribeRequestAuditValueDetailState(skipped, now))

	require.Equal(t, RequestAuditValueDetailStateNotObserved, DescribeRequestAuditValueDetailState(RequestAuditValueDetail{}, now))
}

// AttemptsFromHTTPMetadata 只丢弃真正什么都没有的尝试；耗时／代理 ID 也算事实。
func TestRequestAuditValueDetailAttemptsFromHTTPMetadata(t *testing.T) {
	metadata := []httpattempt.Metadata{
		{AccountID: 1, Model: "m", Protocol: RequestAuditProtocolAnthropic},
		{AccountID: 2, Model: "m", Protocol: RequestAuditProtocolAnthropic, LatencyMillis: int64PtrValueDetail(10)},
		{AccountID: 3, Model: "m", Protocol: RequestAuditProtocolAnthropic, ProxyID: 9},
	}
	attempts := RequestAuditValueDetailAttemptsFromHTTPMetadata(metadata)
	require.Len(t, attempts, 2)
	require.Equal(t, 2, attempts[0].Index, "序号必须保留原始位置，不能因为跳过而重排")
	require.Equal(t, int64(10), *attempts[0].LatencyMillis)
	require.Equal(t, 3, attempts[1].Index)
	require.Equal(t, int64(9), attempts[1].ProxyID)
	require.Empty(t, RequestAuditValueDetailAttemptsFromHTTPMetadata(nil))
}

// ---- 采集接缝 ----

type stubValueDetailRepo struct {
	created []RequestAuditValueDetailWrite
	err     error
}

func (s *stubValueDetailRepo) CreateRequestAuditValueDetail(_ context.Context, write RequestAuditValueDetailWrite) (RequestAuditValueDetail, error) {
	if s.err != nil {
		return RequestAuditValueDetail{}, s.err
	}
	s.created = append(s.created, write)
	return RequestAuditValueDetail{UsageLogID: write.UsageLogID}, nil
}

func (s *stubValueDetailRepo) GetRequestAuditValueDetail(context.Context, int64) (RequestAuditValueDetail, error) {
	return RequestAuditValueDetail{}, ErrRequestAuditValueDetailNotFound
}

func (s *stubValueDetailRepo) ReadRequestAuditValueDetailValues(context.Context, int64, time.Time) (RequestAuditValueDetailValues, error) {
	return RequestAuditValueDetailValues{}, ErrRequestAuditValueDetailGone
}

func (s *stubValueDetailRepo) ClearExpiredRequestAuditValueDetails(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

type stubValueDetailGate struct{ gate RequestAuditValueDetailGate }

func (s stubValueDetailGate) RequestAuditValueDetailGate(context.Context) RequestAuditValueDetailGate {
	return s.gate
}

func TestCaptureIsNoOpWhenGateClosed(t *testing.T) {
	repo := &stubValueDetailRepo{}
	capture := NewRequestAuditValueDetailCapture(repo, stubValueDetailGate{})
	require.False(t, capture.Enabled(context.Background()))
	capture.Capture(context.Background(), &UsageLog{ID: 10}, valueDetailScopeInput())
	require.Empty(t, repo.created, "门控关闭时不得写任何行")
}

func TestCaptureWritesAfterGateOpen(t *testing.T) {
	repo := &stubValueDetailRepo{}
	capture := NewRequestAuditValueDetailCapture(repo, stubValueDetailGate{gate: valueDetailOpenGate()})
	require.True(t, capture.Enabled(context.Background()))
	capture.Capture(context.Background(), &UsageLog{ID: 11}, valueDetailScopeInput())
	require.Len(t, repo.created, 1)
	require.Equal(t, int64(11), repo.created[0].UsageLogID)
	require.True(t, repo.created[0].Retained())
}

// fail-open：写入失败不得 panic，也不得把错误交给调用方。
func TestCaptureSwallowsWriteFailures(t *testing.T) {
	repo := &stubValueDetailRepo{err: errors.New("storage down")}
	capture := NewRequestAuditValueDetailCapture(repo, stubValueDetailGate{gate: valueDetailOpenGate()})
	require.NotPanics(t, func() {
		capture.Capture(context.Background(), &UsageLog{ID: 12}, valueDetailScopeInput())
	})

	// nil 接收者、nil 仓储与 nil 使用记录都是安全的空操作。
	var nilCapture *RequestAuditValueDetailCapture
	require.False(t, nilCapture.Enabled(context.Background()))
	require.NotPanics(t, func() {
		nilCapture.Capture(context.Background(), &UsageLog{ID: 1}, valueDetailScopeInput())
		NewRequestAuditValueDetailCapture(nil, nil).Capture(context.Background(), &UsageLog{ID: 1}, valueDetailScopeInput())
		capture.Capture(context.Background(), nil, valueDetailScopeInput())
		capture.Capture(context.Background(), &UsageLog{}, valueDetailScopeInput())
	})
}

// ---- 读取接缝 ----

type stubValueDetailReadRepo struct {
	detail  RequestAuditValueDetail
	err     error
	values  RequestAuditValueDetailValues
	readErr error
}

func (s *stubValueDetailReadRepo) CreateRequestAuditValueDetail(context.Context, RequestAuditValueDetailWrite) (RequestAuditValueDetail, error) {
	return RequestAuditValueDetail{}, nil
}

func (s *stubValueDetailReadRepo) GetRequestAuditValueDetail(context.Context, int64) (RequestAuditValueDetail, error) {
	if s.err != nil {
		return RequestAuditValueDetail{}, s.err
	}
	return s.detail, nil
}

func (s *stubValueDetailReadRepo) ReadRequestAuditValueDetailValues(context.Context, int64, time.Time) (RequestAuditValueDetailValues, error) {
	if s.readErr != nil {
		return RequestAuditValueDetailValues{}, s.readErr
	}
	return s.values, nil
}

func (s *stubValueDetailReadRepo) ClearExpiredRequestAuditValueDetails(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func TestValueDetailServiceEnvelopeAndReveal(t *testing.T) {
	now := time.Now().UTC()
	repo := &stubValueDetailReadRepo{
		detail: RequestAuditValueDetail{
			UsageLogID:   5,
			State:        RequestAuditValueDetailStateStored,
			Reason:       RequestAuditValueDetailRetained,
			Stored:       true,
			ExpiresAt:    now.Add(time.Hour),
			CreatedAt:    now,
			EntryCount:   3,
			AttemptCount: 1,
			Fields: RequestAuditValueDetailFields{
				Route:        RequestAuditValueDetailRouteMessages,
				Protocol:     RequestAuditProtocolAnthropic,
				ClientStatus: 200,
				StartedAt:    now,
			},
		},
		values: RequestAuditValueDetailValues{Model: "claude-sonnet-4-5"},
	}
	svc := NewRequestAuditValueDetailService(repo, nil)

	envelope, err := svc.GetRequestAuditValueDetail(context.Background(), 5)
	require.NoError(t, err)
	require.Equal(t, RequestAuditValueDetailStateStored, envelope.State)
	require.Equal(t, RequestAuditValueDetailRetained, envelope.Reason)
	require.NotNil(t, envelope.ExpiresAt)
	require.False(t, envelope.CapabilityEnabled)

	// 默认视图不含值：信封类型里根本没有值字段。
	raw, err := json.Marshal(envelope)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "claude-sonnet")

	values, err := svc.RevealRequestAuditValueDetail(context.Background(), 5)
	require.NoError(t, err)
	require.Equal(t, "claude-sonnet-4-5", values.Model)
}

func TestValueDetailServiceRevealSeparatesGoneFromNotRetained(t *testing.T) {
	now := time.Now().UTC()
	purged := RequestAuditValueDetail{
		UsageLogID: 6, Reason: RequestAuditValueDetailRetained,
		ExpiresAt: now.Add(-time.Hour), CreatedAt: now.Add(-8 * 24 * time.Hour),
	}
	svc := NewRequestAuditValueDetailService(&stubValueDetailReadRepo{detail: purged}, nil)
	_, err := svc.RevealRequestAuditValueDetail(context.Background(), 6)
	require.ErrorIs(t, err, ErrRequestAuditValueDetailGone)

	skipped := RequestAuditValueDetail{UsageLogID: 7, Reason: RequestAuditValueDetailSkippedRetentionDisabled}
	svc = NewRequestAuditValueDetailService(&stubValueDetailReadRepo{detail: skipped}, nil)
	_, err = svc.RevealRequestAuditValueDetail(context.Background(), 7)
	require.ErrorIs(t, err, ErrRequestAuditValueDetailNotRetained)
}

func TestValueDetailServiceNotFoundAndUnavailable(t *testing.T) {
	empty := NewRequestAuditValueDetailService(&stubValueDetailReadRepo{}, nil)
	_, err := empty.GetRequestAuditValueDetail(context.Background(), 8)
	require.ErrorIs(t, err, ErrRequestAuditValueDetailNotFound)
	_, err = empty.RevealRequestAuditValueDetail(context.Background(), 8)
	require.ErrorIs(t, err, ErrRequestAuditValueDetailNotFound)

	failing := NewRequestAuditValueDetailService(&stubValueDetailReadRepo{err: ErrRequestAuditValueDetailUnavailable}, nil)
	_, err = failing.GetRequestAuditValueDetail(context.Background(), 8)
	require.ErrorIs(t, err, ErrRequestAuditValueDetailUnavailable)

	// 枚举闭集：未知原因码按 not_observed 披露，不回显任意存储字符串。
	require.Equal(t, RequestAuditValueDetailNotObserved, requestAuditValueDetailDiscloseReason("arbitrary stored text"))
	require.Equal(t, RequestAuditValueDetailRetained, requestAuditValueDetailDiscloseReason(RequestAuditValueDetailRetained))
}

func TestValueDetailServiceRevealPropagatesStorageFailure(t *testing.T) {
	now := time.Now().UTC()
	repo := &stubValueDetailReadRepo{
		detail: RequestAuditValueDetail{
			UsageLogID: 9, Reason: RequestAuditValueDetailRetained, Stored: true,
			ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		},
		readErr: ErrRequestAuditValueDetailUnavailable,
	}
	svc := NewRequestAuditValueDetailService(repo, nil)
	_, err := svc.RevealRequestAuditValueDetail(context.Background(), 9)
	require.ErrorIs(t, err, ErrRequestAuditValueDetailUnavailable, "缺密钥与已到期必须分开")
}

// 空载荷与 nil 载荷不得被当成「已留存」。
func TestRequestAuditValueDetailWriteRetainedRequiresPayload(t *testing.T) {
	require.False(t, RequestAuditValueDetailWrite{State: RequestAuditValueDetailStateStored}.Retained())
	require.False(t, RequestAuditValueDetailWrite{State: RequestAuditValueDetailStateSkipped, Payload: []byte("x")}.Retained())
	require.True(t, RequestAuditValueDetailWrite{State: RequestAuditValueDetailStateStored, Payload: []byte("x")}.Retained())
}

// 载荷上限：超限编码必须失败，而不是写出一个超大的密文。
func TestEncodeRequestAuditValueDetailValuesEnforcesPayloadCap(t *testing.T) {
	big := strings.Repeat("a", RequestAuditValueDetailMaxPayloadBytes)
	_, err := EncodeRequestAuditValueDetailValues(RequestAuditValueDetailValues{Model: big})
	require.Error(t, err)
}
