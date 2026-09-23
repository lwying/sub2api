package service

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/tidwall/gjson"
)

// ThinkingDisabledFormMode 描述 thinking.type=disabled 时多余键的处理方式。
type ThinkingDisabledFormMode int

const (
	// ThinkingDisabledFormModeNormalize 删除多余键后继续转发。
	ThinkingDisabledFormModeNormalize ThinkingDisabledFormMode = iota
	// ThinkingDisabledFormModeStrict 在发出前按上游公开错误形态拒绝。
	ThinkingDisabledFormModeStrict
)

// ThinkingDisabledFormError 是 thinking 禁用形态的本地拒绝，形态对齐 Anthropic 公开 API。
type ThinkingDisabledFormError struct {
	Type    string
	Message string
}

func (e *ThinkingDisabledFormError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func thinkingDisabledFormMode(parsed *ParsedRequest) ThinkingDisabledFormMode {
	if parsed != nil && parsed.ThinkingDisabledStrict {
		return ThinkingDisabledFormModeStrict
	}
	return ThinkingDisabledFormModeNormalize
}

// disabledThinkingFormJSON 是规范化后唯一的顶层 thinking 值。
var disabledThinkingFormJSON = []byte(`{"type":"disabled"}`)

// 重复 JSON 成员（duplicate JSON member）策略：
//
// 只按 JSON 绑定语义判断 thinking 的可见形态，即同名键取最后一次出现
// （Go encoding/json、上游同样如此），且 thinking 对象内重复 type 也取最后一次。
// 因此 gjson 的“首个匹配键”不再作为判断依据，sjson 只改首个顶层成员的问题也一并消失。
//
//   - 可见形态不是 thinking.type=disabled（含只有一个重复成员是 disabled 的情形）：
//     原样返回，不改写也不拒绝，避免误 400 与把 enabled 语义翻转成 disabled。
//   - 可见形态是 disabled 且带多余键或重复成员：
//     默认删除全部顶层 thinking 成员，并在首个出现的位置注入唯一的 {"type":"disabled"}；
//     严格模式在发出前按上游错误形态本地 400。
//   - 可见形态是 disabled 且该对象只有 type：原样返回。
//
// 改写只动顶层 thinking 成员的字节区间，其余原始字节（含大整数等数字字面量）逐字节保留；
// 拒绝信息只含固定文案，不包含请求体、字段值或内部配置名。

// jsonMemberSpan 记录 JSON 对象中一个成员在原始字节里的位置。
// keyStart 指向键的起始引号；valueStart/valueEnd 是值的区间（valueEnd 为开区间）。
type jsonMemberSpan struct {
	key        string
	keyStart   int
	valueStart int
	valueEnd   int
}

// byteEdit 是一次字节替换（end 为开区间）。
type byteEdit struct {
	start int
	end   int
}

// thinkingDisabledFormReport 是 thinking 在 JSON 绑定语义下的可见形态。
type thinkingDisabledFormReport struct {
	occurrences []jsonMemberSpan // 顶层 thinking 成员，按出现顺序
	object      bool             // 可见形态（最后一个 thinking）是否为对象
	typeValue   string           // 可见 type：对象内最后一次出现的字符串 type
	members     int              // 可见对象的顶层成员数
}

// requiresCleanup 判断可见形态是否为需要清理或拒绝的禁用形态。
func (r thinkingDisabledFormReport) requiresCleanup() bool {
	if !r.object || r.typeValue != "disabled" {
		return false
	}
	// members > 1 覆盖多余键与对象内重复 type；occurrences > 1 覆盖重复顶层 thinking。
	return r.members > 1 || len(r.occurrences) > 1
}

// rewrite 返回清理后的 body：删除全部顶层 thinking 成员并在首个位置注入唯一禁用形态。
// 仅当 requiresCleanup 为真时调用。
func (r thinkingDisabledFormReport) rewrite(body []byte) []byte {
	first := r.occurrences[0]
	out := make([]byte, 0, len(body))
	out = append(out, body[:first.valueStart]...)
	out = append(out, disabledThinkingFormJSON...)
	prev := first.valueEnd
	for _, extra := range r.occurrences[1:] {
		edit := removeJSONMemberEdit(body, extra)
		if edit.end > len(body) {
			continue
		}
		if edit.start < prev {
			// 末尾成员的删除区间与其前一个成员“连同其后逗号”的删除落在同一个分隔逗号上：
			// 该逗号已随前一次删除写出，但它后面已不再有成员，需要连同其后的空白一起回退，
			// 否则删除区间会被整段跳过、末尾 thinking 残留（三重复及以上的默认规范化）。
			out = trimTrailingSeparator(out)
			edit.start = prev
		}
		out = append(out, body[prev:edit.start]...)
		prev = edit.end
	}
	out = append(out, body[prev:]...)
	return out
}

// trimTrailingSeparator 回退已写出内容末尾的分隔逗号（连同其后的空白）。
// 末尾不是逗号时原样返回；只有确认后面不再有成员时才会调用。
func trimTrailingSeparator(out []byte) []byte {
	i := len(out)
	for i > 0 && isJSONSpace(out[i-1]) {
		i--
	}
	if i == 0 || out[i-1] != ',' {
		return out
	}
	return out[:i-1]
}

// inspectThinkingDisabledForm 读取顶层 thinking 在绑定语义下的可见形态。
func inspectThinkingDisabledForm(body []byte) thinkingDisabledFormReport {
	var report thinkingDisabledFormReport
	top, ok := scanJSONTopLevelMembers(body)
	if !ok {
		return report
	}
	for _, member := range top {
		if member.key == "thinking" {
			report.occurrences = append(report.occurrences, member)
		}
	}
	if len(report.occurrences) == 0 {
		return report
	}
	effective := report.occurrences[len(report.occurrences)-1]
	raw := body[effective.valueStart:effective.valueEnd]
	inner, ok := scanJSONTopLevelMembers(raw)
	if !ok {
		return report
	}
	report.object = true
	report.members = len(inner)
	for _, member := range inner {
		if member.key != "type" {
			continue
		}
		var value string
		if err := json.Unmarshal(raw[member.valueStart:member.valueEnd], &value); err == nil {
			report.typeValue = value
		} else {
			report.typeValue = ""
		}
	}
	return report
}

// removeJSONMemberEdit 计算删除一个对象成员所需的区间：优先连同其后的逗号删除，
// 成员位于对象末尾时连同其前的逗号删除，保证结果仍是合法 JSON。
func removeJSONMemberEdit(body []byte, member jsonMemberSpan) byteEdit {
	if i := skipJSONSpaceAt(body, member.valueEnd); i < len(body) && body[i] == ',' {
		return byteEdit{start: member.keyStart, end: i + 1}
	}
	j := member.keyStart - 1
	for j >= 0 && isJSONSpace(body[j]) {
		j--
	}
	if j >= 0 && body[j] == ',' {
		return byteEdit{start: j, end: member.valueEnd}
	}
	return byteEdit{start: member.keyStart, end: member.valueEnd}
}

// scanJSONTopLevelMembers 返回 raw 顶层对象的成员位置；raw 不是对象时返回 false。
// 调用方需保证 raw 是合法 JSON。
func scanJSONTopLevelMembers(raw []byte) ([]jsonMemberSpan, bool) {
	i := skipJSONSpaceAt(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return nil, false
	}
	i++
	var members []jsonMemberSpan
	for {
		i = skipJSONSpaceAt(raw, i)
		if i >= len(raw) {
			return members, false
		}
		if raw[i] == '}' {
			return members, true
		}
		if raw[i] != '"' {
			return members, false
		}
		keyStart := i
		keyEnd := skipJSONStringAt(raw, i)
		var key string
		if err := json.Unmarshal(raw[keyStart:keyEnd], &key); err != nil {
			return members, false
		}
		i = skipJSONSpaceAt(raw, keyEnd)
		if i >= len(raw) || raw[i] != ':' {
			return members, false
		}
		i = skipJSONSpaceAt(raw, i+1)
		if i >= len(raw) {
			return members, false
		}
		valueStart := i
		valueEnd := skipJSONValueAt(raw, i)
		members = append(members, jsonMemberSpan{
			key:        key,
			keyStart:   keyStart,
			valueStart: valueStart,
			valueEnd:   valueEnd,
		})
		i = skipJSONSpaceAt(raw, valueEnd)
		if i < len(raw) && raw[i] == ',' {
			i++
			continue
		}
		if i < len(raw) && raw[i] == '}' {
			return members, true
		}
		return members, false
	}
}

// skipJSONValueAt 返回从 i 开始的值结束位置（开区间），i 指向值的首个非空白字符。
func skipJSONValueAt(raw []byte, i int) int {
	if i >= len(raw) {
		return i
	}
	switch raw[i] {
	case '"':
		return skipJSONStringAt(raw, i)
	case '{', '[':
		depth := 0
		for i < len(raw) {
			switch raw[i] {
			case '"':
				i = skipJSONStringAt(raw, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return len(raw)
	default:
		for i < len(raw) {
			switch raw[i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return i
			}
			i++
		}
		return len(raw)
	}
}

// skipJSONStringAt 返回从起始引号 i 开始的字符串结束位置（开区间）。
func skipJSONStringAt(raw []byte, i int) int {
	i++
	for i < len(raw) {
		switch raw[i] {
		case '\\':
			i += 2
			continue
		case '"':
			return i + 1
		}
		i++
	}
	return len(raw)
}

func skipJSONSpaceAt(raw []byte, i int) int {
	for i < len(raw) && isJSONSpace(raw[i]) {
		i++
	}
	return i
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// ApplyThinkingDisabledForm 只改写顶层 thinking 对象。
// 当可见 thinking.type 为 disabled 且带多余键或重复成员时，
// 规范化模式删除全部顶层 thinking 并注入唯一禁用形态，严格模式按上游错误形态拒绝。
func ApplyThinkingDisabledForm(body []byte, mode ThinkingDisabledFormMode) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("parse thinking disabled form: invalid JSON")
	}

	report := inspectThinkingDisabledForm(body)
	if !report.requiresCleanup() {
		return body, nil
	}
	if mode == ThinkingDisabledFormModeStrict {
		return nil, &ThinkingDisabledFormError{
			Type:    "invalid_request_error",
			Message: "thinking: Extra inputs are not permitted",
		}
	}
	return report.rewrite(body), nil
}

// ApplyThinkingDisabledFormToParsed 按请求上的分组开关改写顶层 thinking 禁用形态。
func ApplyThinkingDisabledFormToParsed(parsed *ParsedRequest) error {
	if parsed == nil {
		return nil
	}
	body := parsed.Body.Bytes()
	rewritten, err := ApplyThinkingDisabledForm(body, thinkingDisabledFormMode(parsed))
	if err != nil {
		return err
	}
	if bytes.Equal(rewritten, body) {
		return nil
	}
	if err := parsed.ReplaceBody(rewritten); err != nil {
		return fmt.Errorf("rewrite thinking disabled form: %w", err)
	}
	return nil
}
