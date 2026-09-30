-- 请求 Trace 信封上的"实际选中平台历史"。
--
-- 采集范围与页面检索都按**任一实际选中的上游账号平台**判定（规格 §2.2/§2.3）。
-- 账号已经选中、却在上游发出之前就失败（例如账号并发槽位等待超时）时，平台同样是
-- 一个已知事实。此前只有 wire_attempt 阶段带着平台事实，于是这类"选中但未发出"的
-- 错误 Trace 在列表/导出里既匹配不上任何具体平台，又被当成"平台未知"。
--
-- 这一列把本次逻辑请求**实际选中过的全部平台**挂在信封上（去重、按首次观察顺序、
-- 有界），使它可按任一平台检索，并且不被算作"未知"。
--
-- NULL 表示**从未选到任何账号**（平台未知）。非 NULL 时必须是**非空**的 JSON 数组，
-- 元素为合法的请求时 token，且**每个元素都合法、互不重复**；空数组不是合法取值——
-- 未知只能用 NULL 表达，不能用空数组冒充"已观察但为空"。
--
-- 形状护栏放在一个 IMMUTABLE 校验函数里，而不是直接写进 CHECK：数组"每个元素都
-- 合法"与"元素互不重复"用单条 CHECK 表达式无法完整表达（`@?` 只证明存在某个匹配
-- 元素，漏掉混入的非法元素；数组去重需要子查询/集合函数，CHECK 不允许）。函数用
-- CREATE OR REPLACE，灾备重放安全（migrations/README.md）。
--
-- 不回填历史行：既有 Trace 的平台事实仍由 wire_attempt 阶段承载，查询侧对新旧两处取
-- 并集（见 request_trace_repo.go 与 request_trace_export_source.go 的过滤语义）。
-- 按仓库约定（migrations/README.md），采集是每条请求都要写的高写入路径，本列只加列、
-- 不加索引：平台筛选本就没有索引，与 265 的 group_id / requested_model 一致。
ALTER TABLE request_traces
    ADD COLUMN IF NOT EXISTS observed_platforms JSONB;

COMMENT ON COLUMN request_traces.observed_platforms IS
    '本次逻辑请求实际选中过的上游账号平台（去重、按首次观察顺序、有界）；NULL 表示从未选到账号，空数组非法。';

-- 校验"实际选中平台历史"这一列的全部形状约束：
--   * SQL NULL 合法：平台未知；
--   * 必须是非空 JSON 数组，1..16 个元素；
--   * 每个元素都必须是请求时 token（与 Go 侧 requestTraceFactToken 同一条形状规则）；
--   * 元素之间不得重复（写入侧归一化也去重，这里是数据库侧的第二道防线）；
--   * 整个数组的字节数有界。
CREATE OR REPLACE FUNCTION public.request_trace_observed_platforms_valid(platforms JSONB)
RETURNS boolean
LANGUAGE sql
IMMUTABLE
AS $$
    SELECT CASE
        WHEN platforms IS NULL THEN TRUE
        WHEN jsonb_typeof(platforms) <> 'array' THEN FALSE
        WHEN jsonb_array_length(platforms) NOT BETWEEN 1 AND 16 THEN FALSE
        -- 类型先判：jsonb_array_elements_text 会把数字/布尔转成文本，
        -- 单靠正则无法区分 1 与 "1"。
        WHEN EXISTS (
            SELECT 1
            FROM jsonb_array_elements(platforms) AS element
            WHERE jsonb_typeof(element) <> 'string'
        ) THEN FALSE
        WHEN EXISTS (
            SELECT 1
            FROM jsonb_array_elements_text(platforms) AS element
            WHERE element !~ '^[A-Za-z0-9][A-Za-z0-9_./:+-]{0,127}$'
        ) THEN FALSE
        WHEN (SELECT count(*) FROM jsonb_array_elements_text(platforms))
             <> (SELECT count(DISTINCT element) FROM jsonb_array_elements_text(platforms) AS element) THEN FALSE
        WHEN octet_length(platforms::text) > 2048 THEN FALSE
        ELSE TRUE
    END
$$;

-- 先按名 DROP 再 ADD，灾备重放不会因"约束已存在"失败（migrations/README.md）。
ALTER TABLE request_traces
    DROP CONSTRAINT IF EXISTS request_traces_observed_platforms_shape_allowed;

ALTER TABLE request_traces
    ADD CONSTRAINT request_traces_observed_platforms_shape_allowed
    CHECK (public.request_trace_observed_platforms_valid(observed_platforms));
