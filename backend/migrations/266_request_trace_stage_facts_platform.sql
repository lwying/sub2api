-- wire_attempt 阶段新增 platform 事实键：本次尝试实际选中的上游账号平台。
--
-- 它是"请求时事实"，与账号后来更换平台无关，也不从 usage 或当前账号资料反推。
-- 采集范围据此判定整条逻辑请求的平台结论，页面与导出也按"任一次选中的平台"检索。
--
-- 262 已经按阶段分别固定了允许键集（transport 键只属于 client_metadata／wire_attempt，
-- 决策键只属于 gateway_decision）。这个迁移只在 **transport 那一支** 追加 platform，
-- 不合并两个键集、不放宽任何阶段的分流：
--
--   1. gateway_decision 仍然只接受决策键，且仍然必须是真正的决策对象；
--   2. client_metadata／wire_attempt 接受 transport 键 + platform；
--   3. 其它阶段仍然必须保持 '{}'；
--   4. 4096 字节的 canonical 上限（261）保持不变。
--
-- 261／262 未被修改：这是对同一个约束名的追加式替换，且先按名 DROP，
-- 因此灾备重放不会因"约束已存在"失败（migrations/README.md）。
ALTER TABLE request_trace_stages
    DROP CONSTRAINT IF EXISTS request_trace_stages_metadata_shape_allowed;

ALTER TABLE request_trace_stages
    ADD CONSTRAINT request_trace_stages_metadata_shape_allowed CHECK (
        CASE jsonb_typeof(metadata)
            WHEN 'object' THEN
                CASE
                    WHEN stage = 'gateway_decision' THEN
                        metadata <> '{}'::jsonb
                        AND (metadata - ARRAY[
                            'decision', 'outcome', 'source', 'sequence',
                            'model_from', 'model_to',
                            'protocol_from', 'protocol_to',
                            'account_id', 'decided_at'
                        ]) = '{}'::jsonb
                    WHEN stage IN ('client_metadata', 'wire_attempt') THEN
                        (metadata - ARRAY[
                            'method', 'url', 'url_omitted',
                            'request_headers', 'request_headers_omitted',
                            'response_headers', 'response_headers_omitted',
                            'account_id', 'model', 'platform', 'protocol', 'value_protocol',
                            'status', 'started_at', 'ended_at'
                        ]) = '{}'::jsonb
                    ELSE
                        metadata = '{}'::jsonb
                END
            ELSE FALSE
        END
    );

-- 阶段允许集与 262 一致：只有真正承载事实的阶段可以写非空对象。
ALTER TABLE request_trace_stages
    DROP CONSTRAINT IF EXISTS request_trace_stages_metadata_stage_allowed;
ALTER TABLE request_trace_stages
    ADD CONSTRAINT request_trace_stages_metadata_stage_allowed CHECK (
        metadata = '{}'::jsonb OR stage IN ('client_metadata', 'wire_attempt', 'gateway_decision')
    );
