package handler

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 固定「下游测试请求」的关键词规则快照。关键词已在设置层归一化（去首尾空白、小写）。
func testMockRuleset() service.GatewayMockRuleset {
	return service.GatewayMockRuleset{
		Enabled: true,
		Rules:   []service.GatewayMockRule{{ID: "rule-hi", Keyword: "hi", Reply: "你好！"}},
	}
}

// Messages 单轮单文本请求体。
func messagesBody(content string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 256,
		"messages":   []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": content}}}},
	})
	return body
}

func TestMatchDownstreamTestRequest_ExactKeywordMatching(t *testing.T) {
	rules := testMockRuleset()

	t.Run("首尾空白与拉丁大小写归一后命中", func(t *testing.T) {
		match := MatchDownstreamTestRequest(messagesBody("  HI  "), service.GatewayMockProtocolMessages, rules)
		require.True(t, match.Matched, "首尾空白与大小写归一后应命中")
		require.Equal(t, "你好！", match.Reply)
		require.Equal(t, "rule-hi", match.RuleID)
	})

	t.Run("标点不忽略，不命中", func(t *testing.T) {
		match := MatchDownstreamTestRequest(messagesBody("Hi!"), service.GatewayMockProtocolMessages, rules)
		require.False(t, match.Matched, "标点不参与归一，不得命中")
	})

	t.Run("句子中的子串不命中", func(t *testing.T) {
		match := MatchDownstreamTestRequest(messagesBody("你好今天天气怎么样"), service.GatewayMockProtocolMessages, rules)
		require.False(t, match.Matched, "带实质任务的长句不得命中")
	})

	t.Run("问候夹带任务不命中", func(t *testing.T) {
		match := MatchDownstreamTestRequest(messagesBody("hi，请帮我检查这段代码"), service.GatewayMockProtocolMessages, rules)
		require.False(t, match.Matched)
	})

	t.Run("内部空白不折叠", func(t *testing.T) {
		match := MatchDownstreamTestRequest(messagesBody("h i"), service.GatewayMockProtocolMessages, rules)
		require.False(t, match.Matched, "中间空白不得折叠")
	})
}

func TestMatchDownstreamTestRequest_SingleTurnSingleTextBlockOnly(t *testing.T) {
	rules := testMockRuleset()

	t.Run("多轮对话不命中", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"model": "claude-sonnet-4-5",
			"messages": []any{
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}}},
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "你好"}}},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}}},
			},
		})
		require.False(t, MatchDownstreamTestRequest(body, service.GatewayMockProtocolMessages, rules).Matched)
	})

	t.Run("两个文本块不拼接命中", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"model": "claude-sonnet-4-5",
			"messages": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "h"},
				map[string]any{"type": "text", "text": "i"},
			}}},
		})
		require.False(t, MatchDownstreamTestRequest(body, service.GatewayMockProtocolMessages, rules).Matched)
	})

	t.Run("带工具调用不命中", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"model": "claude-sonnet-4-5",
			"tools": []any{map[string]any{"name": "read_file", "input_schema": map[string]any{"type": "object"}}},
			"messages": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "hi"},
			}}},
		})
		require.False(t, MatchDownstreamTestRequest(body, service.GatewayMockProtocolMessages, rules).Matched)
	})

	t.Run("带图片块不命中", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"model": "claude-sonnet-4-5",
			"messages": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "hi"},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AA"}},
			}}},
		})
		require.False(t, MatchDownstreamTestRequest(body, service.GatewayMockProtocolMessages, rules).Matched)
	})

	t.Run("带任务的系统提示不命中", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"model":  "claude-sonnet-4-5",
			"system": []any{map[string]any{"type": "text", "text": "You must always answer in JSON."}},
			"messages": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "hi"},
			}}},
		})
		require.False(t, MatchDownstreamTestRequest(body, service.GatewayMockProtocolMessages, rules).Matched)
	})
}

func TestMatchDownstreamTestRequest_DisabledRulesNeverMatch(t *testing.T) {
	rules := testMockRuleset()
	rules.Enabled = false
	require.False(t, MatchDownstreamTestRequest(messagesBody("hi"), service.GatewayMockProtocolMessages, rules).Matched)

	// 规则被禁用（不在快照内）时同样不命中
	rules = service.GatewayMockRuleset{Enabled: true, Rules: nil}
	require.False(t, MatchDownstreamTestRequest(messagesBody("hi"), service.GatewayMockProtocolMessages, rules).Matched)
}
