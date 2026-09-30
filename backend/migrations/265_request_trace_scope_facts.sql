-- 请求 Trace 的采集范围事实：下游 API Key 分组与客户端请求模型。
--
-- 这两个值是"请求当时"观察到的：分组来自当次鉴权成功的 API Key，模型来自客户端
-- 请求体（不是出站映射后的模型名）。它们只用于按采集范围判定与页面检索，
-- 不随分组改名、账号迁移或模型重映射而变化，也不从使用记录反推。
--
-- 两者都可为 NULL，表示该事实**未被观察到**（例如鉴权前被拒、请求体未读）。
-- 查询侧必须把 NULL 当作"未知"，不得当成某个具体值，也不得回填历史行。
--
-- 本文件只加列。列上的索引按仓库约定放在 267_request_trace_scope_indexes_notx.sql：
-- 建索引会拿写锁，高写入表的索引必须放在非事务迁移里建（见 migrations/README.md）。
ALTER TABLE request_traces
    ADD COLUMN IF NOT EXISTS group_id BIGINT,
    ADD COLUMN IF NOT EXISTS requested_model TEXT;

COMMENT ON COLUMN request_traces.group_id IS
    '请求当时下游 API Key 所属分组；NULL 表示未观察到（例如鉴权前拒绝）。';
COMMENT ON COLUMN request_traces.requested_model IS
    '客户端请求的模型名（非出站映射结果）；NULL 表示未观察到。';
