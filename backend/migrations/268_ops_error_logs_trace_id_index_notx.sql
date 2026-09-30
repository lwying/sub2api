-- 运维错误记录按服务端 Trace ID 检索的索引（列由 264 添加）。
--
-- ops_error_logs 是持续写入的热表，建索引必须走 CONCURRENTLY，否则会持有写锁把
-- 并行到达的错误记录挡在迁移后面（见 migrations/README.md 与
-- 148_add_ops_error_logs_user_time_index_notx.sql 的同类理由）。
--
-- 部分索引：绝大多数历史行的 request_trace_id 为空，把它们排除在索引之外
-- 既更小也更快，同时不改变"没有 Trace ID 就查不到"的语义。
--
-- 本文件只允许并发索引语句，不含任何其它 DDL/DML。
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ops_error_logs_request_trace_id
    ON ops_error_logs (request_trace_id)
    WHERE request_trace_id IS NOT NULL;
