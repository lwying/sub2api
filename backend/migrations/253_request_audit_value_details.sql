-- usage-owned Claude /v1/messages 请求审计的**值**明细旁路表（ADR 0006）。
--
-- 这是一张**独立旁路表**，不是 request_audits 的列扩展：长期存活的 request_audits 行
-- 永远只保存协议元数据与事件骨架，值明细只在本表以密文存在。范围刻意很窄，
-- 四个「只」缺一不可：
--   * 只对入站路由为 /v1/messages、且这次逻辑请求真的走了 Anthropic 上游的请求；
--   * 只保存通过**闭集白名单 + 有界校验**的**值**：入站请求头值、每次真实上游尝试的
--     请求／响应头值、metadata.user_id 解析出的 device_id／session_id／account_uuid，
--     以及最终模型与每次尝试的模型——模型别名由调用方任意指定，可能承载提示词形态的文本，
--     因此**只存在于密文载荷里**，明文列一律不含模型名；
--   * 只保存密文（AES-256-GCM，专用 HKDF 子密钥，密钥代写在 key_version）；
--     没有可用稳定密钥时一律不留存，**不存在明文回退**；
--   * 只在使用记录与 request_audits 行**都已存在**时写入：没有 request_audits 行的
--     调用不写入本表（由写入语句的 EXISTS 守卫保证），并且使用记录被删除时本行级联删除。
--
-- 明确**不**保存：模型正文（用户输入、模型输出、工具参数与结果、流式文本或增量）、
-- Cookie／Set-Cookie／Authorization／X-Api-Key／Proxy-Authorization／WWW-Authenticate
-- 等凭据类头的值、未知头名、原始 http.Header、认证凭据原文。这些约束由服务层在
-- 持久化边界做二次校验；本迁移只固定存储形状与受限枚举。
--
-- 保留期（按 created_at 计算）：expires_at = created_at + 7 天，从该时刻起 API
-- **精确地在该时刻**立即拒绝读取值。在线主库上的密文由周期批处理物理清除
-- （默认约每 10 分钟一轮、每轮至多 500 行）：停机、积压或单轮未取完都会推迟它，
-- 延迟**无硬性最大延迟**，因此这里不承诺到期即已物理删除，也不承诺物理清除的确切时刻。
-- 副本、备份与 PITR 由部署方自行决定保留窗口；本迁移不声称这些存储层的到期不可恢复，
-- 也不构成任何合规删除声明。列表、状态与到期时间不随物理清除消失：清理只置空密文列，
-- 使「曾留存、现已清除」与「从未留存」在状态上可区分。
--
-- 与 429 错误诊断头值（迁移 252）**互不影响**：两者各自有开关、到期时刻与清理，
-- 本迁移既不读写 error_diagnostic_records，也不放宽那张表的任何约束。
--
-- 回滚：DROP TABLE request_audit_value_details;
-- （回滚会连同在线主库上的值密文一起丢弃，不可恢复。）

CREATE TABLE IF NOT EXISTS request_audit_value_details (
    id BIGSERIAL PRIMARY KEY,
    usage_log_id BIGINT NOT NULL UNIQUE REFERENCES usage_logs(id) ON DELETE CASCADE,
    state TEXT NOT NULL DEFAULT 'not_observed',
    reason TEXT NOT NULL DEFAULT 'not_observed',
    route TEXT NOT NULL DEFAULT '',
    protocol TEXT NOT NULL DEFAULT '',
    client_status INTEGER NOT NULL DEFAULT 0,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    entry_count INTEGER NOT NULL DEFAULT 0,
    payload_bytes INTEGER NOT NULL DEFAULT 0,
    key_version INTEGER NOT NULL DEFAULT 0,
    ciphertext BYTEA,
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '7 days'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE request_audit_value_details IS
    'Encrypted Claude /v1/messages request-audit VALUE sidecar; attached to usage_logs 1:1 and only when the request_audits row exists';

COMMENT ON COLUMN request_audit_value_details.usage_log_id IS
    'Owning usage log; deleted with the usage log and never shared across usages';

COMMENT ON COLUMN request_audit_value_details.state IS
    'Value detail state: not_observed/stored/skipped written at capture; expired/purged derived or set by cleanup';

COMMENT ON COLUMN request_audit_value_details.reason IS
    'Stable reason for the value detail state; out-of-scope, opt-out, no-key, invalid and too-many-attempts attempts stay explicit';

-- 明文列刻意**不含模型名**：模型别名由调用方任意指定（既有审计因此从不落库模型名），
-- 它可能承载提示词形态的文本。最终模型与每次尝试的模型都只存在于密文载荷里。
COMMENT ON COLUMN request_audit_value_details.ciphertext IS
    'Encrypted allowlisted header and metadata.user_id values plus the bounded model scalars (JSON); no plaintext fallback';

COMMENT ON COLUMN request_audit_value_details.expires_at IS
    'created_at + 7 days; API reads are rejected exactly at this instant, physical cleanup is periodic and not guaranteed at this instant';

-- 状态与原因仍是受限枚举：不允许自由文本，也不允许把「没有留存」写成含糊的未知值。
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'request_audit_value_details_state_allowed'
    ) THEN
        ALTER TABLE request_audit_value_details
            ADD CONSTRAINT request_audit_value_details_state_allowed CHECK (
                state IN ('not_observed', 'stored', 'skipped', 'expired', 'purged')
            );
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'request_audit_value_details_reason_allowed'
    ) THEN
        ALTER TABLE request_audit_value_details
            ADD CONSTRAINT request_audit_value_details_reason_allowed CHECK (
                reason IN (
                    'not_observed',
                    'retained',
                    'skipped_out_of_scope',
                    'skipped_value_retention_disabled',
                    'skipped_encryption_unavailable',
                    'skipped_invalid_values',
                    'skipped_too_many_attempts'
                )
            );
    END IF;
    -- 密文与「已留存」必须同时成立：自称 stored 却没有密文的行不允许存在。
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'request_audit_value_details_stored_requires_ciphertext'
    ) THEN
        ALTER TABLE request_audit_value_details
            ADD CONSTRAINT request_audit_value_details_stored_requires_ciphertext CHECK (
                state <> 'stored' OR ciphertext IS NOT NULL
            );
    END IF;
    -- 反过来，持有密文的行只能是 stored（仍可读）或 purged（已被清理标记）之外的状态吗？
    -- 不是：expired 表示「已过 7 天但密文尚未被物理清除」，因此它同样允许持有密文。
    -- 唯一被禁止的是 not_observed／skipped 携带密文——那会让「没留存」看起来像存过值。
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'request_audit_value_details_ciphertext_requires_storage'
    ) THEN
        ALTER TABLE request_audit_value_details
            ADD CONSTRAINT request_audit_value_details_ciphertext_requires_storage CHECK (
                ciphertext IS NULL OR state IN ('stored', 'expired', 'purged')
            );
    END IF;
    -- 尝试数与条目数的边界：宽于服务层上限，只用来挡住不可能的形状。
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'request_audit_value_details_attempt_count_range'
    ) THEN
        ALTER TABLE request_audit_value_details
            ADD CONSTRAINT request_audit_value_details_attempt_count_range CHECK (
                attempt_count >= 0 AND attempt_count <= 16
            );
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'request_audit_value_details_entry_count_range'
    ) THEN
        ALTER TABLE request_audit_value_details
            ADD CONSTRAINT request_audit_value_details_entry_count_range CHECK (
                entry_count >= 0 AND entry_count <= 256
            );
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'request_audit_value_details_payload_bytes_nonnegative'
    ) THEN
        ALTER TABLE request_audit_value_details
            ADD CONSTRAINT request_audit_value_details_payload_bytes_nonnegative CHECK (
                payload_bytes >= 0
            );
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'request_audit_value_details_client_status_range'
    ) THEN
        ALTER TABLE request_audit_value_details
            ADD CONSTRAINT request_audit_value_details_client_status_range CHECK (
                client_status >= 0 AND client_status <= 599
            );
    END IF;
    -- 到期时刻必须晚于创建时刻，否则 7 天窗口会在一开始就是关闭的。
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'request_audit_value_details_expires_after_creation'
    ) THEN
        ALTER TABLE request_audit_value_details
            ADD CONSTRAINT request_audit_value_details_expires_after_creation CHECK (
                expires_at > created_at
            );
    END IF;
END
$$;

-- 第 7 天清理：只扫描仍持有密文的行。
CREATE INDEX IF NOT EXISTS request_audit_value_details_expires_at_idx
    ON request_audit_value_details (expires_at)
    WHERE ciphertext IS NOT NULL;
