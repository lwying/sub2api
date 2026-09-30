-- 导出任务在创建时固定的资源上限快照（规格 §2.4）。
--
-- 规格要求"新任务取创建时的上限快照"：任务创建时把当时生效的总条数/字节/
-- 运行时长固定下来，排队期间管理员改配置不再改变它的预算。没有这一列时，上限
-- 只能在执行时读一次当前配置，"改配置只影响之后开始的任务"就无法兑现。
--
-- 列可空且没有默认值：升级前创建的任务没有快照（NULL），执行时退回当时的生效
-- 配置，不按任何规则回填历史任务。
--
-- 与 request_trace_stages.metadata（261）一样，JSONB 不能只靠应用层自律：
-- 一个漏检的写入方、一次手工 psql、或跳过了服务端归一化的部署都可能在这里放下
-- 一个数组、标量、无界 blob 或让它变成"没有上限"。本迁移补上数据库侧的护栏：
--
--   1. 值必须是 JSON 对象，且只允许 RequestTraceExportLimits 产出的封闭键集
--      （max_rows, max_bytes, max_runtime_seconds, max_shard_rows,
--       max_shard_bytes, configured）；
--   2. 规范文本长度有界，一行不可能借这一列携带 blob；
--   3. 五个数值字段必须存在、必须是数字，并落在服务端允许的区间内，单片上限
--      不得超过整任务上限——上限永远是有限的，且"缺字段"不会被读成零值。
--
-- 约束按校验（非 NOT VALID）方式添加：request_trace_exports 是同一次尚未发布、
-- 默认关闭的 Trace 能力建的表，行数只有单实例自己写下的那几行，扫描它可以证明
-- 已存在的行本来就合规，而不是永远相信应用层守卫。每个约束先按名字 DROP：迁移
-- 运行器按 checksum 跳过已应用的文件，但灾难恢复会重放本文件，重放不能因为
-- "constraint already exists" 失败（见 migrations/README.md）。
ALTER TABLE request_trace_exports
    ADD COLUMN IF NOT EXISTS limits_snapshot JSONB;

ALTER TABLE request_trace_exports
    DROP CONSTRAINT IF EXISTS request_trace_exports_limits_snapshot_shape_allowed;
ALTER TABLE request_trace_exports
    ADD CONSTRAINT request_trace_exports_limits_snapshot_shape_allowed CHECK (
        limits_snapshot IS NULL OR (
            CASE
                WHEN jsonb_typeof(limits_snapshot) = 'object' THEN
                    (limits_snapshot - ARRAY[
                        'max_rows', 'max_bytes', 'max_runtime_seconds',
                        'max_shard_rows', 'max_shard_bytes', 'configured'
                    ]) = '{}'::jsonb
                    AND jsonb_typeof(limits_snapshot -> 'max_rows') = 'number'
                    AND jsonb_typeof(limits_snapshot -> 'max_bytes') = 'number'
                    AND jsonb_typeof(limits_snapshot -> 'max_runtime_seconds') = 'number'
                    AND jsonb_typeof(limits_snapshot -> 'max_shard_rows') = 'number'
                    AND jsonb_typeof(limits_snapshot -> 'max_shard_bytes') = 'number'
                    AND ((limits_snapshot -> 'configured') IS NULL
                         OR jsonb_typeof(limits_snapshot -> 'configured') = 'boolean')
                ELSE FALSE
            END
            AND octet_length(limits_snapshot::text) <= 512
        )
    );

-- 数值区间的判定写在独立的 CASE 里：SQL 的 AND 不保证求值顺序，先做类型判定再
-- 转换，才能保证非数字的取值得到"约束不通过"而不是一个转换错误（261 同样做法）。
ALTER TABLE request_trace_exports
    DROP CONSTRAINT IF EXISTS request_trace_exports_limits_snapshot_bounds_allowed;
ALTER TABLE request_trace_exports
    ADD CONSTRAINT request_trace_exports_limits_snapshot_bounds_allowed CHECK (
        limits_snapshot IS NULL OR (
            CASE
                WHEN jsonb_typeof(limits_snapshot) = 'object' THEN
                    CASE WHEN jsonb_typeof(limits_snapshot -> 'max_rows') = 'number'
                        THEN (limits_snapshot ->> 'max_rows')::numeric BETWEEN 100 AND 5000000
                        ELSE FALSE END
                    AND CASE WHEN jsonb_typeof(limits_snapshot -> 'max_bytes') = 'number'
                        THEN (limits_snapshot ->> 'max_bytes')::numeric BETWEEN 1048576 AND 68719476736
                        ELSE FALSE END
                    AND CASE WHEN jsonb_typeof(limits_snapshot -> 'max_runtime_seconds') = 'number'
                        THEN (limits_snapshot ->> 'max_runtime_seconds')::numeric BETWEEN 30 AND 21600
                        ELSE FALSE END
                    AND CASE WHEN jsonb_typeof(limits_snapshot -> 'max_shard_rows') = 'number'
                        THEN (limits_snapshot ->> 'max_shard_rows')::numeric BETWEEN 10 AND 5000000
                        ELSE FALSE END
                    AND CASE WHEN jsonb_typeof(limits_snapshot -> 'max_shard_bytes') = 'number'
                        THEN (limits_snapshot ->> 'max_shard_bytes')::numeric BETWEEN 1048576 AND 68719476736
                        ELSE FALSE END
                    AND CASE WHEN jsonb_typeof(limits_snapshot -> 'max_shard_rows') = 'number'
                            AND jsonb_typeof(limits_snapshot -> 'max_rows') = 'number'
                        THEN (limits_snapshot ->> 'max_shard_rows')::numeric <= (limits_snapshot ->> 'max_rows')::numeric
                        ELSE FALSE END
                    AND CASE WHEN jsonb_typeof(limits_snapshot -> 'max_shard_bytes') = 'number'
                            AND jsonb_typeof(limits_snapshot -> 'max_bytes') = 'number'
                        THEN (limits_snapshot ->> 'max_shard_bytes')::numeric <= (limits_snapshot ->> 'max_bytes')::numeric
                        ELSE FALSE END
                ELSE FALSE
            END
        )
    );

COMMENT ON COLUMN request_trace_exports.limits_snapshot IS
    '任务创建时固定的资源上限快照；NULL 表示升级前创建的任务没有快照，执行时按当前的生效配置处理。';
