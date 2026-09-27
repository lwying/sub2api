package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// errPlaintextCaptureSupportProbeUnavailable 表示探针没有可用的查询接缝（构造错误）。
// 它只是一种 error，由服务层收敛成 probe_failed，不对外暴露。
var errPlaintextCaptureSupportProbeUnavailable = errors.New("plaintext capture support probe has no queryer")

// 三类新明文采集（值明细、独立诊断正文、独立诊断 429 头值）的部署探针。
//
// 这三类明文的共同生命周期是「可靠关联到使用记录后随该使用记录删除」，数据库里真正的
// 保证是两张旁路表到 usage_logs 的 ON DELETE CASCADE 所有权外键。探针只读系统目录，
// 回答的问题只有一个：**这台部署的库形态还能不能兑现这个保证**。
//
// 判据（与清理侧 dashboard_aggregation_repo.go 的失败关闭判据同向，因此「探针说受支持」
// 与「清理敢删」不会各说各话）：
//
//	usage_logs 是分区表                                  ⇒ 不支持（分区 DROP 不触发行级外键动作）
//	request_audit_value_details 在，但 usage_log_id 上
//	  没有指向 usage_logs 的 ON DELETE CASCADE 外键       ⇒ 不支持（逐行 DELETE 不再级联）
//	error_diagnostic_records 在，但 plain_owner_usage_log_id
//	  上没有同样的外键                                   ⇒ 不支持
//
// 旁路表不存在按「该能力尚未部署」处理（没有行可成为孤儿），不视为不支持。
// 判定结果只含闭集原因码：数据库错误原文不进响应面，也不进日志。

// plaintextCaptureSupportProbe 是探针的具体实现，依赖只读查询接缝（*sql.DB 或 *sql.Tx），
// 使集成测试可以在固定 search_path 的事务里复刻分区/缺外键形态。
type plaintextCaptureSupportProbe struct {
	q sqlQueryer
}

// NewPlaintextCaptureSupportProbe 构造生产探针。db 为只读系统目录查询的连接池。
func NewPlaintextCaptureSupportProbe(db *sql.DB) service.PlaintextCaptureSupportProbe {
	return &plaintextCaptureSupportProbe{q: db}
}

var _ service.PlaintextCaptureSupportProbe = (*plaintextCaptureSupportProbe)(nil)

// ProbePlaintextCaptureSupport 报告本部署是否支持新明文采集。
//
// 只读一次系统目录（pg_partitioned_table / pg_constraint / pg_attribute），不扫描业务行。
// 查询失败（权限、连接、目录不可用）返回 error，由服务层收敛成 probe_failed 并 fail closed：
// 判不出来与判出来「不支持」是两件事，但都不允许开启新明文。
func (p *plaintextCaptureSupportProbe) ProbePlaintextCaptureSupport(ctx context.Context) (service.PlaintextCaptureSupport, error) {
	if p == nil || p.q == nil {
		// 没有查询接缝就没有结论；返回 error 让服务层按 probe_failed 处理。
		return service.PlaintextCaptureSupport{}, errPlaintextCaptureSupportProbeUnavailable
	}
	// 外键判定刻意**只认迁移 253/255 的原形**：恰好一列（conkey[1] ↔ confkey[1]），列名是
	// 该表的所有权列，指向的正是 usage_logs.id（confrelid 取自 to_regclass 的 oid，不按名字
	// 跨 schema 猜），并且 confdeltype='c'（ON DELETE CASCADE）。
	//
	// 不按「键里出现过这个名字」放宽：复合外键只可能建在分区父表上，而分区在上一条判定里
	// 已经被判不支持，因此这里的严格只会让「受支持」更难被满足，不会漏掉任何可达的受支持形态。
	// （清理侧的判据同样只认可级联所有权外键，两侧对「敢不敢启用 / 敢不敢删除」结论同向。）
	const query = `
		WITH rels AS (
			SELECT
				to_regclass('usage_logs') AS usage_logs,
				to_regclass('request_audit_value_details') AS value_details,
				to_regclass('error_diagnostic_records') AS diagnostics
		)
		SELECT
			rels.usage_logs IS NULL AS usage_logs_missing,
			rels.usage_logs IS NOT NULL AND EXISTS (
				SELECT 1 FROM pg_partitioned_table pt WHERE pt.partrelid = rels.usage_logs
			) AS usage_logs_partitioned,
			rels.value_details IS NULL OR EXISTS (
				SELECT 1
				FROM pg_constraint con
				JOIN pg_attribute att ON att.attrelid = con.conrelid AND att.attnum = con.conkey[1]
				JOIN pg_attribute ref ON ref.attrelid = con.confrelid AND ref.attnum = con.confkey[1]
				WHERE con.conrelid = rels.value_details
				  AND con.contype = 'f'
				  AND con.confdeltype = 'c'
				  AND con.confrelid = rels.usage_logs
				  AND array_length(con.conkey, 1) = 1
				  AND att.attname = 'usage_log_id'
				  AND ref.attname = 'id'
			) AS value_details_owned,
			rels.diagnostics IS NULL OR EXISTS (
				SELECT 1
				FROM pg_constraint con
				JOIN pg_attribute att ON att.attrelid = con.conrelid AND att.attnum = con.conkey[1]
				JOIN pg_attribute ref ON ref.attrelid = con.confrelid AND ref.attnum = con.confkey[1]
				WHERE con.conrelid = rels.diagnostics
				  AND con.contype = 'f'
				  AND con.confdeltype = 'c'
				  AND con.confrelid = rels.usage_logs
				  AND array_length(con.conkey, 1) = 1
				  AND att.attname = 'plain_owner_usage_log_id'
				  AND ref.attname = 'id'
			) AS diagnostics_owned
		FROM rels
	`
	var missing, partitioned, valueDetailsOwned, diagnosticsOwned bool
	if err := scanSingleRow(ctx, p.q, query, nil, &missing, &partitioned, &valueDetailsOwned, &diagnosticsOwned); err != nil {
		return service.PlaintextCaptureSupport{}, err
	}
	switch {
	case missing:
		// usage_logs 都不在：这不是「尚未部署明文旁路」，而是连归属关系本身都无从谈起。
		// 归入未知形态而不是「支持」——没有 usage_logs 就没有可验证的所有权。
		return service.PlaintextCaptureSupport{Reason: service.PlaintextCaptureSupportReasonUnknownDeployment}, nil
	case partitioned:
		// 分区父表不可能带迁移 253/255 的单列所有权外键；分区 DROP 也不触发行级外键动作。
		return service.PlaintextCaptureSupport{Reason: service.PlaintextCaptureSupportReasonPartitionedUsageLogs}, nil
	case !valueDetailsOwned || !diagnosticsOwned:
		return service.PlaintextCaptureSupport{Reason: service.PlaintextCaptureSupportReasonMissingOwnership}, nil
	default:
		return service.PlaintextCaptureSupport{Supported: true, Reason: service.PlaintextCaptureSupportReasonSupported}, nil
	}
}
