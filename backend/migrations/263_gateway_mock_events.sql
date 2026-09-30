-- 下游测试请求的本地 mock 最小事件。
--
-- 只记录"网关在这里返回过一次本地 mock"这一事实及其可观察元数据：命中规则的 ID 与
-- 版本、不透明请求摘要、协议、模型、以及当下的 API Key/用户/分组/账号。**不保存**
-- 管理员配置的关键词与回复正文，也不保存任何模型正文；因此它不会因为规则被删除或
-- 修改而泄漏已经撤销的配置内容。
--
-- 该表没有 usage_logs 外键：本地 mock 不产生使用记录，也就不能依附 usage 生命周期。
-- 清理直接复用使用记录的保留策略（见 dashboard_aggregation_service 的保留清理），
-- 因此 cleanup_after 是"按当次策略算出的清理截止时刻"，而不是固定期限。
-- 预算耗尽时按最长期限兜底，避免无限期留存。
CREATE TABLE IF NOT EXISTS gateway_mock_events (
    id BIGSERIAL PRIMARY KEY,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    rule_id TEXT NOT NULL,
    rule_version TEXT NOT NULL DEFAULT '',
    protocol TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    request_digest TEXT NOT NULL DEFAULT '',
    api_key_id BIGINT NOT NULL DEFAULT 0,
    user_id BIGINT NOT NULL DEFAULT 0,
    group_id BIGINT NOT NULL DEFAULT 0,
    account_id BIGINT NOT NULL DEFAULT 0,
    client_ip TEXT NOT NULL DEFAULT '',
    trace_id TEXT NOT NULL DEFAULT '',
    cleanup_after TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '730 days'),
    CONSTRAINT gateway_mock_events_protocol_allowed CHECK (
        protocol IN ('messages', 'chat_completions', 'responses')
    ),
    CONSTRAINT gateway_mock_events_rule_id_present CHECK (btrim(rule_id) <> '')
);

COMMENT ON TABLE gateway_mock_events IS '下游测试请求 mock 的最小事件元数据；不含关键词与回复正文，清理沿用使用记录保留策略。';
COMMENT ON COLUMN gateway_mock_events.cleanup_after IS '按当次使用记录保留策略算出的清理截止；使用记录清理被停用时按最长兜底期限。';

-- 清理扫描按 (cleanup_after, id) 有界推进。
CREATE INDEX IF NOT EXISTS gateway_mock_events_cleanup_idx ON gateway_mock_events (cleanup_after, id);
-- 管理端按时间倒序查看最近命中。
CREATE INDEX IF NOT EXISTS gateway_mock_events_occurred_idx ON gateway_mock_events (occurred_at DESC, id DESC);
