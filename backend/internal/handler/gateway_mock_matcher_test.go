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

// Responses 形态里"实质任务 / 历史续写"的保守判定。
// instructions 有任务或带 previous_response_id 时都不属于单轮测试请求；
// 形态无法确认时同样不命中（送回上游）。
func TestMatchDownstreamTestRequest_ResponsesTaskAndHistoryGuards(t *testing.T) {
	rules := testMockRuleset()

	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "带任务的 instructions 不命中",
			body: `{"model":"gpt-5.1","instructions":"You must always answer in JSON.","input":"hi"}`,
			want: false,
		},
		{
			name: "带任务的 instructions 数组不命中",
			body: `{"model":"gpt-5.1","instructions":[{"type":"text","text":"You are a coding agent."}],"input":"hi"}`,
			want: false,
		},
		{
			name: "previous_response_id 续写不命中",
			body: `{"model":"gpt-5.1","previous_response_id":"resp_123456","input":"hi"}`,
			want: false,
		},
		{
			name: "缺 type 的纯用户消息命中",
			body: `{"model":"gpt-5.1","input":[{"role":"user","content":"hi"}]}`,
			want: true,
		},
		{
			name: "缺 type 且无 role 的条目不命中",
			body: `{"model":"gpt-5.1","input":[{"content":"hi"}]}`,
			want: false,
		},
		{
			name: "缺 type 的开发者任务不命中",
			body: `{"model":"gpt-5.1","input":[{"role":"developer","content":"Always answer in JSON."},{"role":"user","content":"hi"}]}`,
			want: false,
		},
		{
			name: "纯空白 instructions 视为无任务",
			body: `{"model":"gpt-5.1","instructions":"   ","input":"hi"}`,
			want: true,
		},
		{
			name: "协议提醒 instructions 可忽略",
			body: `{"model":"gpt-5.1","instructions":"<system-reminder>health check</system-reminder>","input":"hi"}`,
			want: true,
		},
		{
			name: "空 previous_response_id 视为未续写",
			body: `{"model":"gpt-5.1","previous_response_id":"","input":"hi"}`,
			want: true,
		},
		{
			name: "previous_response_id 形态不明不命中",
			body: `{"model":"gpt-5.1","previous_response_id":{"id":"resp_1"},"input":"hi"}`,
			want: false,
		},
		{
			name: "显式 type 的既有命中不受影响",
			body: `{"model":"gpt-5.1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			want: true,
		},
		{
			name: "无任务字段的字符串 input 仍命中",
			body: `{"model":"gpt-5.1","input":"hi"}`,
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			match := MatchDownstreamTestRequest([]byte(tc.body), service.GatewayMockProtocolResponses, rules)
			require.Equal(t, tc.want, match.Matched)
			if tc.want {
				require.Equal(t, "你好！", match.Reply)
			}
		})
	}
}

// matcherProtocolCarrier 描述某个协议里"协议包装文本"落在哪个字段，
// 用于让同一组包装文本在 Messages / Chat Completions / Responses 上跑同一份判定。
type matcherProtocolCarrier struct {
	name     string
	protocol service.GatewayMockProtocol
	body     func(text string) []byte
}

// matcherBody 构造测试请求体；这些字面量不可能序列化失败。
func matcherBody(value map[string]any) []byte {
	body, _ := json.Marshal(value)
	return body
}

// 三种协议里协议包装文本的承载位置：system / developer 消息与 Responses 的 instructions。
func matcherProtocolCarriers() []matcherProtocolCarrier {
	userTextBlock := []any{map[string]any{"type": "text", "text": "hi"}}
	return []matcherProtocolCarrier{
		{
			name:     "Messages system 字符串",
			protocol: service.GatewayMockProtocolMessages,
			body: func(text string) []byte {
				return matcherBody(map[string]any{
					"model":    "claude-sonnet-4-5",
					"system":   text,
					"messages": []any{map[string]any{"role": "user", "content": userTextBlock}},
				})
			},
		},
		{
			name:     "Messages system 文本块",
			protocol: service.GatewayMockProtocolMessages,
			body: func(text string) []byte {
				return matcherBody(map[string]any{
					"model":    "claude-sonnet-4-5",
					"system":   []any{map[string]any{"type": "text", "text": text}},
					"messages": []any{map[string]any{"role": "user", "content": userTextBlock}},
				})
			},
		},
		{
			name:     "Chat system 消息",
			protocol: service.GatewayMockProtocolChatCompletions,
			body: func(text string) []byte {
				return matcherBody(map[string]any{
					"model": "gpt-5.1",
					"messages": []any{
						map[string]any{"role": "system", "content": text},
						map[string]any{"role": "user", "content": "hi"},
					},
				})
			},
		},
		{
			name:     "Chat developer 消息",
			protocol: service.GatewayMockProtocolChatCompletions,
			body: func(text string) []byte {
				return matcherBody(map[string]any{
					"model": "gpt-5.1",
					"messages": []any{
						map[string]any{"role": "developer", "content": text},
						map[string]any{"role": "user", "content": "hi"},
					},
				})
			},
		},
		{
			name:     "Responses instructions 字符串",
			protocol: service.GatewayMockProtocolResponses,
			body: func(text string) []byte {
				return matcherBody(map[string]any{
					"model": "gpt-5.1", "instructions": text, "input": "hi",
				})
			},
		},
		{
			name:     "Responses instructions 数组",
			protocol: service.GatewayMockProtocolResponses,
			body: func(text string) []byte {
				return matcherBody(map[string]any{
					"model":        "gpt-5.1",
					"instructions": []any{map[string]any{"type": "text", "text": text}},
					"input":        "hi",
				})
			},
		},
		{
			name:     "Responses developer 输入条目",
			protocol: service.GatewayMockProtocolResponses,
			body: func(text string) []byte {
				return matcherBody(map[string]any{
					"model": "gpt-5.1",
					"input": []any{
						map[string]any{"type": "message", "role": "developer", "content": text},
						map[string]any{"type": "message", "role": "user", "content": "hi"},
					},
				})
			},
		},
	}
}

// 只有整体由成对平衡的 <system-reminder> 元素组成的包装才算"无任务"；
// 闭合标签之后的任何散文都必须按任务处理，三种协议口径一致。
func TestMatchDownstreamTestRequest_OnlyBalancedProtocolRemindersAreBoilerplate(t *testing.T) {
	rules := testMockRuleset()

	cases := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "闭合标签后跟任务散文",
			text: "<system-reminder>ctx</system-reminder> You must always answer in JSON.",
			want: false,
		},
		{
			name: "任务散文后用换行跟提醒",
			text: "You must always answer in JSON.\n<system-reminder>ctx</system-reminder>",
			want: false,
		},
		{
			name: "单个闭合提醒",
			text: "<system-reminder>ctx</system-reminder>",
			want: true,
		},
		{
			name: "多个闭合提醒",
			text: "<system-reminder>a</system-reminder>\n  <system-reminder>b</system-reminder>",
			want: true,
		},
		{
			name: "提醒前后空白可忽略",
			text: "  <system-reminder>ctx</system-reminder>  ",
			want: true,
		},
		{
			name: "多余闭合标签",
			text: "<system-reminder>ctx</system-reminder></system-reminder>",
			want: false,
		},
		{
			name: "未闭合提醒",
			text: "<system-reminder>ctx",
			want: false,
		},
		{
			name: "嵌套提醒保守按任务处理",
			text: "<system-reminder>a<system-reminder>b</system-reminder></system-reminder>",
			want: false,
		},
		{
			name: "普通任务文本",
			text: "Always answer in JSON.",
			want: false,
		},
		{
			name: "空文本",
			text: "",
			want: true,
		},
		{
			name: "纯空白",
			text: "   \n",
			want: true,
		},
	}

	for _, carrier := range matcherProtocolCarriers() {
		for _, tc := range cases {
			t.Run(carrier.name+"/"+tc.name, func(t *testing.T) {
				match := MatchDownstreamTestRequest(carrier.body(tc.text), carrier.protocol, rules)
				require.Equal(t, tc.want, match.Matched)
				if tc.want {
					require.Equal(t, "你好！", match.Reply)
				}
			})
		}
	}
}

func TestMatchDownstreamTestRequest_DisabledRulesNeverMatch(t *testing.T) {
	rules := testMockRuleset()
	rules.Enabled = false
	require.False(t, MatchDownstreamTestRequest(messagesBody("hi"), service.GatewayMockProtocolMessages, rules).Matched)

	// 规则被禁用（不在快照内）时同样不命中
	rules = service.GatewayMockRuleset{Enabled: true, Rules: nil}
	require.False(t, MatchDownstreamTestRequest(messagesBody("hi"), service.GatewayMockProtocolMessages, rules).Matched)
}
