-- 普通用户「已分配账号只读查看」能力（票据 05）。
--
-- 两项结构均默认关闭／为空：
--   1. users.can_view_assigned_accounts：逐用户的查看能力开关，默认 false；
--      新注册与管理员创建的用户都不会隐式获得。
--   2. user_visible_accounts：管理员逐用户显式分配的可见账号集合，默认零行。
--
-- 该授权与用户分组／账号路由关系无关，不复用任何调度或分组过滤。
-- 用户或账号被删除时关系行随之清理（ON DELETE CASCADE）；
-- 账号软删除（deleted_at）由查询条件处理，不依赖级联。
-- 回滚：DROP TABLE user_visible_accounts; ALTER TABLE users DROP COLUMN can_view_assigned_accounts;

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS can_view_assigned_accounts BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS user_visible_accounts (
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    -- 授权管理员；仅用于追责，管理员删除不清理本行，故不建外键。
    granted_by BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, account_id)
);

CREATE INDEX IF NOT EXISTS user_visible_accounts_account_id_idx
    ON user_visible_accounts (account_id);
