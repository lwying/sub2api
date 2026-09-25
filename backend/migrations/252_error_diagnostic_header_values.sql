-- Claude Messages 上游 429 诊断的请求／响应头**值**留存列。
--
-- 范围刻意比正文诊断更窄（ADR 0005 的 429 头值例外）：
--   * 只有 Messages 协议、且这次真实上游尝试恰好收到 HTTP 429 时才有值可采；
--     其它状态码（含其它 4xx/5xx）与其它协议一律按「未采集」记录，不写任何头值；
--   * 采集与正文留存**互不影响**：本列有自己的开关，正文开关关闭时头值照样成立，
--     头值开关关闭时正文行为一字不改；
--   * 只保存通过闭集白名单与有界校验的**值**，不保存原始 http.Header，也不保存
--     未知头名、Cookie／Set-Cookie／Authorization／X-Api-Key／WWW-Authenticate 等凭据类
--     头的值（这些头即使出现也只做存在性判断，存在性本身不落库）。
-- 列名与语义与正文列同构，但两套 TTL 独立计算，互不覆盖。
--
-- 保留期（按 created_at 计算）：header_expires_at = created_at + 7 天，从该时刻起 API
-- 立即拒绝读取头值。在线主库上的物理清除同样由周期批处理执行（每轮有批量上限，
-- 停机、积压或单轮未取完都会推迟它），因此这里不承诺到期即已物理删除，
-- 也不承诺物理清除的确切时刻；积压与最老超期时长由既有清理服务的告警与计数暴露。
-- 副本、备份与 PITR 由部署方自行决定保留窗口；本迁移不声称这些存储层的到期不可恢复，
-- 也不构成任何合规删除声明。
--
-- 回滚：ALTER TABLE error_diagnostic_records DROP COLUMN header_expires_at, ...;
-- （回滚会连同在线主库上的头值密文一起丢弃，不可恢复。）

ALTER TABLE error_diagnostic_records
    ADD COLUMN IF NOT EXISTS header_state TEXT NOT NULL DEFAULT 'not_observed',
    ADD COLUMN IF NOT EXISTS header_reason TEXT NOT NULL DEFAULT 'not_observed',
    ADD COLUMN IF NOT EXISTS header_ciphertext BYTEA,
    ADD COLUMN IF NOT EXISTS header_key_version INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS header_bytes INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS header_entry_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS header_expires_at TIMESTAMPTZ;

COMMENT ON COLUMN error_diagnostic_records.header_state IS
    '429 header value state: not_observed/stored/skipped/expired/purged; values only, never credentials';
COMMENT ON COLUMN error_diagnostic_records.header_reason IS
    'Stable reason for the header value state; out-of-scope and opt-out attempts stay explicit';
COMMENT ON COLUMN error_diagnostic_records.header_ciphertext IS
    'Encrypted allowlisted 429 request/response header values (JSON); no plaintext fallback';

-- 状态与原因仍是受限枚举：不允许自由文本，也不允许把「没有留存」写成含糊的未知值。
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'error_diagnostic_records_header_state_allowed'
    ) THEN
        ALTER TABLE error_diagnostic_records
            ADD CONSTRAINT error_diagnostic_records_header_state_allowed CHECK (
                header_state IN ('not_observed', 'stored', 'skipped', 'expired', 'purged')
            );
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'error_diagnostic_records_header_reason_allowed'
    ) THEN
        ALTER TABLE error_diagnostic_records
            ADD CONSTRAINT error_diagnostic_records_header_reason_allowed CHECK (
                header_reason IN (
                    'not_observed',
                    'retained',
                    'skipped_header_retention_disabled',
                    'skipped_encryption_unavailable',
                    'skipped_invalid_values'
                )
            );
    END IF;
    -- 密文只能与到期时刻同时出现；清理置空密文后到期时刻保留，作为「曾留存、已清除」的记录。
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'error_diagnostic_records_header_ciphertext_pairing'
    ) THEN
        ALTER TABLE error_diagnostic_records
            ADD CONSTRAINT error_diagnostic_records_header_ciphertext_pairing CHECK (
                header_ciphertext IS NULL OR header_expires_at IS NOT NULL
            );
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'error_diagnostic_records_header_entry_count_range'
    ) THEN
        ALTER TABLE error_diagnostic_records
            ADD CONSTRAINT error_diagnostic_records_header_entry_count_range CHECK (
                header_entry_count >= 0 AND header_entry_count <= 64
            );
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'error_diagnostic_records_header_bytes_nonnegative'
    ) THEN
        ALTER TABLE error_diagnostic_records
            ADD CONSTRAINT error_diagnostic_records_header_bytes_nonnegative CHECK (
                header_bytes >= 0
            );
    END IF;
    -- 头值到期必须严格早于所在行的元数据到期，否则 7 天头值会随元数据一起被提前删掉。
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'error_diagnostic_records_header_metadata_outlives_headers'
    ) THEN
        ALTER TABLE error_diagnostic_records
            ADD CONSTRAINT error_diagnostic_records_header_metadata_outlives_headers CHECK (
                header_expires_at IS NULL OR metadata_expires_at > header_expires_at
            );
    END IF;
END
$$;

-- 第 7 天头值清理：只扫描仍持有头值密文的行。
CREATE INDEX IF NOT EXISTS error_diagnostic_records_header_expires_at_idx
    ON error_diagnostic_records (header_expires_at)
    WHERE header_ciphertext IS NOT NULL;
