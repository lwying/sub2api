package handler

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	clientResponseAuditSnapshotToken      = "requestAuditMetadata = snapshotClientResponseAudit(requestAuditMetadata, c)"
	clientResponseAuditProtocolFieldToken = "requestAuditMetadata.ProtocolFields = service.SanitizeRequestAuditProtocolFields("
	clientResponseAuditGuardToken         = `if notCapturedReason == "" {`
)

// clientResponseAuditWiringCase 钉住「返回客户端的响应」阶段在六个 usage 提交点的接线契约。
// submitCount / snapshotCount 是回归守卫：未采集路径（NotCapturedReason=phase1_uncovered）
// 也有提交点，但绝不能出现快照，否则空链路会伪装出一个从未按采集口径记录的响应阶段。
type clientResponseAuditWiringCase struct {
	file          string
	function      string
	submitCount   int
	snapshotCount int
}

// TestClientResponseAuditSnapshotPrecedesAsyncUsageSubmit 验证四阶段中「返回客户端的响应」
// 阶段事实的采集位置。契约来自 CONTEXT.md（协议元数据/采集完整性）与 ADR 0002（只挂已有
// 使用记录）：
//
//  1. 每个已采集提交点都必须在构造 requestAuditMetadata 之后、提交异步 usage 任务之前取快照；
//     异步 worker 内访问 gin.Context 会与 gin 的 context 复用竞争，所以快照不得在闭包内。
//  2. 快照前必须有 `notCapturedReason == ""` 守卫：未采集链路不得记录响应阶段。
//  3. 未采集提交点（gateway_handler.go 中同函数的另一个提交点）不得出现快照。
func TestClientResponseAuditSnapshotPrecedesAsyncUsageSubmit(t *testing.T) {
	tests := []clientResponseAuditWiringCase{
		{file: "gateway_handler.go", function: "Messages", submitCount: 2, snapshotCount: 1},
		{file: "gateway_handler_chat_completions.go", function: "ChatCompletions", submitCount: 1, snapshotCount: 1},
		{file: "gateway_handler_responses.go", function: "Responses", submitCount: 1, snapshotCount: 1},
		{file: "openai_chat_completions.go", function: "ChatCompletions", submitCount: 1, snapshotCount: 1},
		{file: "openai_gateway_handler.go", function: "Responses", submitCount: 1, snapshotCount: 1},
		{file: "openai_gateway_handler.go", function: "Messages", submitCount: 1, snapshotCount: 1},
	}
	submitTokens := []string{"submitUsageRecordTask(", "submitOpenAIUsageRecordTask("}

	for _, tt := range tests {
		t.Run(tt.file+"/"+tt.function, func(t *testing.T) {
			source := stripGoComments(goFunctionSource(t, tt.file, tt.function))

			protocolFields := allIndices(source, clientResponseAuditProtocolFieldToken)
			snapshots := allIndices(source, clientResponseAuditSnapshotToken)
			submits := 0
			for _, token := range submitTokens {
				submits += len(allIndices(source, token))
			}

			require.Equal(t, tt.submitCount, submits, "usage 提交点数量变化时，必须重新确认哪些提交点该采集响应阶段")
			require.Equal(t, tt.snapshotCount, len(snapshots), "已采集提交点必须恰好各取一次客户端响应快照")
			require.Len(t, protocolFields, tt.snapshotCount,
				"每个构造 requestAuditMetadata 的提交点都必须配一个客户端响应快照")

			for i, protocolFieldIdx := range protocolFields {
				snapshotIdx := snapshots[i]
				require.Less(t, protocolFieldIdx, snapshotIdx,
					"快照必须在该提交点的 metadata 构造之后")

				// 守卫必须落在构造与快照之间：未采集链路（not_captured）不得记录响应阶段。
				guardIdx := indexAfter(source, protocolFieldIdx, clientResponseAuditGuardToken)
				require.NotEqual(t, -1, guardIdx, "missing notCapturedReason guard")
				require.Less(t, guardIdx, snapshotIdx, "快照必须受 notCapturedReason 守卫")

				// 构造与快照之间不得打开新的函数字面量：一旦快照落进 worker 闭包，
				// 就等于在异步线程里读 gin.Context。
				between := source[protocolFieldIdx:snapshotIdx]
				require.NotContains(t, between, "func(", "快照必须在提交异步 usage 任务之前，不得落进 worker 闭包")

				nextSubmitIdx := firstIndexOf(source, snapshotIdx, submitTokens)
				require.NotEqual(t, -1, nextSubmitIdx,
					"快照之后必须存在本次提交点的异步提交调用")
			}
		})
	}
}

func allIndices(source, token string) []int {
	var indices []int
	for offset := 0; ; {
		idx := strings.Index(source[offset:], token)
		if idx < 0 {
			return indices
		}
		indices = append(indices, offset+idx)
		offset += idx + len(token)
	}
}

func indexAfter(source string, from int, token string) int {
	if from < 0 || from >= len(source) {
		return -1
	}
	idx := strings.Index(source[from:], token)
	if idx < 0 {
		return -1
	}
	return from + idx
}

func firstIndexOf(source string, from int, tokens []string) int {
	found := -1
	for _, token := range tokens {
		idx := indexAfter(source, from, token)
		if idx >= 0 && (found == -1 || idx < found) {
			found = idx
		}
	}
	return found
}
