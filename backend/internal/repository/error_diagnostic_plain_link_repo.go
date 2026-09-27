package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.ErrorDiagnosticUsageAttacher = (*errorDiagnosticRepository)(nil)

// 新明文诊断与 usage 的可靠关联（票据 09）：只有「同一逻辑请求 + 真实传输尝试序号 +
// 该次尝试的上游状态」逐项吻合时才建立所有权；任何一项对不上都不关联，绝不拿一个
// 兜底推断的序号去猜一条关联。
//
// 关联的关键是**真实 wire 序号到审计时间线的映射**。诊断行的 attempt_index 是
// 「本逻辑请求第几次真实上游发送」（1-based，见 httpattempt.Attempt.ordinal 与
// error_diagnostic_observer 的 AttemptOrdinal）。而 request_audits.attempts 由 handler 的
// appendOpenAITransportAttempts 构造：它在**数组前面**追加本地阶段（client_entry、
// post_normalize），真实 wire 尝试只在之后追加（见 handler/request_audit_attempts.go 与
// handler 侧 TestAppendOpenAITransportAttemptsBuildsOneTimeline）。
//
// 所以 `attempts -> (attempt_index - 1)` 是错的：真实形状下它落在 client_entry /
// post_normalize 上，永远绑不上；本地阶段条数变化或状态偶然相等时还会拿错序号猜一次。
// 正确映射只对 stage='wire' 的元素按出现顺序计数、取第 N 个，因此：
//   - 前置本地阶段的条数（0、1、2 或将来更多）都不影响映射；
//   - 序号超出真实 wire 次数时子查询返回 NULL，比较结果不为真 → 不关联，绝不越位；
//   - 状态按文本比对，不对任意 JSON 值做整数转换：形态异常的行只会「不匹配」，不会让
//     整条语句报错（关联/补关联是 fail-open 旁路，一条坏行不能拖垮整批）。

// LinkPlainErrorDiagnostics is a compare-and-set: only a real, non-expired
// diagnostic attempt whose audit belongs to this usage can acquire ownership.
// It does not inspect or store client-supplied request IDs or body bytes.
func (r *errorDiagnosticRepository) LinkPlainErrorDiagnostics(ctx context.Context, usageLogID int64, linkDigest string, now time.Time) (int64, error) {
	if r == nil || r.db == nil {
		return 0, service.ErrErrorDiagnosticUnavailable
	}
	if usageLogID <= 0 || len(linkDigest) != 64 {
		return 0, nil
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE error_diagnostic_records AS diagnostic
		SET plain_owner_usage_log_id = $1, plain_link_digest = NULL
		FROM request_audits AS audit
		WHERE audit.usage_log_id = $1
		  AND audit.metadata -> 'ids' ->> 'local_request_fingerprint' = $2
		  AND audit.capture_completeness <> 'not_captured'
		  AND diagnostic.plain_record
		  AND diagnostic.plain_owner_usage_log_id IS NULL
		  AND diagnostic.plain_link_digest = $2
		  AND diagnostic.plain_link_attempt_index = diagnostic.attempt_index
		  AND diagnostic.plain_link_wire_status = diagnostic.upstream_status
		  AND (
				SELECT wire.element ->> 'upstream_status'
				FROM jsonb_array_elements(
					CASE WHEN jsonb_typeof(audit.attempts) = 'array' THEN audit.attempts ELSE '[]'::jsonb END
				) WITH ORDINALITY AS wire(element, position)
				WHERE wire.element ->> 'stage' = 'wire'
				ORDER BY wire.position
				OFFSET diagnostic.plain_link_attempt_index - 1
				LIMIT 1
			  ) = diagnostic.plain_link_wire_status::text
		  AND diagnostic.metadata_expires_at > $3
	`, usageLogID, linkDigest, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("link error diagnostics to usage: %w", err)
	}
	return result.RowsAffected()
}

// ReconcilePlainErrorDiagnosticLinks repairs diagnostics whose asynchronous
// insert finished after usage was recorded. Both operations remain bounded.
func (r *errorDiagnosticRepository) ReconcilePlainErrorDiagnosticLinks(ctx context.Context, now time.Time, batch int) (int64, error) {
	if r == nil || r.db == nil {
		return 0, service.ErrErrorDiagnosticUnavailable
	}
	if batch <= 0 {
		return 0, nil
	}
	result, err := r.db.ExecContext(ctx, `
		WITH candidates AS (
			SELECT diagnostic.diagnostic_id, audit.usage_log_id
			FROM error_diagnostic_records AS diagnostic
			JOIN request_audits AS audit
			  ON audit.metadata -> 'ids' ->> 'local_request_fingerprint' = diagnostic.plain_link_digest
			 AND audit.capture_completeness <> 'not_captured'
			WHERE diagnostic.plain_record AND diagnostic.plain_owner_usage_log_id IS NULL
			  AND diagnostic.plain_link_digest IS NOT NULL
			  AND diagnostic.metadata_expires_at > $1
			  AND diagnostic.plain_link_attempt_index = diagnostic.attempt_index
			  AND diagnostic.plain_link_wire_status = diagnostic.upstream_status
			  AND (
					SELECT wire.element ->> 'upstream_status'
					FROM jsonb_array_elements(
						CASE WHEN jsonb_typeof(audit.attempts) = 'array' THEN audit.attempts ELSE '[]'::jsonb END
					) WITH ORDINALITY AS wire(element, position)
					WHERE wire.element ->> 'stage' = 'wire'
					ORDER BY wire.position
					OFFSET diagnostic.plain_link_attempt_index - 1
					LIMIT 1
				  ) = diagnostic.plain_link_wire_status::text
			ORDER BY diagnostic.created_at ASC
			LIMIT $2
			FOR UPDATE OF diagnostic SKIP LOCKED
		)
		UPDATE error_diagnostic_records AS diagnostic
		SET plain_owner_usage_log_id = candidates.usage_log_id, plain_link_digest = NULL
		FROM candidates WHERE diagnostic.diagnostic_id = candidates.diagnostic_id
	`, now.UTC(), batch)
	if err != nil {
		return 0, fmt.Errorf("reconcile error diagnostic usage links: %w", err)
	}
	return result.RowsAffected()
}
