package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type dashboardAggregationRepository struct {
	sql   sqlExecutor
	clock func() time.Time
}

const usageLogsCleanupBatchSize = 10000
const usageBillingDedupCleanupBatchSize = 10000

// NewDashboardAggregationRepository 创建仪表盘预聚合仓储。
func NewDashboardAggregationRepository(sqlDB *sql.DB) service.DashboardAggregationRepository {
	if sqlDB == nil {
		return nil
	}
	if !isPostgresDriver(sqlDB) {
		log.Printf("[DashboardAggregation] 检测到非 PostgreSQL 驱动，已自动禁用预聚合")
		return nil
	}
	return newDashboardAggregationRepositoryWithSQL(sqlDB)
}

func newDashboardAggregationRepositoryWithSQL(sqlq sqlExecutor) *dashboardAggregationRepository {
	return &dashboardAggregationRepository{sql: sqlq, clock: time.Now}
}

func (r *dashboardAggregationRepository) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

func isPostgresDriver(db *sql.DB) bool {
	if db == nil {
		return false
	}
	_, ok := db.Driver().(*pq.Driver)
	return ok
}

func (r *dashboardAggregationRepository) AggregateRange(ctx context.Context, start, end time.Time) error {
	if r == nil || r.sql == nil {
		return nil
	}
	loc := timezone.Location()
	startLocal := start.In(loc)
	endLocal := end.In(loc)
	if !endLocal.After(startLocal) {
		return nil
	}

	hourStart := startLocal.Truncate(time.Hour)
	hourEnd := endLocal.Truncate(time.Hour)
	if endLocal.After(hourEnd) {
		hourEnd = hourEnd.Add(time.Hour)
	}

	dayStart := truncateToDay(startLocal)
	dayEnd := truncateToDay(endLocal)
	if endLocal.After(dayEnd) {
		dayEnd = dayEnd.Add(24 * time.Hour)
	}

	if db, ok := r.sql.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		txRepo := newDashboardAggregationRepositoryWithSQL(tx)
		if err := txRepo.aggregateRangeInTx(ctx, hourStart, hourEnd, dayStart, dayEnd); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	return r.aggregateRangeInTx(ctx, hourStart, hourEnd, dayStart, dayEnd)
}

func (r *dashboardAggregationRepository) aggregateRangeInTx(ctx context.Context, hourStart, hourEnd, dayStart, dayEnd time.Time) error {
	// 以桶边界聚合，允许覆盖 end 所在桶的剩余区间。
	if err := r.insertHourlyActiveUsers(ctx, hourStart, hourEnd); err != nil {
		return err
	}
	if err := r.insertDailyActiveUsers(ctx, hourStart, hourEnd); err != nil {
		return err
	}
	if err := r.upsertHourlyAggregates(ctx, hourStart, hourEnd); err != nil {
		return err
	}
	if err := r.upsertDailyAggregates(ctx, dayStart, dayEnd); err != nil {
		return err
	}
	return nil
}

func (r *dashboardAggregationRepository) RecomputeRange(ctx context.Context, start, end time.Time) error {
	if r == nil || r.sql == nil {
		return nil
	}
	loc := timezone.Location()
	startLocal := start.In(loc)
	endLocal := end.In(loc)
	if !endLocal.After(startLocal) {
		return nil
	}

	hourStart := startLocal.Truncate(time.Hour)
	hourEnd := endLocal.Truncate(time.Hour)
	if endLocal.After(hourEnd) {
		hourEnd = hourEnd.Add(time.Hour)
	}

	dayStart := truncateToDay(startLocal)
	dayEnd := truncateToDay(endLocal)
	if endLocal.After(dayEnd) {
		dayEnd = dayEnd.Add(24 * time.Hour)
	}

	// 尽量使用事务保证范围内的一致性（允许在非 *sql.DB 的情况下退化为非事务执行）。
	if db, ok := r.sql.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := lockGroupUsageRollupState(ctx, tx); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := invalidateGroupUsageRollupsAt(ctx, tx, start); err != nil {
			_ = tx.Rollback()
			return err
		}
		txRepo := newDashboardAggregationRepositoryWithSQL(tx)
		if err := txRepo.recomputeRangeInTx(ctx, hourStart, hourEnd, dayStart, dayEnd); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := txRepo.syncGroupUsageRollupsInTx(ctx, service.GroupUsageTodayStart(r.now())); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	return r.recomputeRangeInTx(ctx, hourStart, hourEnd, dayStart, dayEnd)
}

func (r *dashboardAggregationRepository) recomputeRangeInTx(ctx context.Context, hourStart, hourEnd, dayStart, dayEnd time.Time) error {
	// 先清空范围内桶，再重建（避免仅增量插入导致活跃用户等指标无法回退）。
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_hourly WHERE bucket_start >= $1 AND bucket_start < $2", hourStart, hourEnd); err != nil {
		return err
	}
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_hourly_users WHERE bucket_start >= $1 AND bucket_start < $2", hourStart, hourEnd); err != nil {
		return err
	}
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_daily WHERE bucket_date >= $1::date AND bucket_date < $2::date", dayStart, dayEnd); err != nil {
		return err
	}
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_daily_users WHERE bucket_date >= $1::date AND bucket_date < $2::date", dayStart, dayEnd); err != nil {
		return err
	}

	if err := r.insertHourlyActiveUsers(ctx, hourStart, hourEnd); err != nil {
		return err
	}
	if err := r.insertDailyActiveUsers(ctx, hourStart, hourEnd); err != nil {
		return err
	}
	if err := r.upsertHourlyAggregates(ctx, hourStart, hourEnd); err != nil {
		return err
	}
	if err := r.upsertDailyAggregates(ctx, dayStart, dayEnd); err != nil {
		return err
	}
	return nil
}

func (r *dashboardAggregationRepository) GetAggregationWatermark(ctx context.Context) (time.Time, error) {
	var ts time.Time
	query := "SELECT last_aggregated_at FROM usage_dashboard_aggregation_watermark WHERE id = 1"
	if err := scanSingleRow(ctx, r.sql, query, nil, &ts); err != nil {
		if err == sql.ErrNoRows {
			return time.Unix(0, 0).UTC(), nil
		}
		return time.Time{}, err
	}
	return ts.UTC(), nil
}

func (r *dashboardAggregationRepository) UpdateAggregationWatermark(ctx context.Context, aggregatedAt time.Time) error {
	query := `
		INSERT INTO usage_dashboard_aggregation_watermark (id, last_aggregated_at, updated_at)
		VALUES (1, $1, NOW())
		ON CONFLICT (id)
		DO UPDATE SET last_aggregated_at = EXCLUDED.last_aggregated_at, updated_at = EXCLUDED.updated_at
	`
	_, err := r.sql.ExecContext(ctx, query, aggregatedAt.UTC())
	return err
}

func (r *dashboardAggregationRepository) CleanupAggregates(ctx context.Context, hourlyCutoff, dailyCutoff time.Time) error {
	hourlyCutoffUTC := hourlyCutoff.UTC()
	dailyCutoffUTC := dailyCutoff.UTC()
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_hourly WHERE bucket_start < $1", hourlyCutoffUTC); err != nil {
		return err
	}
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_hourly_users WHERE bucket_start < $1", hourlyCutoffUTC); err != nil {
		return err
	}
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_daily WHERE bucket_date < $1::date", dailyCutoffUTC); err != nil {
		return err
	}
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_daily_users WHERE bucket_date < $1::date", dailyCutoffUTC); err != nil {
		return err
	}
	return nil
}

func (r *dashboardAggregationRepository) CleanupUsageLogs(ctx context.Context, cutoff time.Time) error {
	isPartitioned, err := r.isUsageLogsPartitioned(ctx)
	if err != nil {
		return err
	}
	if isPartitioned {
		if err := r.dropUsageLogsPartitions(ctx, cutoff); err != nil {
			// Partition DROP skips row-level cascading for plaintext sidecars.
			// Continue with bounded row deletes, which do invoke the FK actions
			// whenever the ownership foreign keys are still in place; report the
			// blocked DROP after that safe progress rather than stalling retention
			// for every later cycle. When the ownership keys are gone the row
			// deletes are refused too, so the error is returned with the data left
			// untouched instead of trading the DROP for an orphan.
			if cleanupErr := r.cleanupUsageLogsBatches(ctx, cutoff); cleanupErr != nil {
				return errors.Join(err, cleanupErr)
			}
			return err
		}
	}
	// Remove the expired part of the boundary partition as well, so a retention
	// window measured in days does not silently round up to a whole month.
	if err := r.cleanupUsageLogsBatches(ctx, cutoff); err != nil {
		return err
	}
	return r.SyncGroupUsageRollups(ctx, service.GroupUsageTodayStart(r.now()))
}

// errPlaintextOwnershipUnverified 表示随 usage 的明文旁路表已部署，但保证「usage 行真
// 删除时它们同步消失」的数据库级所有权外键已经缺失（迁移 253/255 的单列
// `REFERENCES usage_logs(id) ON DELETE CASCADE`，或分区父表上唯一可能的
// (id, created_at) 复合形式）。此时删除 usage 行或 DROP 分区都会让明文变成没有任何
// 外键动作能回收的孤儿，因此清理必须失败关闭：报错、保留数据。
var errPlaintextOwnershipUnverified = errors.New(
	"refusing usage cleanup: plaintext sidecar tables exist without an ON DELETE CASCADE ownership foreign key to usage_logs")

// plaintextSidecarOwnership 是一次目录核对的结论：每张随 usage 明文旁路表是否已部署，
// 以及它的所有权是否仍由数据库外键保证。没部署的表视为无需保护。
type plaintextSidecarOwnership struct {
	valueDetailsExists bool
	valueDetailsOwned  bool
	diagnosticsExists  bool
	diagnosticsOwned   bool
}

// verificationError 在所有权不成立时返回带缺失列名的错误，成立时返回 nil。
func (o plaintextSidecarOwnership) verificationError() error {
	var missing []string
	if !o.valueDetailsOwned {
		missing = append(missing, "request_audit_value_details.usage_log_id")
	}
	if !o.diagnosticsOwned {
		missing = append(missing, "error_diagnostic_records.plain_owner_usage_log_id")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w (%s)", errPlaintextOwnershipUnverified, strings.Join(missing, ", "))
}

// probePlaintextSidecarOwnership 用一次只读系统目录的查询回答：两张随 usage 明文旁路表
// 是否存在，以及各自的所有权外键是否仍在。所有权判定要求：外键（contype='f'）、
// ON DELETE CASCADE（confdeltype='c'）、被引用列是 usage_logs、**与旁路表在同一个
// schema**（不是只比表名，否则同名表或跨 schema 的同名表会被误判为已保证），且
// conkey 里确实包含该旁路表的所有权列。查询不扫描业务行，也不处理不存在的表。
func probePlaintextSidecarOwnership(ctx context.Context, q sqlQueryer) (plaintextSidecarOwnership, error) {
	const query = `
		WITH sidecars AS (
			SELECT to_regclass('request_audit_value_details') AS value_details,
			       to_regclass('error_diagnostic_records') AS diagnostics
		)
		SELECT
			sidecars.value_details IS NOT NULL AS value_details_exists,
			sidecars.value_details IS NULL OR EXISTS (
				SELECT 1
				FROM pg_constraint con
				JOIN pg_class parent ON parent.oid = con.confrelid
				JOIN pg_class child ON child.oid = con.conrelid
				JOIN pg_attribute att ON att.attrelid = con.conrelid AND att.attnum = ANY (con.conkey)
				WHERE con.conrelid = sidecars.value_details
				  AND con.contype = 'f'
				  AND con.confdeltype = 'c'
				  AND parent.relname = 'usage_logs'
				  AND parent.relnamespace = child.relnamespace
				  AND att.attname = 'usage_log_id'
			) AS value_details_owned,
			sidecars.diagnostics IS NOT NULL AS diagnostics_exists,
			sidecars.diagnostics IS NULL OR EXISTS (
				SELECT 1
				FROM pg_constraint con
				JOIN pg_class parent ON parent.oid = con.confrelid
				JOIN pg_class child ON child.oid = con.conrelid
				JOIN pg_attribute att ON att.attrelid = con.conrelid AND att.attnum = ANY (con.conkey)
				WHERE con.conrelid = sidecars.diagnostics
				  AND con.contype = 'f'
				  AND con.confdeltype = 'c'
				  AND parent.relname = 'usage_logs'
				  AND parent.relnamespace = child.relnamespace
				  AND att.attname = 'plain_owner_usage_log_id'
			) AS diagnostics_owned
		FROM sidecars
	`
	var ownership plaintextSidecarOwnership
	if err := scanSingleRow(ctx, q, query, nil,
		&ownership.valueDetailsExists, &ownership.valueDetailsOwned,
		&ownership.diagnosticsExists, &ownership.diagnosticsOwned,
	); err != nil {
		return plaintextSidecarOwnership{}, err
	}
	return ownership, nil
}

// errPlaintextOwnershipLockUnpinned 表示调用方的执行器不是可确认的事务，因此无法把
// 「核对时所有权外键在」这一结论钉到 DELETE：锁一旦离开语句就可能被并发的
// ALTER TABLE ... DROP CONSTRAINT 改写。旁路表已部署时一律拒绝删除。
var errPlaintextOwnershipLockUnpinned = errors.New(
	"refusing usage cleanup: this adapter cannot hold the plaintext ownership lock before deleting usage rows")

// deployedSidecars 返回已部署（存在）的明文旁路表名。
func (o plaintextSidecarOwnership) deployedSidecars() []string {
	deployed := make([]string, 0, 2)
	if o.valueDetailsExists {
		deployed = append(deployed, "request_audit_value_details")
	}
	if o.diagnosticsExists {
		deployed = append(deployed, "error_diagnostic_records")
	}
	return deployed
}

// ensureUsageCleanupPreservesPlaintextOwnership 必须在执行删除的同一个事务里、且在任何
// usage 行 DELETE 与任何分区 DROP 之前调用。数据库级所有权不成立时一律返回错误并保留
// 数据：不做「先删 usage 行再看孤儿」的任何尝试，外键缺失就是不删。
//
// lockPlaintextSidecars=true 只允许在调用方明确持有事务时传（*sql.DB 下守卫自己开的
// 事务，或调用方的 *sql.Tx）：守卫在确认所有权成立后对已部署的旁路表取 ACCESS SHARE
// 锁，再重新核对一次。ACCESS SHARE 只与 ALTER TABLE 需要的 ACCESS EXCLUSIVE 冲突，不会
// 挡住采集写入（ROW EXCLUSIVE），却能把「核对时外键在」钉到事务结束，使并发的
// ALTER TABLE ... DROP CONSTRAINT 无法在核对与 DELETE 之间生效。分区 DROP 门禁已持有
// 更强的 SHARE ROW EXCLUSIVE 锁（走 false）。无法确认事务的适配器不得传 true：它连
// 「锁还在不在」都证明不了，只能失败关闭——见 cleanupUsageLogsBatches 的非事务分支。
func ensureUsageCleanupPreservesPlaintextOwnership(
	ctx context.Context,
	q sqlExecutor,
	lockPlaintextSidecars bool,
) error {
	ownership, err := probePlaintextSidecarOwnership(ctx, q)
	if err != nil {
		return err
	}
	if err := ownership.verificationError(); err != nil {
		return err
	}
	if !lockPlaintextSidecars {
		return nil
	}
	// 只锁已部署的表：未部署的能力没有可孤儿化的行，也不该让 LOCK 因缺表而报错。
	sidecars := make([]string, 0, 2)
	if ownership.valueDetailsExists {
		sidecars = append(sidecars, "request_audit_value_details")
	}
	if ownership.diagnosticsExists {
		sidecars = append(sidecars, "error_diagnostic_records")
	}
	if len(sidecars) == 0 {
		return nil
	}
	for _, table := range sidecars {
		if _, err := q.ExecContext(ctx, "LOCK TABLE "+pq.QuoteIdentifier(table)+" IN ACCESS SHARE MODE"); err != nil {
			return err
		}
	}
	// 拿锁之前可能刚好有一次并发的 DROP CONSTRAINT 提交；拿锁之后外键状态才是稳定的，
	// 所以重新核对一次再决定是否允许删除。
	ownership, err = probePlaintextSidecarOwnership(ctx, q)
	if err != nil {
		return err
	}
	return ownership.verificationError()
}

func (r *dashboardAggregationRepository) cleanupUsageLogsBatches(ctx context.Context, cutoff time.Time) error {
	db, transactional := r.sql.(*sql.DB)
	for {
		if transactional {
			affected, err := cleanupUsageLogsBatchWithRollupInvalidation(ctx, db, cutoff)
			if err != nil {
				return err
			}
			if affected < usageLogsCleanupBatchSize {
				return nil
			}
			continue
		}

		// 该分支没有 *sql.DB 可用，因而拿不到「自己开事务」的保证。只有调用方明确传进来的
		// 事务（*sql.Tx）才可能把 ACCESS SHARE 锁留到 DELETE；其它执行器（连接池包装、
		// 自动提交适配器）连「锁还在不在」都证明不了，一律失败关闭：只要旁路表已部署就
		// 拒绝删除，两张表都未部署（该能力未部署/未迁移）时照常推进保留期。
		if _, callerTx := r.sql.(*sql.Tx); callerTx {
			if err := ensureUsageCleanupPreservesPlaintextOwnership(ctx, r.sql, true); err != nil {
				return err
			}
		} else {
			ownership, err := probePlaintextSidecarOwnership(ctx, r.sql)
			if err != nil {
				return err
			}
			if deployed := ownership.deployedSidecars(); len(deployed) > 0 {
				return fmt.Errorf("%w (%s)", errPlaintextOwnershipLockUnpinned, strings.Join(deployed, ", "))
			}
		}
		res, err := r.sql.ExecContext(ctx, `
			WITH victims AS (
				SELECT tableoid, ctid
				FROM usage_logs
				WHERE created_at < $1
				ORDER BY created_at ASC, id ASC
				LIMIT $2
			)
			DELETE FROM usage_logs
			WHERE (tableoid, ctid) IN (SELECT tableoid, ctid FROM victims)
		`, cutoff.UTC(), usageLogsCleanupBatchSize)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected < usageLogsCleanupBatchSize {
			return nil
		}
	}
}

func cleanupUsageLogsBatchWithRollupInvalidation(ctx context.Context, db *sql.DB, cutoff time.Time) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	rollback := func(err error) (int64, error) {
		_ = tx.Rollback()
		return 0, err
	}

	if err := lockGroupUsageRollupState(ctx, tx); err != nil {
		return rollback(err)
	}
	// 逐行 DELETE 依赖所有权外键把随 usage 明文一起带走；外键被手工去掉后同一条
	// DELETE 会把明文留成孤儿，所以先在同一事务里核对所有权并钉住结论，缺失即整批拒绝。
	if err := ensureUsageCleanupPreservesPlaintextOwnership(ctx, tx, true); err != nil {
		return rollback(err)
	}
	rows, err := tx.QueryContext(ctx, `
		WITH victims AS (
			SELECT tableoid, ctid
			FROM usage_logs
			WHERE created_at < $1
			ORDER BY created_at ASC, id ASC
			LIMIT $2
		)
		DELETE FROM usage_logs
		WHERE (tableoid, ctid) IN (SELECT tableoid, ctid FROM victims)
		RETURNING created_at
	`, cutoff.UTC(), usageLogsCleanupBatchSize)
	if err != nil {
		return rollback(err)
	}

	var affected int64
	var earliestDeletedAt time.Time
	for rows.Next() {
		var deletedAt time.Time
		if err := rows.Scan(&deletedAt); err != nil {
			_ = rows.Close()
			return rollback(err)
		}
		affected++
		if earliestDeletedAt.IsZero() || deletedAt.Before(earliestDeletedAt) {
			earliestDeletedAt = deletedAt
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return rollback(err)
	}
	if err := rows.Close(); err != nil {
		return rollback(err)
	}
	if affected > 0 {
		if err := invalidateGroupUsageRollupsAt(ctx, tx, earliestDeletedAt); err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return affected, nil
}

func (r *dashboardAggregationRepository) CleanupUsageBillingDedup(ctx context.Context, cutoff time.Time) error {
	for {
		res, err := r.sql.ExecContext(ctx, `
			WITH victims AS (
				SELECT ctid, request_id, api_key_id, request_fingerprint, created_at
				FROM usage_billing_dedup
				WHERE created_at < $1
				LIMIT $2
			), archived AS (
				INSERT INTO usage_billing_dedup_archive (request_id, api_key_id, request_fingerprint, created_at)
				SELECT request_id, api_key_id, request_fingerprint, created_at
				FROM victims
				ON CONFLICT (request_id, api_key_id) DO NOTHING
			)
			DELETE FROM usage_billing_dedup
			WHERE ctid IN (SELECT ctid FROM victims)
		`, cutoff.UTC(), usageBillingDedupCleanupBatchSize)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected < usageBillingDedupCleanupBatchSize {
			return nil
		}
	}
}

func (r *dashboardAggregationRepository) EnsureUsageLogsPartitions(ctx context.Context, now time.Time) error {
	isPartitioned, err := r.isUsageLogsPartitioned(ctx)
	if err != nil || !isPartitioned {
		return err
	}
	monthStart := truncateToMonthUTC(now)
	prevMonth := monthStart.AddDate(0, -1, 0)
	nextMonth := monthStart.AddDate(0, 1, 0)

	for _, m := range []time.Time{prevMonth, monthStart, nextMonth} {
		if err := r.createUsageLogsPartition(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (r *dashboardAggregationRepository) insertHourlyActiveUsers(ctx context.Context, start, end time.Time) error {
	tzName := timezone.Name()
	query := `
		INSERT INTO usage_dashboard_hourly_users (bucket_start, user_id)
		SELECT DISTINCT
			date_trunc('hour', created_at AT TIME ZONE $3) AT TIME ZONE $3 AS bucket_start,
			user_id
		FROM usage_logs
		WHERE created_at >= $1 AND created_at < $2
		ON CONFLICT DO NOTHING
	`
	_, err := r.sql.ExecContext(ctx, query, start, end, tzName)
	return err
}

func (r *dashboardAggregationRepository) insertDailyActiveUsers(ctx context.Context, start, end time.Time) error {
	tzName := timezone.Name()
	query := `
		INSERT INTO usage_dashboard_daily_users (bucket_date, user_id)
		SELECT DISTINCT
			(bucket_start AT TIME ZONE $3)::date AS bucket_date,
			user_id
		FROM usage_dashboard_hourly_users
		WHERE bucket_start >= $1 AND bucket_start < $2
		ON CONFLICT DO NOTHING
	`
	_, err := r.sql.ExecContext(ctx, query, start, end, tzName)
	return err
}

func (r *dashboardAggregationRepository) upsertHourlyAggregates(ctx context.Context, start, end time.Time) error {
	tzName := timezone.Name()
	query := `
		WITH hourly AS (
			SELECT
				date_trunc('hour', created_at AT TIME ZONE $3) AT TIME ZONE $3 AS bucket_start,
				COUNT(*) AS total_requests,
				COALESCE(SUM(input_tokens), 0) AS input_tokens,
				COALESCE(SUM(output_tokens), 0) AS output_tokens,
				COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
				COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
				COALESCE(SUM(total_cost), 0) AS total_cost,
				COALESCE(SUM(actual_cost), 0) AS actual_cost,
				COALESCE(SUM(COALESCE(account_stats_cost, total_cost) * COALESCE(account_rate_multiplier, 1)), 0) AS account_cost,
				COALESCE(SUM(COALESCE(duration_ms, 0)), 0) AS total_duration_ms
			FROM usage_logs
			WHERE created_at >= $1 AND created_at < $2
			GROUP BY 1
		),
		user_counts AS (
			SELECT bucket_start, COUNT(*) AS active_users
			FROM usage_dashboard_hourly_users
			WHERE bucket_start >= $1 AND bucket_start < $2
			GROUP BY bucket_start
		)
		INSERT INTO usage_dashboard_hourly (
			bucket_start,
			total_requests,
			input_tokens,
			output_tokens,
			cache_creation_tokens,
			cache_read_tokens,
			total_cost,
			actual_cost,
			account_cost,
			total_duration_ms,
			active_users,
			computed_at
		)
		SELECT
			hourly.bucket_start,
			hourly.total_requests,
			hourly.input_tokens,
			hourly.output_tokens,
			hourly.cache_creation_tokens,
			hourly.cache_read_tokens,
			hourly.total_cost,
			hourly.actual_cost,
			hourly.account_cost,
			hourly.total_duration_ms,
			COALESCE(user_counts.active_users, 0) AS active_users,
			NOW()
		FROM hourly
		LEFT JOIN user_counts ON user_counts.bucket_start = hourly.bucket_start
		ON CONFLICT (bucket_start)
		DO UPDATE SET
			total_requests = EXCLUDED.total_requests,
			input_tokens = EXCLUDED.input_tokens,
			output_tokens = EXCLUDED.output_tokens,
			cache_creation_tokens = EXCLUDED.cache_creation_tokens,
			cache_read_tokens = EXCLUDED.cache_read_tokens,
			total_cost = EXCLUDED.total_cost,
			actual_cost = EXCLUDED.actual_cost,
			account_cost = EXCLUDED.account_cost,
			total_duration_ms = EXCLUDED.total_duration_ms,
			active_users = EXCLUDED.active_users,
			computed_at = EXCLUDED.computed_at
	`
	_, err := r.sql.ExecContext(ctx, query, start, end, tzName)
	return err
}

func (r *dashboardAggregationRepository) upsertDailyAggregates(ctx context.Context, start, end time.Time) error {
	tzName := timezone.Name()
	query := `
		WITH daily AS (
			SELECT
				(bucket_start AT TIME ZONE $5)::date AS bucket_date,
				COALESCE(SUM(total_requests), 0) AS total_requests,
				COALESCE(SUM(input_tokens), 0) AS input_tokens,
				COALESCE(SUM(output_tokens), 0) AS output_tokens,
				COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
				COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
				COALESCE(SUM(total_cost), 0) AS total_cost,
				COALESCE(SUM(actual_cost), 0) AS actual_cost,
				COALESCE(SUM(account_cost), 0) AS account_cost,
				COALESCE(SUM(total_duration_ms), 0) AS total_duration_ms
			FROM usage_dashboard_hourly
			WHERE bucket_start >= $1 AND bucket_start < $2
			GROUP BY (bucket_start AT TIME ZONE $5)::date
		),
		user_counts AS (
			SELECT bucket_date, COUNT(*) AS active_users
			FROM usage_dashboard_daily_users
			WHERE bucket_date >= $3::date AND bucket_date < $4::date
			GROUP BY bucket_date
		)
		INSERT INTO usage_dashboard_daily (
			bucket_date,
			total_requests,
			input_tokens,
			output_tokens,
			cache_creation_tokens,
			cache_read_tokens,
			total_cost,
			actual_cost,
			account_cost,
			total_duration_ms,
			active_users,
			computed_at
		)
		SELECT
			daily.bucket_date,
			daily.total_requests,
			daily.input_tokens,
			daily.output_tokens,
			daily.cache_creation_tokens,
			daily.cache_read_tokens,
			daily.total_cost,
			daily.actual_cost,
			daily.account_cost,
			daily.total_duration_ms,
			COALESCE(user_counts.active_users, 0) AS active_users,
			NOW()
		FROM daily
		LEFT JOIN user_counts ON user_counts.bucket_date = daily.bucket_date
		ON CONFLICT (bucket_date)
		DO UPDATE SET
			total_requests = EXCLUDED.total_requests,
			input_tokens = EXCLUDED.input_tokens,
			output_tokens = EXCLUDED.output_tokens,
			cache_creation_tokens = EXCLUDED.cache_creation_tokens,
			cache_read_tokens = EXCLUDED.cache_read_tokens,
			total_cost = EXCLUDED.total_cost,
			actual_cost = EXCLUDED.actual_cost,
			account_cost = EXCLUDED.account_cost,
			total_duration_ms = EXCLUDED.total_duration_ms,
			active_users = EXCLUDED.active_users,
			computed_at = EXCLUDED.computed_at
	`
	_, err := r.sql.ExecContext(ctx, query, start, end, start, end, tzName)
	return err
}

func (r *dashboardAggregationRepository) isUsageLogsPartitioned(ctx context.Context) (bool, error) {
	query := `
		SELECT EXISTS(
			SELECT 1
			FROM pg_partitioned_table pt
			JOIN pg_class c ON c.oid = pt.partrelid
			WHERE c.relname = 'usage_logs'
		)
	`
	var partitioned bool
	if err := scanSingleRow(ctx, r.sql, query, nil, &partitioned); err != nil {
		return false, err
	}
	return partitioned, nil
}

func (r *dashboardAggregationRepository) dropUsageLogsPartitions(ctx context.Context, cutoff time.Time) error {
	rows, err := r.sql.QueryContext(ctx, `
		SELECT c.relname
		FROM pg_inherits
		JOIN pg_class c ON c.oid = pg_inherits.inhrelid
		JOIN pg_class p ON p.oid = pg_inherits.inhparent
		WHERE p.relname = 'usage_logs'
	`)
	if err != nil {
		return err
	}
	cutoffMonth := truncateToMonthUTC(cutoff)
	type usageLogsPartition struct {
		name  string
		month time.Time
	}
	partitions := make([]usageLogsPartition, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		if !strings.HasPrefix(name, "usage_logs_") {
			continue
		}
		suffix := strings.TrimPrefix(name, "usage_logs_")
		month, err := time.Parse("200601", suffix)
		if err != nil {
			continue
		}
		month = month.UTC()
		if month.Before(cutoffMonth) {
			partitions = append(partitions, usageLogsPartition{name: name, month: month})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	sort.Slice(partitions, func(i, j int) bool {
		return partitions[i].month.Before(partitions[j].month)
	})
	if db, ok := r.sql.(*sql.DB); ok {
		for _, partition := range partitions {
			if err := dropUsageLogsPartitionWithRollupInvalidation(ctx, db, partition.name, partition.month); err != nil {
				return err
			}
		}
		return nil
	}
	if len(partitions) > 0 {
		// A DROP does not run row-level ON DELETE CASCADE. This adapter cannot
		// synchronously guard the deletion, so fail closed.
		return fmt.Errorf("usage partition %s requires transactional plaintext diagnostic cleanup before DROP", partitions[0].name)
	}
	return nil
}

func dropUsageLogsPartitionWithRollupInvalidation(ctx context.Context, db *sql.DB, name string, monthStart time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}

	if err := lockGroupUsageRollupState(ctx, tx); err != nil {
		return rollback(err)
	}
	if err := invalidateGroupUsageRollupsAt(ctx, tx, monthStart); err != nil {
		return rollback(err)
	}
	// PostgreSQL partition DROP bypasses row-level FK deletion. Refuse the
	// DROP while the victim partition owns any plaintext diagnostic rows;
	// usage retention can still proceed by ordinary batched DELETE (which
	// invokes the ON DELETE CASCADE FK). A caller must not bypass this guard.
	//
	// The check and the DROP must also not race a concurrent link: a plaintext
	// diagnostic that acquires its usage owner between the two statements would
	// be dropped without its cascade ever running. SHARE ROW EXCLUSIVE conflicts
	// with the ROW EXCLUSIVE taken by that linking UPDATE, so a link either
	// committed before this lock (and is therefore visible to the check below) or
	// waits until this transaction has finished.
	if _, err := tx.ExecContext(ctx, `LOCK TABLE error_diagnostic_records IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return rollback(err)
	}
	// A concurrent value-detail INSERT could commit after the EXISTS check but
	// before the partition DROP. Serialize both writes and the check with this
	// table lock so the usage-owned plaintext cannot be orphaned.
	if _, err := tx.ExecContext(ctx, `LOCK TABLE request_audit_value_details IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return rollback(err)
	}
	// 上面两个 EXISTS 只按「受害分区此刻仍有可关联的 usage 行」判断，一旦前一轮清理
	// 已经把 usage 行删掉（或外键缺失让它们变成孤儿），分区看起来就干净了。因此在
	// DROP 之前先在同一事务里核对所有权：旁路表已部署而所有权外键缺失时拒绝 DROP，
	// 不能把「已经没人指认的明文」随分区一起丢掉。这里已持有 SHARE ROW EXCLUSIVE
	// （强于 ACCESS SHARE），并发的 DROP CONSTRAINT 无法在核对与 DROP 之间生效。
	if err := ensureUsageCleanupPreservesPlaintextOwnership(ctx, tx, false); err != nil {
		return rollback(fmt.Errorf("usage partition %s: %w", name, err))
	}
	var linkedValueDetails bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM request_audit_value_details v
			JOIN usage_logs u ON u.id = v.usage_log_id
			WHERE u.tableoid = $1::regclass
			  AND v.storage_format = 'plaintext_usage_bound'
		)`, name).Scan(&linkedValueDetails); err != nil {
		return rollback(err)
	}
	if linkedValueDetails {
		return rollback(fmt.Errorf("usage partition %s contains usage-owned plaintext value details; refusing DROP", name))
	}
	var linkedPlaintext bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM error_diagnostic_records d
			JOIN usage_logs u ON u.id = d.plain_owner_usage_log_id
			WHERE u.tableoid = $1::regclass
			  AND d.plain_record
		)`, name).Scan(&linkedPlaintext); err != nil {
		return rollback(err)
	}
	if linkedPlaintext {
		return rollback(fmt.Errorf("usage partition %s contains linked plaintext diagnostics; refusing DROP", name))
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", pq.QuoteIdentifier(name))); err != nil {
		return rollback(err)
	}
	return tx.Commit()
}

func (r *dashboardAggregationRepository) createUsageLogsPartition(ctx context.Context, month time.Time) error {
	monthStart := truncateToMonthUTC(month)
	nextMonth := monthStart.AddDate(0, 1, 0)
	name := fmt.Sprintf("usage_logs_%s", monthStart.Format("200601"))
	query := fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s PARTITION OF usage_logs FOR VALUES FROM (%s) TO (%s)",
		pq.QuoteIdentifier(name),
		pq.QuoteLiteral(monthStart.Format("2006-01-02")),
		pq.QuoteLiteral(nextMonth.Format("2006-01-02")),
	)
	_, err := r.sql.ExecContext(ctx, query)
	return err
}

func truncateToDay(t time.Time) time.Time {
	return timezone.StartOfDay(t)
}

func truncateToMonthUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
