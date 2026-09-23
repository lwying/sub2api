//go:build integration

package repository

import (
	"context"
	"net/http"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/requestaudit"
	"github.com/Wei-Shaw/sub2api/ent/requestauditreservation"
	"github.com/Wei-Shaw/sub2api/ent/usagelog"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// 验收来源：#3「请求审计最小闭环」验收标准「删除使用记录后审计行不在」，
// 以及 #6 中强制审计预留在上游发送前落库、随后挂接到使用记录（request_audit_reservations.usage_log_id）。
//
// 生产 PostgreSQL 由编号迁移建立级联：
//   - 240_request_audit.sql: request_audits.usage_log_id ... ON DELETE CASCADE
//   - 245_request_audit_reservations.sql: request_audit_reservations.usage_log_id ... ON DELETE CASCADE
//
// 注意：ent/migrate/schema.go 目前对这两个外键声明为 NoAction / SetNull（漂移），
// 本测试刻意以编号迁移为事实来源，不改 Ent schema，也不重新生成 Ent 代码。

// TestRequestAuditCascadeSchemaMatchesNumberedMigrations 断言运行库（应用编号迁移后）中
// 两条审计链路外键确实是 ON DELETE CASCADE，而不是 Ent 生成 schema 里的 NoAction / SetNull。
func TestRequestAuditCascadeSchemaMatchesNumberedMigrations(t *testing.T) {
	tx := testTx(t)

	requireForeignKeyOnDelete(t, tx, "request_audits", "usage_log_id", "usage_logs", "CASCADE")
	requireForeignKeyOnDelete(t, tx, "request_audit_reservations", "usage_log_id", "usage_logs", "CASCADE")

	// 审计行必须外键必填；预留行可在挂接前为空。
	requireColumn(t, tx, "request_audits", "usage_log_id", "bigint", 0, false)
	requireColumn(t, tx, "request_audit_reservations", "usage_log_id", "bigint", 0, true)
}

// TestRequestAuditCascadeDeletesAuditAndLinkedReservationWithUsageLog 覆盖
// 「删除使用记录后审计行不在」：删 usage_logs 一行，request_audits 与该使用记录已挂接的
// request_audit_reservations 都必须由数据库级联一起消失，且不得误删兄弟记录或未挂接预留。
func TestRequestAuditCascadeDeletesAuditAndLinkedReservationWithUsageLog(t *testing.T) {
	ctx := context.Background()

	// 隔离事务：写入只存在于本事务内，测试结束回滚，不会污染共享测试库数据。
	tx := testEntTx(t)
	client := tx.Client()

	usageRepo := newUsageLogRepositoryWithSQL(client, tx)
	auditRepo := NewRequestAuditRepository(client)
	reservationRepo, ok := auditRepo.(service.RequestAuditReservationRepository)
	require.True(t, ok, "请求审计仓储必须提供预留操作")

	targetLog := createCascadeTestUsageLog(t, ctx, client, usageRepo)
	siblingLog := createCascadeTestUsageLog(t, ctx, client, usageRepo)

	targetKey := "cascade-target-" + uuid.NewString()
	siblingKey := "cascade-sibling-" + uuid.NewString()
	unlinkedKey := "cascade-unlinked-" + uuid.NewString()

	// 目标使用记录：一条审计链路 + 一条已挂接的强制审计预留。
	require.NoError(t, auditRepo.CreateRequestAudit(ctx, &service.RequestAuditRecord{
		UsageLogID:          targetLog.ID,
		CaptureCompleteness: service.RequestAuditCaptureComplete,
	}))
	require.NoError(t, reservationRepo.ReserveAttempt(ctx, service.RequestAuditReservationScope{
		LogicalKey:  targetKey,
		RouteFamily: service.RequestAuditRouteMessages,
		Forced:      true,
		Headers:     http.Header{"Content-Type": []string{"application/json"}},
		ExpiresAt:   time.Now().Add(time.Hour),
	}, cascadeTestAttempt(targetLog.AccountID)))
	require.NoError(t, reservationRepo.MarkReservationIncomplete(ctx, targetKey, targetLog.ID, "finalization_failed"))

	// 兄弟使用记录：同样挂一条审计链路 + 已挂接预留，用于证明级联只作用于被删的那一行。
	require.NoError(t, auditRepo.CreateRequestAudit(ctx, &service.RequestAuditRecord{
		UsageLogID:          siblingLog.ID,
		CaptureCompleteness: service.RequestAuditCaptureComplete,
	}))
	require.NoError(t, reservationRepo.ReserveAttempt(ctx, service.RequestAuditReservationScope{
		LogicalKey:  siblingKey,
		RouteFamily: service.RequestAuditRouteMessages,
		Forced:      true,
		Headers:     http.Header{"Content-Type": []string{"application/json"}},
		ExpiresAt:   time.Now().Add(time.Hour),
	}, cascadeTestAttempt(siblingLog.AccountID)))
	require.NoError(t, reservationRepo.MarkReservationIncomplete(ctx, siblingKey, siblingLog.ID, "finalization_failed"))

	// 未挂接预留（usage_log_id IS NULL）：上游发送前已落库、尚未关联使用记录。
	require.NoError(t, reservationRepo.ReserveAttempt(ctx, service.RequestAuditReservationScope{
		LogicalKey:  unlinkedKey,
		RouteFamily: service.RequestAuditRouteMessages,
		Forced:      true,
		Headers:     http.Header{"Content-Type": []string{"application/json"}},
		ExpiresAt:   time.Now().Add(time.Hour),
	}, cascadeTestAttempt(siblingLog.AccountID)))

	// 前置断言：删除之前，两条使用记录各自都真的有审计行与已挂接预留。
	requireRequestAuditRows(t, ctx, client, targetLog.ID, 1)
	requireRequestAuditRows(t, ctx, client, siblingLog.ID, 1)

	require.NoError(t, usageRepo.Delete(ctx, targetLog.ID))

	targetLogs, err := client.UsageLog.Query().Where(usagelog.IDEQ(targetLog.ID)).Count(ctx)
	require.NoError(t, err, "统计使用记录")
	require.Zero(t, targetLogs, "使用记录已被删除")

	// 核心断言：数据库级联删除了该使用记录的审计行与已挂接预留。
	requireRequestAuditRows(t, ctx, client, targetLog.ID, 0)

	// 兄弟使用记录的审计链路不受影响。
	requireRequestAuditRows(t, ctx, client, siblingLog.ID, 1)

	// 未挂接预留不受影响，且仍然没有 usage_log_id。
	unlinked, err := client.RequestAuditReservation.Query().
		Where(requestauditreservation.LogicalKeyEQ(unlinkedKey)).
		Only(ctx)
	require.NoError(t, err, "读取未挂接预留")
	require.Nil(t, unlinked.UsageLogID, "未挂接预留不应被使用记录删除影响")
}

// createCascadeTestUsageLog 造一条只属于本测试的使用记录（唯一用户 / API Key / 账号 / request_id）。
func createCascadeTestUsageLog(t *testing.T, ctx context.Context, client *dbent.Client, repo *usageLogRepository) *service.UsageLog {
	t.Helper()

	suffix := uuid.NewString()
	user := mustCreateUser(t, client, &service.User{Email: "cascade-" + suffix + "@example.com"})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-cascade-" + suffix, Name: "cascade"})
	account := mustCreateAccount(t, client, &service.Account{Name: "acc-cascade-" + suffix})

	log := &service.UsageLog{
		UserID:       user.ID,
		APIKeyID:     apiKey.ID,
		AccountID:    account.ID,
		RequestID:    suffix,
		Model:        "claude-sonnet-4-5",
		InputTokens:  10,
		OutputTokens: 20,
		TotalCost:    0.5,
		ActualCost:   0.5,
		CreatedAt:    time.Now().UTC(),
	}

	_, err := repo.Create(ctx, log)
	require.NoError(t, err, "创建使用记录")
	require.NotZero(t, log.ID, "使用记录 ID")
	return log
}

func cascadeTestAttempt(accountID int64) service.RequestAuditAttempt {
	return service.RequestAuditAttempt{
		AccountID: accountID,
		Model:     "claude-sonnet-4-5",
		Protocol:  service.RequestAuditProtocolAnthropic,
		Stage:     service.RequestAuditStageWire,
	}
}

// requireRequestAuditRows 断言某条使用记录当下挂着的审计行与已挂接预留数量。
func requireRequestAuditRows(t *testing.T, ctx context.Context, client *dbent.Client, usageLogID int64, expected int) {
	t.Helper()

	audits, err := client.RequestAudit.Query().
		Where(requestaudit.UsageLogID(usageLogID)).
		Count(ctx)
	require.NoError(t, err, "统计 request_audits 行")
	require.Equal(t, expected, audits, "usage_log %d 的 request_audits 行数", usageLogID)

	reservations, err := client.RequestAuditReservation.Query().
		Where(requestauditreservation.UsageLogIDEQ(usageLogID)).
		Count(ctx)
	require.NoError(t, err, "统计 request_audit_reservations 行")
	require.Equal(t, expected, reservations, "usage_log %d 的已挂接 request_audit_reservations 行数", usageLogID)
}
