//go:build integration

package repository

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// F1 回归：诊断行的真实上游尝试序号（1-based wire ordinal）落在 request_audits.attempts
// 里 stage='wire' 的**第 N 个**元素上，而不是数组第 N 个元素。
//
// handler 侧 appendOpenAITransportAttempts 在数组**前面**追加本地阶段
// （client_entry、post_normalize，见 handler/request_audit_attempts.go 与
// TestAppendOpenAITransportAttemptsBuildsOneTimeline），因此真实 wire 尝试从数组
// 偏移处开始。旧的关联谓词用 `attempts -> (attempt_index - 1)`，在真实形状下会指向
// client_entry/post_normalize，永远绑不上；一旦本地阶段数量变化或状态偶然相等，还会
// 拿错序号去猜一条关联。
//
// 关联必须按「真实 wire 序号 + 状态」逐项吻合，且：
//   - 允许本地阶段缺省（只按 stage='wire' 计数，不硬编码 2 的偏移）；
//   - 序号越界、状态不符、无对应 usage/audit 一律不关联，绝不猜；
//   - capture_completeness='not_captured' 的审计不是关联依据（正向与补偿路径同一条件）；
//   - attempts 形态异常（非数组、状态非数字）不得让整条语句报错（fail-open 旁路里
//     一条坏行不能拖垮整批补关联）。

// realHandlerAuditAttempts 复刻 handler 实际落库的 attempts JSON 形状：本地阶段在前，
// 真实 wire 尝试按序号在后。
func realHandlerAuditAttempts() string {
	return `[
		{"stage":"client_entry","protocol":"anthropic.messages"},
		{"stage":"post_normalize","protocol":"anthropic.messages"},
		{"stage":"wire","upstream_status":503},
		{"stage":"wire","upstream_status":429}
	]`
}

// insertAuditWithCompleteness 写入一条 request_audits 行（每 usage 唯一），并显式指定
// capture_completeness。
func insertAuditWithCompleteness(t *testing.T, usageID int64, digest, attemptsJSON, completeness string) {
	t.Helper()
	_, err := integrationDB.ExecContext(context.Background(), `
		INSERT INTO request_audits (usage_log_id, metadata, attempts, capture_completeness)
		VALUES ($1, jsonb_build_object('ids', jsonb_build_object('local_request_fingerprint', $2::text)), $3::jsonb, $4)`,
		usageID, digest, attemptsJSON, completeness)
	require.NoError(t, err)
}

// insertAuditAttemptsJSON 写入一条已采集（complete）的 request_audits 行。
func insertAuditAttemptsJSON(t *testing.T, usageID int64, digest, attemptsJSON string) {
	t.Helper()
	insertAuditWithCompleteness(t, usageID, digest, attemptsJSON, "complete")
}

func plainDiagnosticOwner(t *testing.T, id string) sql.NullInt64 {
	t.Helper()
	var owner sql.NullInt64
	require.NoError(t, integrationDB.QueryRowContext(context.Background(),
		`SELECT plain_owner_usage_log_id FROM error_diagnostic_records WHERE diagnostic_id = $1`, id).Scan(&owner))
	return owner
}

func createPlainDiagnostic(t *testing.T, digest string, attemptIndex, status int) service.ErrorDiagnosticWrite {
	t.Helper()
	write := plainDiagnosticWriteFixture(t, digest, attemptIndex, status)
	_, err := NewErrorDiagnosticRepository(integrationDB, nil).CreateErrorDiagnostic(context.Background(), write, time.Now().UTC())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM error_diagnostic_records WHERE diagnostic_id = $1`, write.ID)
	})
	return write
}

// TestPlainErrorDiagnosticLinksByRealWireOrdinal 覆盖正向关联：在真实 handler 形状下，
// 第 1 次与第 3 次真实 wire 尝试必须各自绑定到对应元素；状态不符或序号越界绝不绑定。
func TestPlainErrorDiagnosticLinksByRealWireOrdinal(t *testing.T) {
	ctx := context.Background()
	repo := NewErrorDiagnosticRepository(integrationDB, nil)
	attacher, ok := repo.(service.ErrorDiagnosticUsageAttacher)
	require.True(t, ok)

	// —— 第 1 次真实尝试：数组里它在 client_entry/post_normalize 之后 ——
	_, _, _, firstUsageID := createErrorDiagnosticUsageFixture(t, ctx)
	cleanupUsageLogFixture(t, firstUsageID)
	firstDigest := strings.Repeat("1", 64)
	insertAuditAttemptsJSON(t, firstUsageID, firstDigest, realHandlerAuditAttempts())

	first := createPlainDiagnostic(t, firstDigest, 1, 503)         // wire#1 = 503 → 应绑定
	firstMismatch := createPlainDiagnostic(t, firstDigest, 1, 429) // 序号对但状态是 wire#2 的 → 不得越位绑定
	outOfRange := createPlainDiagnostic(t, firstDigest, 3, 429)    // 只有 2 次 wire → 越界，不得绑定

	linked, err := attacher.LinkPlainErrorDiagnostics(ctx, firstUsageID, firstDigest, time.Now().UTC())
	require.NoError(t, err)
	require.EqualValues(t, 1, linked, "只有序号与状态都吻合的第 1 次尝试可以绑定")

	require.True(t, plainDiagnosticOwner(t, first.ID).Valid, "第 1 次真实 wire 尝试必须绑定到 wire#1，而不是数组偏移 0 的 client_entry")
	require.Equal(t, firstUsageID, plainDiagnosticOwner(t, first.ID).Int64)
	require.False(t, plainDiagnosticOwner(t, firstMismatch.ID).Valid, "状态不符时绝不绑定到后一次尝试")
	require.False(t, plainDiagnosticOwner(t, outOfRange.ID).Valid, "序号越界时绝不绑定")

	// —— 第 3 次真实尝试：必须绑定到第 3 个 stage='wire' 元素（429），不是数组第 3 个元素 ——
	_, _, _, thirdUsageID := createErrorDiagnosticUsageFixture(t, ctx)
	cleanupUsageLogFixture(t, thirdUsageID)
	thirdDigest := strings.Repeat("3", 64)
	insertAuditAttemptsJSON(t, thirdUsageID, thirdDigest, `[
		{"stage":"client_entry","protocol":"anthropic.messages"},
		{"stage":"post_normalize","protocol":"anthropic.messages"},
		{"stage":"wire","upstream_status":500},
		{"stage":"wire","upstream_status":501},
		{"stage":"wire","upstream_status":429}
	]`)

	third := createPlainDiagnostic(t, thirdDigest, 3, 429)
	linked, err = attacher.LinkPlainErrorDiagnostics(ctx, thirdUsageID, thirdDigest, time.Now().UTC())
	require.NoError(t, err)
	require.EqualValues(t, 1, linked, "第 3 次真实尝试必须绑定")
	require.True(t, plainDiagnosticOwner(t, third.ID).Valid)
	require.Equal(t, thirdUsageID, plainDiagnosticOwner(t, third.ID).Int64)
}

// TestPlainErrorDiagnosticLinkCountsOnlyWireStages 覆盖本地阶段缺省：只按 stage='wire'
// 计数，不假设固定 2 个前置阶段。
func TestPlainErrorDiagnosticLinkCountsOnlyWireStages(t *testing.T) {
	ctx := context.Background()
	repo := NewErrorDiagnosticRepository(integrationDB, nil)
	attacher := repo.(service.ErrorDiagnosticUsageAttacher)

	// 只有 post_normalize 一个前置阶段（client_entry 缺失）。
	_, _, _, usageID := createErrorDiagnosticUsageFixture(t, ctx)
	cleanupUsageLogFixture(t, usageID)
	digest := strings.Repeat("5", 64)
	insertAuditAttemptsJSON(t, usageID, digest, `[
		{"stage":"post_normalize","protocol":"anthropic.messages"},
		{"stage":"wire","upstream_status":502},
		{"stage":"wire","upstream_status":429}
	]`)
	diag := createPlainDiagnostic(t, digest, 2, 429)

	linked, err := attacher.LinkPlainErrorDiagnostics(ctx, usageID, digest, time.Now().UTC())
	require.NoError(t, err)
	require.EqualValues(t, 1, linked, "前置本地阶段数量与 handler 当前形状不同也必须按 wire 序号映射")
	require.True(t, plainDiagnosticOwner(t, diag.ID).Valid)

	// 完全没有本地阶段（强制审计/预留路径）：第 1 次 wire 就是数组第 1 个元素。
	_, _, _, noPhaseUsageID := createErrorDiagnosticUsageFixture(t, ctx)
	cleanupUsageLogFixture(t, noPhaseUsageID)
	noPhaseDigest := strings.Repeat("6", 64)
	insertAuditAttemptsJSON(t, noPhaseUsageID, noPhaseDigest, `[
		{"stage":"wire","upstream_status":500},
		{"stage":"wire","upstream_status":429}
	]`)
	noPhase := createPlainDiagnostic(t, noPhaseDigest, 1, 500)
	linked, err = attacher.LinkPlainErrorDiagnostics(ctx, noPhaseUsageID, noPhaseDigest, time.Now().UTC())
	require.NoError(t, err)
	require.EqualValues(t, 1, linked)
	require.True(t, plainDiagnosticOwner(t, noPhase.ID).Valid)
}

// TestPlainErrorDiagnosticLinkRequiresMatchingUsage 覆盖无对应 usage/audit：诊断行不得被
// 平白关联，也不得因为别的 usage 存在而复活。
func TestPlainErrorDiagnosticLinkRequiresMatchingUsage(t *testing.T) {
	ctx := context.Background()
	repo := NewErrorDiagnosticRepository(integrationDB, nil)
	attacher := repo.(service.ErrorDiagnosticUsageAttacher)

	// usage 存在但审计摘要不匹配：链接摘要不同 → 不关联。
	_, _, _, usageID := createErrorDiagnosticUsageFixture(t, ctx)
	cleanupUsageLogFixture(t, usageID)
	auditDigest := strings.Repeat("7", 64)
	insertAuditAttemptsJSON(t, usageID, auditDigest, realHandlerAuditAttempts())

	diagnosticDigest := strings.Repeat("8", 64)
	diag := createPlainDiagnostic(t, diagnosticDigest, 1, 503)

	linked, err := attacher.LinkPlainErrorDiagnostics(ctx, usageID, diagnosticDigest, time.Now().UTC())
	require.NoError(t, err)
	require.Zero(t, linked, "没有匹配审计的诊断不得被关联")
	require.False(t, plainDiagnosticOwner(t, diag.ID).Valid)

	// usage 存在但其下没有任何审计行 → 同样不关联。
	_, _, _, emptyUsageID := createErrorDiagnosticUsageFixture(t, ctx)
	cleanupUsageLogFixture(t, emptyUsageID)
	linked, err = attacher.LinkPlainErrorDiagnostics(ctx, emptyUsageID, diagnosticDigest, time.Now().UTC())
	require.NoError(t, err)
	require.Zero(t, linked)
	require.False(t, plainDiagnosticOwner(t, diag.ID).Valid)
}

// TestPlainErrorDiagnosticReconcileUsesRealWireOrdinal 覆盖补偿路径：异步反序场景下，
// 补关联同样必须按真实 wire 序号映射，状态不符绝不绑定。
func TestPlainErrorDiagnosticReconcileUsesRealWireOrdinal(t *testing.T) {
	ctx := context.Background()
	repo := NewErrorDiagnosticRepository(integrationDB, nil)
	reconciler, ok := repo.(service.ErrorDiagnosticLinkReconciler)
	require.True(t, ok)

	_, _, _, matchUsageID := createErrorDiagnosticUsageFixture(t, ctx)
	cleanupUsageLogFixture(t, matchUsageID)
	matchDigest := strings.Repeat("a1", 32)
	insertAuditAttemptsJSON(t, matchUsageID, matchDigest, realHandlerAuditAttempts())
	match := createPlainDiagnostic(t, matchDigest, 2, 429) // wire#2 = 429

	_, _, _, mismatchUsageID := createErrorDiagnosticUsageFixture(t, ctx)
	cleanupUsageLogFixture(t, mismatchUsageID)
	mismatchDigest := strings.Repeat("b1", 32)
	insertAuditAttemptsJSON(t, mismatchUsageID, mismatchDigest, realHandlerAuditAttempts())
	mismatch := createPlainDiagnostic(t, mismatchDigest, 2, 502) // wire#2 是 429，不是 502

	_, err := reconciler.ReconcilePlainErrorDiagnosticLinks(ctx, time.Now().UTC(), 100)
	require.NoError(t, err)

	require.True(t, plainDiagnosticOwner(t, match.ID).Valid, "序号与状态都吻合的诊断必须被补关联")
	require.Equal(t, matchUsageID, plainDiagnosticOwner(t, match.ID).Int64)
	require.False(t, plainDiagnosticOwner(t, mismatch.ID).Valid, "状态不吻合时绝不猜一个关联")
}

// TestPlainErrorDiagnosticLinkRefusesNotCapturedAudit 覆盖未采集审计：capture_completeness
// = 'not_captured' 的审计即使带了一条形状完好的 wire 尝试，也不得成为关联依据。正向关联
// 与补偿关联（reconciler）必须使用同一拒绝条件，否则一条伪造的未采集审计能绕过正向谓词、
// 由补偿路径建立所有权。
func TestPlainErrorDiagnosticLinkRefusesNotCapturedAudit(t *testing.T) {
	ctx := context.Background()
	repo := NewErrorDiagnosticRepository(integrationDB, nil)
	attacher := repo.(service.ErrorDiagnosticUsageAttacher)
	reconciler := repo.(service.ErrorDiagnosticLinkReconciler)

	_, _, _, usageID := createErrorDiagnosticUsageFixture(t, ctx)
	cleanupUsageLogFixture(t, usageID)
	digest := strings.Repeat("e2", 32)
	// 合成一条形状完好的 wire 尝试：序号与状态都能对上诊断行，唯一的问题是它未被采集。
	insertAuditWithCompleteness(t, usageID, digest, realHandlerAuditAttempts(), "not_captured")
	diag := createPlainDiagnostic(t, digest, 2, 429)

	linked, err := attacher.LinkPlainErrorDiagnostics(ctx, usageID, digest, time.Now().UTC())
	require.NoError(t, err)
	require.Zero(t, linked, "未采集审计不得作为正向关联依据")

	_, err = reconciler.ReconcilePlainErrorDiagnosticLinks(ctx, time.Now().UTC(), 100)
	require.NoError(t, err)
	require.False(t, plainDiagnosticOwner(t, diag.ID).Valid, "补偿关联同样不得使用未采集审计")
}

// TestPlainErrorDiagnosticLinkToleratesMalformedAttempts 覆盖形态异常：非数组 attempts 与
// 非数字状态都不得触发 JSON 转换错误，只按「不匹配」处理（fail-open 旁路不能让一条坏行
// 拖垮整批关联/补关联）。
func TestPlainErrorDiagnosticLinkToleratesMalformedAttempts(t *testing.T) {
	ctx := context.Background()
	repo := NewErrorDiagnosticRepository(integrationDB, nil)
	attacher := repo.(service.ErrorDiagnosticUsageAttacher)
	reconciler := repo.(service.ErrorDiagnosticLinkReconciler)

	// attempts 不是数组。
	_, _, _, nonArrayUsageID := createErrorDiagnosticUsageFixture(t, ctx)
	cleanupUsageLogFixture(t, nonArrayUsageID)
	nonArrayDigest := strings.Repeat("c1", 32)
	insertAuditAttemptsJSON(t, nonArrayUsageID, nonArrayDigest, `{"oops":true}`)
	nonArray := createPlainDiagnostic(t, nonArrayDigest, 1, 500)
	linked, err := attacher.LinkPlainErrorDiagnostics(ctx, nonArrayUsageID, nonArrayDigest, time.Now().UTC())
	require.NoError(t, err, "非数组 attempts 必须按不匹配处理，而不是报错")
	require.Zero(t, linked)
	require.False(t, plainDiagnosticOwner(t, nonArray.ID).Valid)

	// wire 元素的 upstream_status 不是数字。
	_, _, _, badStatusUsageID := createErrorDiagnosticUsageFixture(t, ctx)
	cleanupUsageLogFixture(t, badStatusUsageID)
	badStatusDigest := strings.Repeat("d1", 32)
	insertAuditAttemptsJSON(t, badStatusUsageID, badStatusDigest, `[
		{"stage":"wire","upstream_status":"not-a-number"}
	]`)
	badStatus := createPlainDiagnostic(t, badStatusDigest, 1, 500)
	linked, err = attacher.LinkPlainErrorDiagnostics(ctx, badStatusUsageID, badStatusDigest, time.Now().UTC())
	require.NoError(t, err, "非数字状态不得触发整数转换错误")
	require.Zero(t, linked)
	require.False(t, plainDiagnosticOwner(t, badStatus.ID).Valid)

	// 补关联整批跑过这些坏行也不能报错，且不会误绑。
	_, err = reconciler.ReconcilePlainErrorDiagnosticLinks(ctx, time.Now().UTC(), 100)
	require.NoError(t, err)
	require.False(t, plainDiagnosticOwner(t, nonArray.ID).Valid)
	require.False(t, plainDiagnosticOwner(t, badStatus.ID).Valid)
}
