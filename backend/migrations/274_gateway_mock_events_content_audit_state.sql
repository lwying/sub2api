-- 给下游测试请求 mock 的最小命中事件增加"内容审计是否执行"的有界状态。
--
-- 背景：严格 Mock 命中会在提示词／内容审计之前由本地直接应答，不再执行两类审计。
-- 事件需要如实记录这一事实，同时保持向后兼容：既有记录不追溯猜测，一律按 unknown
-- （未记录）呈现，而不是伪造成 skipped_local_mock。
--
-- 取值闭集：
--   - unknown            ：旧记录，或写入方未给出状态（默认值）。
--   - skipped_local_mock ：新的早期严格命中：本地 Mock 直接应答，未执行内容审计。
--
-- 该列沿用 NOT NULL + DEFAULT 'unknown'：旧实例的 INSERT 不带该列时由默认值补齐，
-- 旧实例的 SELECT 也不读它，因此滚动发布与回滚都不会读不动这一行。列只描述审计动作，
-- 不含审计结论，也不含任何正文。
ALTER TABLE gateway_mock_events
    ADD COLUMN IF NOT EXISTS content_audit_state TEXT NOT NULL DEFAULT 'unknown';

-- 约束与注释都要可重复执行：灾难恢复会重放这个文件。
ALTER TABLE gateway_mock_events
    DROP CONSTRAINT IF EXISTS gateway_mock_events_content_audit_state_allowed;

ALTER TABLE gateway_mock_events
    ADD CONSTRAINT gateway_mock_events_content_audit_state_allowed CHECK (
        content_audit_state IN ('unknown', 'skipped_local_mock')
    );

COMMENT ON COLUMN gateway_mock_events.content_audit_state IS '命中时内容审计是否执行的有界状态：unknown=旧记录/未记录，skipped_local_mock=早期严格 Mock 命中未审计。';
