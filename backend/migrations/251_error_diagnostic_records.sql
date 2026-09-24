-- 错误诊断记录（error diagnostic records）。
--
-- 独立于 usage-owned request audit 的短期诊断事实：每次真正发出的上游尝试收到
-- 4xx/5xx 时各写一行，即使逻辑请求随后重试成功；没有使用记录时也成立。
-- 本表只放受限枚举状态与有限关联标识，不放原始 URL、请求头、错误消息或身份字段；
-- 票 01 只写元数据，票 02 的加密出站正文放在同一行的 body_ciphertext 列。
--
-- 主键是应用生成的不可猜 opaque 标识（128 位随机值的 hex），不是可枚举序号。
--
-- 保留期（按 created_at 计算）。两个时刻的含义必须分开理解：
--   * body_expires_at     = created_at + 7 天：**从该时刻起 API 立即拒绝读取正文**；
--   * metadata_expires_at = created_at + 30 天：**从该时刻起 API 立即拒绝列表／详情**。
-- 在线主库上的物理清除是周期任务：正文清空密文列、元数据删除整行，每轮有批量上限，
-- 停机、积压或单轮未取完都会推迟它。因此这里**不承诺**「到期即已物理删除」，
-- 也不承诺物理清除的确切时刻；积压量与最老超期时长由清理服务上报，供运维判断是否落后。
-- 副本、备份与 PITR 由部署方自行决定保留窗口；本迁移不声称这些存储层的到期不可恢复，
-- 也不构成任何合规删除声明。
--
-- 关联语义：usage_log_id 可为空（无 usage 的失败也成立）；使用记录被删除时置空而不是级联，
-- 诊断按自身期限继续保留，不阻断 usage 清理。
--
-- 回滚：DROP TABLE error_diagnostic_records;

CREATE TABLE IF NOT EXISTS error_diagnostic_records (
    -- 应用生成的 opaque 尝试标识：32 个 hex 字符。
    diagnostic_id       TEXT PRIMARY KEY,
    -- 可选关联到使用记录；无 usage 的诊断保持 NULL。
    usage_log_id        BIGINT REFERENCES usage_logs(id) ON DELETE SET NULL,
    protocol            TEXT NOT NULL,
    attempt_index       INTEGER NOT NULL,
    stage               TEXT NOT NULL,
    upstream_status     INTEGER NOT NULL,
    body_state          TEXT NOT NULL,
    body_reason         TEXT NOT NULL,
    -- 票 02：仅在实际发出且合格的文本 JSON 被加密后写入。
    body_ciphertext     BYTEA,
    body_key_version    INTEGER NOT NULL DEFAULT 0,
    body_bytes          INTEGER NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    metadata_expires_at TIMESTAMPTZ NOT NULL,
    body_expires_at     TIMESTAMPTZ,

    CONSTRAINT error_diagnostic_records_id_shape CHECK (
        diagnostic_id ~ '^[0-9a-f]{32}$'
    ),
    CONSTRAINT error_diagnostic_records_protocol_allowed CHECK (
        protocol IN ('messages', 'chat_completions', 'responses')
    ),
    CONSTRAINT error_diagnostic_records_stage_allowed CHECK (
        stage = 'wire'
    ),
    CONSTRAINT error_diagnostic_records_status_range CHECK (
        upstream_status BETWEEN 400 AND 599
    ),
    CONSTRAINT error_diagnostic_records_attempt_index_range CHECK (
        attempt_index >= 0 AND attempt_index <= 1000
    ),
    CONSTRAINT error_diagnostic_records_body_state_allowed CHECK (
        body_state IN ('not_observed', 'stored', 'skipped', 'expired', 'purged')
    ),
    CONSTRAINT error_diagnostic_records_body_reason_allowed CHECK (
        body_reason IN (
            'not_observed',
            'retained',
            'skipped_not_text_json',
            'skipped_too_large',
            'skipped_attachment',
            'skipped_known_credential',
            'skipped_incomplete_read',
            'skipped_encryption_unavailable',
            'skipped_body_retention_disabled'
        )
    ),
    -- 密文只能与到期时刻同时出现；清理置空密文后到期时刻保留，作为「曾留存、已清除」的记录。
    CONSTRAINT error_diagnostic_records_body_ciphertext_pairing CHECK (
        body_ciphertext IS NULL OR body_expires_at IS NOT NULL
    ),
    CONSTRAINT error_diagnostic_records_body_bytes_nonnegative CHECK (
        body_bytes >= 0
    ),
    -- 元数据自身的保留窗口必须严格长于正文，否则 7 天正文无处存放。
    CONSTRAINT error_diagnostic_records_metadata_outlives_body CHECK (
        body_expires_at IS NULL OR metadata_expires_at > body_expires_at
    )
);

-- 第 7 天正文清理：只扫描仍持有密文的行。
CREATE INDEX IF NOT EXISTS error_diagnostic_records_body_expires_at_idx
    ON error_diagnostic_records (body_expires_at)
    WHERE body_ciphertext IS NOT NULL;

-- 第 30 天整行删除：按元数据到期扫描。
CREATE INDEX IF NOT EXISTS error_diagnostic_records_metadata_expires_at_idx
    ON error_diagnostic_records (metadata_expires_at);

-- 管理员按使用记录关联查询（含重试后成功前的失败）。
CREATE INDEX IF NOT EXISTS error_diagnostic_records_usage_log_id_idx
    ON error_diagnostic_records (usage_log_id)
    WHERE usage_log_id IS NOT NULL;

-- 无 usage 诊断的独立入口：按协议 + 时间倒序检索。
CREATE INDEX IF NOT EXISTS error_diagnostic_records_protocol_created_at_idx
    ON error_diagnostic_records (protocol, created_at DESC);
