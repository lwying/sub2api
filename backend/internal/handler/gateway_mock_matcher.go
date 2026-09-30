package handler

import (
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 下游测试请求的判定与匹配。这里只做纯计算：输入请求体字节与设置快照，
// 输出是否命中以及要回复的固定文本；不发上游、不写库、不依赖线程状态。

// GatewayMockMatch 是一次匹配的结论。Matched 为 false 时其余字段无意义。
type GatewayMockMatch struct {
	Matched bool
	RuleID  string
	// RuleVersion 是命中规则内容的版本标记，用于在规则被改动后解释历史命中。
	RuleVersion string
	Keyword     string
	Reply       string
}

// MatchDownstreamTestRequest 判断请求是否整体只是"下游测试请求"并命中管理员配置的关键词。
// 只有单轮、恰一个用户纯文本块、且没有实质任务（系统提示、工具、附件、历史）时才参与匹配。
func MatchDownstreamTestRequest(body []byte, protocol service.GatewayMockProtocol, ruleset service.GatewayMockRuleset) GatewayMockMatch {
	if !ruleset.Enabled || len(ruleset.Rules) == 0 {
		return GatewayMockMatch{}
	}
	text, ok := extractSingleUserText(body, protocol)
	if !ok {
		return GatewayMockMatch{}
	}
	rule, found := ruleset.MatchReply(text)
	if !found {
		return GatewayMockMatch{}
	}
	return GatewayMockMatch{
		Matched: true, RuleID: rule.ID, RuleVersion: rule.UpdatedAt,
		Keyword: rule.Keyword, Reply: rule.Reply,
	}
}

// extractSingleUserText 按协议抽取唯一的用户纯文本块。
func extractSingleUserText(body []byte, protocol service.GatewayMockProtocol) (string, bool) {
	root := map[string]any{}
	if err := json.Unmarshal(body, &root); err != nil {
		return "", false
	}
	switch protocol {
	case service.GatewayMockProtocolMessages:
		return extractMessagesSingleUserText(root)
	case service.GatewayMockProtocolChatCompletions:
		return extractChatCompletionsSingleUserText(root)
	case service.GatewayMockProtocolResponses:
		return extractResponsesSingleUserText(root)
	default:
		return "", false
	}
}

// extractMessagesSingleUserText 处理 Anthropic Messages 形态。
func extractMessagesSingleUserText(root map[string]any) (string, bool) {
	if hasToolsField(root) {
		return "", false
	}
	systemOnlyProtocolBoilerplate, ok := messagesSystemIsProtocolBoilerplate(root)
	if !ok {
		return "", false
	}
	if !systemOnlyProtocolBoilerplate {
		return "", false
	}
	messages, ok := root["messages"].([]any)
	if !ok || len(messages) != 1 {
		return "", false
	}
	message, ok := messages[0].(map[string]any)
	if !ok {
		return "", false
	}
	if role, _ := message["role"].(string); role != "user" {
		return "", false
	}
	return singleTextValue(message["content"])
}

// messagesSystemIsProtocolBoilerplate 报告 system 字段是否存在实质任务。
// 返回 (true, true) 表示没有 system 或只有已知协议提醒；(false, true) 表示存在任务指令；
// (*, false) 表示形态无法确认。
func messagesSystemIsProtocolBoilerplate(root map[string]any) (bool, bool) {
	raw, present := root["system"]
	if !present || raw == nil {
		return true, true
	}
	switch value := raw.(type) {
	case string:
		return isProtocolBoilerplateText(value), true
	case []any:
		for _, block := range value {
			item, ok := block.(map[string]any)
			if !ok {
				return false, false
			}
			if blockType, _ := item["type"].(string); blockType != "text" {
				return false, false
			}
			text, _ := item["text"].(string)
			if !isProtocolBoilerplateText(text) {
				return false, false
			}
		}
		return true, true
	default:
		return false, false
	}
}

// isProtocolBoilerplateText 只把已知的协议提醒视为不含任务。其余任何 system 文本都按任务处理。
func isProtocolBoilerplateText(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return true
	}
	return strings.HasPrefix(trimmed, "<system-reminder>")
}

// extractChatCompletionsSingleUserText 处理 OpenAI Chat Completions 形态。
func extractChatCompletionsSingleUserText(root map[string]any) (string, bool) {
	if hasToolsField(root) {
		return "", false
	}
	messages, ok := root["messages"].([]any)
	if !ok || len(messages) == 0 {
		return "", false
	}
	count := 0
	text := ""
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			return "", false
		}
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer":
			content, ok := singleTextValue(message["content"])
			if !ok || !isProtocolBoilerplateText(content) {
				return "", false
			}
		case "user":
			content, ok := singleTextValue(message["content"])
			if !ok {
				return "", false
			}
			count++
			text = content
		default:
			// 已有助手或工具消息 = 历史对话，不属于下游测试请求。
			return "", false
		}
	}
	if count != 1 {
		return "", false
	}
	return text, true
}

// extractResponsesSingleUserText 处理 OpenAI Responses 形态。
func extractResponsesSingleUserText(root map[string]any) (string, bool) {
	if hasToolsField(root) {
		return "", false
	}
	raw, present := root["input"]
	if !present {
		return "", false
	}
	switch value := raw.(type) {
	case string:
		return value, true
	case []any:
		count := 0
		text := ""
		for _, item := range value {
			if scalar, ok := singleTextValue(item); ok {
				count++
				if count > 1 {
					return "", false
				}
				text = scalar
				continue
			}
			entry, ok := item.(map[string]any)
			if !ok {
				return "", false
			}
			entryType, _ := entry["type"].(string)
			switch entryType {
			case "message":
				role, _ := entry["role"].(string)
				content, ok := singleTextValue(entry["content"])
				if !ok {
					return "", false
				}
				if role == "system" || role == "developer" {
					if !isProtocolBoilerplateText(content) {
						return "", false
					}
					continue
				}
				if role != "user" {
					return "", false
				}
				count++
				text = content
			case "input_text":
				count++
				text, _ = entry["text"].(string)
			case "function_call", "function_call_output", "reasoning", "item_reference":
				return "", false
			default:
				return "", false
			}
		}
		if count != 1 {
			return "", false
		}
		return text, true
	default:
		return "", false
	}
}

// singleTextValue 只在字段确实是"单个纯文本块"时返回值。
// 字符串形态按单文本处理；数组形态必须恰有一个 type=text 的块。
func singleTextValue(raw any) (string, bool) {
	switch value := raw.(type) {
	case string:
		return value, true
	case []any:
		if len(value) != 1 {
			return "", false
		}
		block, ok := value[0].(map[string]any)
		if !ok {
			return "", false
		}
		if blockType, _ := block["type"].(string); blockType != "text" && blockType != "input_text" {
			return "", false
		}
		text, ok := block["text"].(string)
		return text, ok
	default:
		return "", false
	}
}

// hasToolsField 报告请求是否声明了工具/函数能力。
func hasToolsField(root map[string]any) bool {
	for _, key := range []string{"tools", "functions", "tool_choice", "function_call"} {
		if value, present := root[key]; present && value != nil {
			return true
		}
	}
	return false
}
