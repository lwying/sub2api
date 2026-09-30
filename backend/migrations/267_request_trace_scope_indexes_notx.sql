-- 采集范围事实（265 的列）与阶段平台键（266）上的检索索引。
--
-- 这些表是高写入路径（抓取器每条请求都要写 Trace 与阶段），按仓库约定把建索引
-- 放在 _notx 迁移里用 CONCURRENTLY 执行：普通 CREATE INDEX 会持有写锁，
-- 把接入期间的 Trace 写入堵在迁移后面（见 migrations/README.md 与
-- 233_add_usage_log_billing_dedup_created_at_brin_notx.sql 的同类理由）。
--
-- 本文件只允许并发索引语句，不含任何其它 DDL/DML。
CREATE INDEX CONCURRENTLY IF NOT EXISTS request_traces_group_id_idx
    ON request_traces (group_id, created_at DESC);

CREATE INDEX CONCURRENTLY IF NOT EXISTS request_traces_requested_model_idx
    ON request_traces (requested_model, created_at DESC);
