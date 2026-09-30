-- 逐条运维错误记录携带服务端请求 Trace ID，用于从错误详情直达对应的请求 Trace。
--
-- 该列只保存网关在请求进入时生成的**服务端** Trace ID；绝不接受客户端
-- X-Request-ID 之类可重复的值，因此它不会把两条不同请求错误地关联到同一条 Trace。
-- 历史行保持 NULL（上线前不存在该关联），不按时间或客户端 ID 猜测回填。
--
-- 本文件只加列。列上的索引按仓库约定放在 268_ops_error_logs_trace_id_index_notx.sql：
-- ops_error_logs 是持续写入的热表，建索引必须用不阻塞写入的方式，因此放在非事务迁移里
-- （见 migrations/README.md 与 148_add_ops_error_logs_user_time_index_notx.sql）。
ALTER TABLE ops_error_logs
    ADD COLUMN IF NOT EXISTS request_trace_id VARCHAR(32);

COMMENT ON COLUMN ops_error_logs.request_trace_id IS
    '服务端生成的请求 Trace ID；仅用于从错误详情直达 Trace，NULL 表示该次错误与 Trace 无可靠关联。';
