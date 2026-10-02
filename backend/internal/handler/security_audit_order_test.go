package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type promptAuditOrderCase struct {
	file       string
	function   string
	auditToken string
}

func TestPromptAuditGatePrecedesAccountBillingAndUpstreamSideEffects(t *testing.T) {
	tests := []promptAuditOrderCase{
		{file: "gateway_handler.go", function: "Messages", auditToken: "checkSecurityAudit"},
		{file: "gateway_handler_chat_completions.go", function: "ChatCompletions", auditToken: "checkSecurityAudit"},
		{file: "gateway_handler_responses.go", function: "Responses", auditToken: "checkSecurityAudit"},
		{file: "gemini_v1beta_handler.go", function: "GeminiV1BetaModels", auditToken: "checkSecurityAudit"},
		{file: "openai_gateway_handler.go", function: "Responses", auditToken: "checkSecurityAudit"},
		{file: "openai_gateway_handler.go", function: "Messages", auditToken: "checkSecurityAudit"},
		{file: "openai_chat_completions.go", function: "ChatCompletions", auditToken: "checkSecurityAudit"},
		{file: "openai_images.go", function: "Images", auditToken: "checkSecurityAudit"},
		{file: "grok_media.go", function: "handleGrokMedia", auditToken: "checkSecurityAudit"},
		{file: "openai_embeddings.go", function: "Embeddings", auditToken: "checkSecurityAudit"},
		{file: "openai_alpha_search.go", function: "AlphaSearch", auditToken: "checkSecurityAudit"},
		{file: "image_task_handler.go", function: "Submit", auditToken: "checkSecurityAuditBeforeSubmit"},
		{file: "batch_image_handler.go", function: "Submit", auditToken: "checkSecurityAuditBeforeSubmit"},
	}
	sideEffectTokens := []string{
		"CheckBillingEligibility(", "SelectAccount", ".Forward", "acquireResponsesUserSlot(",
		"AcquireUserSlot", "TryAcquireUserSlot", "acquireImageGenerationSlot(",
		"h.tasks.Create(", "h.service.Submit(",
	}
	for _, tt := range tests {
		t.Run(tt.file+"/"+tt.function, func(t *testing.T) {
			functionSource := stripGoComments(goFunctionSource(t, tt.file, tt.function))
			auditIndex := strings.Index(functionSource, tt.auditToken)
			require.NotEqual(t, -1, auditIndex, "missing Prompt Audit gate")
			foundSideEffect := false
			for _, sideEffect := range sideEffectTokens {
				index := strings.Index(functionSource, sideEffect)
				if index < 0 {
					continue
				}
				foundSideEffect = true
				require.Lessf(t, auditIndex, index, "%s must run before %s", tt.auditToken, sideEffect)
			}
			require.True(t, foundSideEffect, "coverage case must contain a downstream side effect")
		})
	}
}

// TestGatewayMockGatePrecedesContentAuditAtEveryEntry 把新优先级钉在控制流上：
// 每个承接下游测试请求的入口都必须先经过严格 Mock 闸门，未命中才进入内容审计。
func TestGatewayMockGatePrecedesContentAuditAtEveryEntry(t *testing.T) {
	entries := []promptAuditOrderCase{
		{file: "gateway_handler.go", function: "Messages", auditToken: "checkSecurityAudit"},
		{file: "gateway_handler_responses.go", function: "Responses", auditToken: "checkSecurityAudit"},
		{file: "gateway_handler_chat_completions.go", function: "ChatCompletions", auditToken: "checkSecurityAudit"},
		{file: "openai_gateway_handler.go", function: "Responses", auditToken: "checkSecurityAudit"},
		{file: "openai_gateway_handler.go", function: "Messages", auditToken: "checkSecurityAudit"},
		{file: "openai_chat_completions.go", function: "ChatCompletions", auditToken: "checkSecurityAudit"},
	}
	for _, tt := range entries {
		t.Run(tt.file+"/"+tt.function, func(t *testing.T) {
			functionSource := stripGoComments(goFunctionSource(t, tt.file, tt.function))
			gateIndex := strings.Index(functionSource, "serveEarlyDownstreamTestMock")
			auditIndex := strings.Index(functionSource, tt.auditToken)
			require.NotEqual(t, -1, gateIndex, "missing early Mock gate")
			require.NotEqual(t, -1, auditIndex, "missing content audit gate")
			require.Lessf(t, gateIndex, auditIndex, "early Mock gate must precede %s", tt.auditToken)
			// 跳过两类内容审计不等于跳过基础限流配额：命中分支必须接上既有
			// CheckBillingEligibility（余额/订阅/平台配额/API Key 窗口/RPM）。
			require.Contains(t, functionSource, "billingCheck:", "early Mock gate must carry the billing/rate-limit admission")
			require.Contains(t, functionSource, "writeBillingReject:", "early Mock gate must map billing rejections")
		})
	}
}

// TestEarlyMockGateKeepsBaseLimitsBeforeServing 证明命中分支内部仍保留基础限制：
// 匹配 -> 权限/封禁预检 -> 冷却 -> 非等待用户并发准入 -> 归还槽位 -> 才写回本地回复。
// 不允许命中后无准入直接本地放行。
func TestEarlyMockGateKeepsBaseLimitsBeforeServing(t *testing.T) {
	source := stripGoComments(goFunctionSource(t, "gateway_mock_gate.go", "serveEarlyDownstreamTestMock"))

	tokens := []string{
		"MatchDownstreamTestRequest", "precheckReject", "retryAfter",
		"TryAcquireUserSlotForAPIKey", "wrapReleaseOnDone", "sendGatewayMockReply",
	}
	index := make(map[string]int, len(tokens))
	for _, token := range tokens {
		idx := strings.Index(source, token)
		require.NotEqual(t, -1, idx, "early Mock gate must contain %s", token)
		index[token] = idx
	}

	require.Less(t, index["MatchDownstreamTestRequest"], index["TryAcquireUserSlotForAPIKey"],
		"只有在匹配之后才做准入")
	require.Less(t, index["precheckReject"], index["TryAcquireUserSlotForAPIKey"],
		"权限/封禁预检必须在取用户槽之前")
	require.Less(t, index["retryAfter"], index["sendGatewayMockReply"],
		"冷却检查必须在写回本地回复之前")
	require.Less(t, index["TryAcquireUserSlotForAPIKey"], index["wrapReleaseOnDone"],
		"取槽之后必须注册归还")
	require.Less(t, index["wrapReleaseOnDone"], index["sendGatewayMockReply"],
		"写回本地回复前槽位归还必须已就位")
}

func stripGoComments(source string) string {
	source = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(source, "")
	return regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(source, "")
}

func goFunctionSource(t *testing.T, filename, functionName string) string {
	t.Helper()
	raw, err := os.ReadFile(filename)
	require.NoError(t, err)
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, filename, raw, 0)
	require.NoError(t, err)
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != functionName || function.Body == nil {
			continue
		}
		start := files.Position(function.Pos()).Offset
		end := files.Position(function.End()).Offset
		require.Greater(t, end, start)
		return string(raw[start:end])
	}
	t.Fatalf("function %s not found in %s", functionName, filename)
	return ""
}
