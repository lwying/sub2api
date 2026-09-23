//go:build unit

package service

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// topLevelJSONKeys 返回 body 顶层对象的键序列（含重复键），用于验证重复成员已被消除。
func topLevelJSONKeys(t *testing.T, body []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	first, err := dec.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), first)

	var keys []string
	depth := 1
	expectKey := true
	for {
		token, err := dec.Token()
		require.NoError(t, err)
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			default:
				depth--
				if depth == 0 {
					return keys
				}
			}
		} else if depth == 1 && expectKey {
			if key, ok := token.(string); ok {
				keys = append(keys, key)
			}
		}
		if depth == 1 {
			expectKey = !expectKey
		}
	}
}

func countTopLevelKey(t *testing.T, body []byte, key string) int {
	t.Helper()
	count := 0
	for _, k := range topLevelJSONKeys(t, body) {
		if k == key {
			count++
		}
	}
	return count
}

func TestNormalizeThinkingDisabledForm_stripsExtraKeysWhenTypeDisabled(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"thinking":{"type":"disabled","display":"hidden","budget_tokens":1024,"unknown":true},
		"messages":[{"role":"user","content":"hi"}]
	}`)

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeNormalize)
	require.NoError(t, err)

	thinking := gjson.GetBytes(out, "thinking")
	require.True(t, thinking.Exists())
	require.Equal(t, "disabled", thinking.Get("type").String())

	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(thinking.Raw), &obj))
	require.Equal(t, map[string]any{"type": "disabled"}, obj)

	require.Equal(t, "hi", gjson.GetBytes(out, "messages.0.content").String())
	require.Equal(t, "claude-sonnet-4-5", gjson.GetBytes(out, "model").String())
}

func TestNormalizeThinkingDisabledForm_preservesUnrelatedLargeIntegers(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","sequence":900719925474099312345,"thinking":{"type":"disabled","display":"hidden"},"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"lookup","input":{"id":900719925474099312345}}]}]}`)
	want := []byte(`{"model":"claude-sonnet-4-5","sequence":900719925474099312345,"thinking":{"type":"disabled"},"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"lookup","input":{"id":900719925474099312345}}]}]}`)

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeNormalize)
	require.NoError(t, err)
	require.Equal(t, want, out)
	require.Equal(t, "900719925474099312345", gjson.GetBytes(out, "sequence").Raw)
	require.Equal(t, "900719925474099312345", gjson.GetBytes(out, "messages.0.content.0.input.id").Raw)
}

func TestNormalizeThinkingDisabledForm_leavesEnabledThinkingUnchanged(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[{"role":"user","content":"hi"}]
	}`)

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeNormalize)
	require.NoError(t, err)
	require.JSONEq(t, string(body), string(out))
}

func TestApplyThinkingDisabledForm_strictRejectsExtraKeys(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"thinking":{"type":"disabled","display":"hidden"},
		"messages":[{"role":"user","content":"hi"}]
	}`)

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeStrict)
	require.Error(t, err)
	require.Nil(t, out)

	var rejected *ThinkingDisabledFormError
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, "invalid_request_error", rejected.Type)
	require.Contains(t, rejected.Message, "thinking")
	require.NotContains(t, rejected.Message, "ThinkingDisabled")
	require.NotContains(t, rejected.Message, "strict")
	require.NotContains(t, rejected.Error(), "gateway")
}

func TestApplyThinkingDisabledForm_strictAllowsTypeOnly(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"thinking":{"type":"disabled"},
		"messages":[{"role":"user","content":"hi"}]
	}`)

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeStrict)
	require.NoError(t, err)
	require.JSONEq(t, string(body), string(out))
}

func TestThinkingDisabledFormMode_defaultsToNormalize(t *testing.T) {
	require.Equal(t, ThinkingDisabledFormModeNormalize, thinkingDisabledFormMode(nil))
	require.Equal(t, ThinkingDisabledFormModeNormalize, thinkingDisabledFormMode(&ParsedRequest{}))
	require.Equal(t, ThinkingDisabledFormModeStrict, thinkingDisabledFormMode(&ParsedRequest{ThinkingDisabledStrict: true}))
}

func TestApplyThinkingDisabledForm_doesNotTouchNestedThinkingFields(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"thinking":{"type":"disabled","display":"hidden"},
		"messages":[{"role":"user","content":[{"type":"text","text":"hi","thinking":{"display":"keep-me"}}]}]
	}`)

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeNormalize)
	require.NoError(t, err)
	require.Equal(t, "disabled", gjson.GetBytes(out, "thinking.type").String())
	require.False(t, gjson.GetBytes(out, "thinking.display").Exists())
	require.Equal(t, "keep-me", gjson.GetBytes(out, "messages.0.content.0.thinking.display").String())
}

func TestApplyThinkingDisabledFormToParsedNormalizeAndStrict(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4","thinking":{"type":"disabled","budget_tokens":1024}}`)
	parsed := &ParsedRequest{Body: NewRequestBodyRef(body)}
	require.NoError(t, ApplyThinkingDisabledFormToParsed(parsed))
	require.Equal(t, "disabled", gjson.GetBytes(parsed.Body.Bytes(), "thinking.type").String())
	require.False(t, gjson.GetBytes(parsed.Body.Bytes(), "thinking.budget_tokens").Exists())

	strict := &ParsedRequest{Body: NewRequestBodyRef(body), ThinkingDisabledStrict: true}
	err := ApplyThinkingDisabledFormToParsed(strict)
	var formErr *ThinkingDisabledFormError
	require.ErrorAs(t, err, &formErr)
	require.Equal(t, "invalid_request_error", formErr.Type)
	require.Equal(t, "thinking: Extra inputs are not permitted", formErr.Message)
}

// --- 重复 JSON 成员（duplicate JSON member）策略 ---
// 可见形态按 JSON 绑定语义（同名键取最后出现）判定：可见 type 不是 disabled 时不改，
// 可见 type 是 disabled 且带多余键或重复成员时默认清理为唯一禁用形态、严格模式 400。

func TestNormalizeThinkingDisabledForm_duplicateTypeEnabledThenDisabledWithDisplay(t *testing.T) {
	// 单个 thinking 对象内重复 type：gjson/sjson 看首个（enabled），JSON 绑定看最后（disabled）。
	body := []byte(`{"model":"claude-sonnet-4-5","thinking":{"type":"enabled","type":"disabled","display":"hidden"},"messages":[{"role":"user","content":"hi"}]}`)
	want := []byte(`{"model":"claude-sonnet-4-5","thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`)

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeNormalize)
	require.NoError(t, err)
	require.Equal(t, want, out)
	require.Equal(t, 1, countTopLevelKey(t, out, "thinking"))
	require.False(t, gjson.GetBytes(out, "thinking.display").Exists())
	require.Equal(t, "hi", gjson.GetBytes(out, "messages.0.content").String())
}

func TestStrictThinkingDisabledForm_duplicateTypeEnabledThenDisabledWithDisplay(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","thinking":{"type":"enabled","type":"disabled","display":"hidden"},"messages":[{"role":"user","content":"hi"}]}`)

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeStrict)
	require.Error(t, err)
	require.Nil(t, out)

	var rejected *ThinkingDisabledFormError
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, "invalid_request_error", rejected.Type)
	require.Equal(t, "thinking: Extra inputs are not permitted", rejected.Message)
}

func TestNormalizeThinkingDisabledForm_duplicateTopLevelThinkingEnabledThenDisabledWithDisplay(t *testing.T) {
	// sjson 只改首个顶层 thinking，绑定取最后出现，因此重复成员必须整体清理成一个。
	body := []byte(`{"model":"claude-sonnet-4-5","thinking":{"type":"enabled","budget_tokens":5},"thinking":{"type":"disabled","display":"hidden"},"messages":[{"role":"user","content":"hi"}]}`)
	want := []byte(`{"model":"claude-sonnet-4-5","thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`)

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeNormalize)
	require.NoError(t, err)
	require.Equal(t, want, out)
	require.Equal(t, 1, countTopLevelKey(t, out, "thinking"))
	require.Equal(t, "disabled", gjson.GetBytes(out, "thinking.type").String())
	require.False(t, gjson.GetBytes(out, "thinking.display").Exists())
	require.False(t, gjson.GetBytes(out, "thinking.budget_tokens").Exists())
}

func TestStrictThinkingDisabledForm_duplicateTopLevelThinkingEnabledThenDisabledWithDisplay(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","thinking":{"type":"enabled","budget_tokens":5},"thinking":{"type":"disabled","display":"hidden"},"messages":[{"role":"user","content":"hi"}]}`)

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeStrict)
	require.Error(t, err)
	require.Nil(t, out)

	var rejected *ThinkingDisabledFormError
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, "invalid_request_error", rejected.Type)
	require.Equal(t, "thinking: Extra inputs are not permitted", rejected.Message)
}

func TestThinkingDisabledForm_duplicateEffectiveEnabledIsUnchanged(t *testing.T) {
	// 可见（绑定语义最后出现）type 为 enabled 时既不 400 也不改写，避免误拒与语义翻转。
	cases := map[string]string{
		"disabled then enabled":           `{"model":"claude-sonnet-4-5","thinking":{"type":"disabled"},"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`,
		"disabled extras then enabled":    `{"model":"claude-sonnet-4-5","thinking":{"type":"disabled","display":"hidden"},"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`,
		"duplicate type disabled first":   `{"model":"claude-sonnet-4-5","thinking":{"type":"disabled","display":"hidden","type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`,
		"three occurrences enabled first": `{"model":"claude-sonnet-4-5","thinking":{"type":"disabled","display":"hidden"},"thinking":{"type":"enabled"},"thinking":{"type":"enabled","budget_tokens":5},"messages":[{"role":"user","content":"hi"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			for _, mode := range []ThinkingDisabledFormMode{ThinkingDisabledFormModeNormalize, ThinkingDisabledFormModeStrict} {
				out, err := ApplyThinkingDisabledForm([]byte(body), mode)
				require.NoError(t, err)
				require.Equal(t, body, string(out))
			}
		})
	}
}

func TestThinkingDisabledForm_duplicateWithoutDisabledIsUnchanged(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","thinking":{"type":"enabled"},"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`)

	for _, mode := range []ThinkingDisabledFormMode{ThinkingDisabledFormModeNormalize, ThinkingDisabledFormModeStrict} {
		out, err := ApplyThinkingDisabledForm(body, mode)
		require.NoError(t, err)
		require.Equal(t, string(body), string(out))
	}
}

func TestNormalizeThinkingDisabledForm_dedupesDuplicateTypeOnlyDisabled(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","thinking":{"type":"disabled"},"thinking":{"type":"disabled"}}`)
	want := []byte(`{"model":"claude-sonnet-4-5","thinking":{"type":"disabled"}}`)

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeNormalize)
	require.NoError(t, err)
	require.Equal(t, want, out)
	require.Equal(t, 1, countTopLevelKey(t, out, "thinking"))

	_, strictErr := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeStrict)
	var rejected *ThinkingDisabledFormError
	require.ErrorAs(t, strictErr, &rejected)
	require.Equal(t, "invalid_request_error", rejected.Type)
}

func TestNormalizeThinkingDisabledForm_duplicateLastMemberAndTrailingWhitespace(t *testing.T) {
	// 重复成员位于末尾时需连同前面的逗号删除，且保留其余空白与同行字段。
	body := []byte("{\n  \"model\":\"claude-sonnet-4-5\",\n  \"thinking\":{\"type\":\"enabled\"},\n  \"thinking\":{\"type\":\"disabled\",\"display\":\"hidden\"}\n}")
	want := []byte("{\n  \"model\":\"claude-sonnet-4-5\",\n  \"thinking\":{\"type\":\"disabled\"}\n}")

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeNormalize)
	require.NoError(t, err)
	require.Equal(t, want, out)
	require.Equal(t, 1, countTopLevelKey(t, out, "thinking"))
	require.Equal(t, "claude-sonnet-4-5", gjson.GetBytes(out, "model").String())
}

func TestNormalizeThinkingDisabledForm_duplicatePreservesUnrelatedLargeIntegers(t *testing.T) {
	body := []byte(`{"sequence":900719925474099312345,"thinking":{"type":"enabled"},"thinking":{"type":"disabled","display":"hidden"},"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"lookup","input":{"id":900719925474099312345}}]}]}`)
	want := []byte(`{"sequence":900719925474099312345,"thinking":{"type":"disabled"},"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"lookup","input":{"id":900719925474099312345}}]}]}`)

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeNormalize)
	require.NoError(t, err)
	require.Equal(t, want, out)
	require.Equal(t, "900719925474099312345", gjson.GetBytes(out, "sequence").Raw)
	require.Equal(t, "900719925474099312345", gjson.GetBytes(out, "messages.0.content.0.input.id").Raw)
	require.Equal(t, 1, countTopLevelKey(t, out, "thinking"))
}

func TestApplyThinkingDisabledForm_doesNotPersistOrLeakRawBody(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","thinking":{"type":"enabled"},"thinking":{"type":"disabled","display":"super-secret-value"},"messages":[{"role":"user","content":"hi"}]}`)
	original := append([]byte(nil), body...)

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeNormalize)
	require.NoError(t, err)
	require.Equal(t, string(original), string(body), "输入 body 不得被就地改写")
	require.NotContains(t, string(out), "super-secret-value")

	_, strictErr := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeStrict)
	require.Error(t, strictErr)
	require.NotContains(t, strictErr.Error(), "super-secret-value")
	require.NotContains(t, strictErr.Error(), "ThinkingDisabled")
	require.NotContains(t, strictErr.Error(), "gateway")
}

func TestNormalizeThinkingDisabledForm_dedupesDuplicateDisabledWithExtras(t *testing.T) {
	// 两次出现都是 disabled 且各带多余键：清理为唯一 {"type":"disabled"}，两次多余键都不残留。
	body := []byte(`{"model":"claude-sonnet-4-5","thinking":{"type":"disabled","display":"hidden"},"thinking":{"type":"disabled","budget_tokens":5}}`)
	want := []byte(`{"model":"claude-sonnet-4-5","thinking":{"type":"disabled"}}`)

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeNormalize)
	require.NoError(t, err)
	require.Equal(t, want, out)
	require.Equal(t, 1, countTopLevelKey(t, out, "thinking"))

	_, strictErr := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeStrict)
	var rejected *ThinkingDisabledFormError
	require.ErrorAs(t, strictErr, &rejected)
	require.Equal(t, "invalid_request_error", rejected.Type)
}

// threeDuplicateThinkingBodies 是三个顶层 thinking 均为禁用形态的样本：
// 末尾成员带空白、重复成员之间夹着其他成员、以及带大整数与多余键三种三重复场景。
var threeDuplicateThinkingBodies = map[string]string{
	"adjacent duplicates before trailing member": `{"sequence":900719925474099312345,"thinking":{"type":"disabled","display":"hidden"},"thinking":{"type":"disabled","budget_tokens":1024},"thinking":{"type":"disabled","display":"x"},"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"lookup","input":{"id":900719925474099312345}}]}]}`,
	"terminal duplicate with whitespace":         "{\n  \"model\":\"claude-sonnet-4-5\",\n  \"thinking\":{\"type\":\"disabled\",\"display\":\"hidden\"},\n  \"thinking\":{\"type\":\"disabled\",\"budget_tokens\":1024},\n  \"thinking\":{\"type\":\"disabled\",\"display\":\"x\"}\n}",
	"terminal duplicate without whitespace":      `{"model":"claude-sonnet-4-5","thinking":{"type":"disabled","display":"hidden"},"thinking":{"type":"disabled","budget_tokens":1024},"thinking":{"type":"disabled","display":"x"}}`,
	"four duplicates before trailing member":     `{"model":"claude-sonnet-4-5","thinking":{"type":"disabled","display":"hidden"},"thinking":{"type":"disabled","budget_tokens":1},"thinking":{"type":"disabled","budget_tokens":2},"thinking":{"type":"disabled","display":"x"},"messages":[{"role":"user","content":"hi"}]}`,
	"duplicates around a middle member":          `{"model":"claude-sonnet-4-5","thinking":{"type":"disabled","display":"hidden"},"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":1024},"thinking":{"type":"disabled","display":"x"}}`,
}

func TestNormalizeThinkingDisabledForm_threeDuplicateTopLevelThinking(t *testing.T) {
	// 三次以上重复时，末尾成员的删除区间与前一次“连同其后逗号”的删除落在同一个分隔逗号上；
	// 若把重叠区间整段跳过，末尾 thinking（仍带 display 等多余键）会残留，清理结果必须唯一。
	want := map[string]string{
		"adjacent duplicates before trailing member": `{"sequence":900719925474099312345,"thinking":{"type":"disabled"},"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"lookup","input":{"id":900719925474099312345}}]}]}`,
		"terminal duplicate with whitespace":         "{\n  \"model\":\"claude-sonnet-4-5\",\n  \"thinking\":{\"type\":\"disabled\"}\n}",
		"terminal duplicate without whitespace":      `{"model":"claude-sonnet-4-5","thinking":{"type":"disabled"}}`,
		"four duplicates before trailing member":     `{"model":"claude-sonnet-4-5","thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`,
		"duplicates around a middle member":          `{"model":"claude-sonnet-4-5","thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`,
	}

	for name, body := range threeDuplicateThinkingBodies {
		t.Run(name, func(t *testing.T) {
			out, err := ApplyThinkingDisabledForm([]byte(body), ThinkingDisabledFormModeNormalize)
			require.NoError(t, err)
			require.Equal(t, want[name], string(out))
			require.Equal(t, 1, countTopLevelKey(t, out, "thinking"))
			require.Equal(t, "disabled", gjson.GetBytes(out, "thinking.type").String())
			require.False(t, gjson.GetBytes(out, "thinking.display").Exists())
			require.False(t, gjson.GetBytes(out, "thinking.budget_tokens").Exists())
		})
	}
}

func TestNormalizeThinkingDisabledForm_threeDuplicateTopLevelThinkingPreservesUnrelatedIntegers(t *testing.T) {
	// 清理只动顶层 thinking 成员：大整数与其余成员逐字节保留。
	body := []byte(threeDuplicateThinkingBodies["adjacent duplicates before trailing member"])

	out, err := ApplyThinkingDisabledForm(body, ThinkingDisabledFormModeNormalize)
	require.NoError(t, err)
	require.Equal(t, "900719925474099312345", gjson.GetBytes(out, "sequence").Raw)
	require.Equal(t, "900719925474099312345", gjson.GetBytes(out, "messages.0.content.0.input.id").Raw)
	require.Equal(t, "lookup", gjson.GetBytes(out, "messages.0.content.0.name").String())
}

func TestStrictThinkingDisabledForm_threeDuplicateTopLevelThinking(t *testing.T) {
	// 严格模式对三重复形态的拒绝行为与两重复一致，且不改写 body。
	for name, body := range threeDuplicateThinkingBodies {
		t.Run(name, func(t *testing.T) {
			original := []byte(body)

			out, err := ApplyThinkingDisabledForm(original, ThinkingDisabledFormModeStrict)
			require.Error(t, err)
			require.Nil(t, out)

			var rejected *ThinkingDisabledFormError
			require.ErrorAs(t, err, &rejected)
			require.Equal(t, "invalid_request_error", rejected.Type)
			require.Equal(t, "thinking: Extra inputs are not permitted", rejected.Message)
			require.Equal(t, body, string(original), "严格拒绝时不得改写 body")
		})
	}
}

func TestApplyThinkingDisabledFormToParsed_duplicateThinking(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4","thinking":{"type":"enabled"},"thinking":{"type":"disabled","display":"hidden","budget_tokens":1024}}`)

	parsed := &ParsedRequest{Body: NewRequestBodyRef(body)}
	require.NoError(t, ApplyThinkingDisabledFormToParsed(parsed))
	out := parsed.Body.Bytes()
	require.Equal(t, 1, countTopLevelKey(t, out, "thinking"))
	require.Equal(t, "disabled", gjson.GetBytes(out, "thinking.type").String())
	require.False(t, gjson.GetBytes(out, "thinking.display").Exists())
	require.False(t, gjson.GetBytes(out, "thinking.budget_tokens").Exists())

	strict := &ParsedRequest{Body: NewRequestBodyRef(body), ThinkingDisabledStrict: true}
	err := ApplyThinkingDisabledFormToParsed(strict)
	var formErr *ThinkingDisabledFormError
	require.ErrorAs(t, err, &formErr)
	require.Equal(t, "invalid_request_error", formErr.Type)
	require.Equal(t, "thinking: Extra inputs are not permitted", formErr.Message)
	require.Equal(t, string(body), string(strict.Body.Bytes()), "严格拒绝时不得改写 body")
}
