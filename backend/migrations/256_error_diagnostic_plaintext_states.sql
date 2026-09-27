-- 新明文诊断的清理状态（票据 08／09）。
--
-- 迁移 255 给 error_diagnostic_records 加了明文列，但没有把「已物理清除」这一状态放进
-- 明文列的封闭集合：那里的 plain_body_state／plain_header_state 只允许
-- not_observed／stored／skipped，而清理必须把「曾留存、现已清除」记成 purged，
-- 才能与「从未留存」区分开（旧密文层就是这么记的）。
--
-- 本迁移只放宽这两个状态集合，让明文列与密文列的同名状态集合一致；它不新增列、不改语义、
-- 不碰任何已存行，也不触碰迁移 251／252／253／255 的其它约束与索引。
-- 原因码集合不变：purged 只是状态，原因码仍然保留 plain_body_retained／plain_header_retained，
-- 读取侧据此把「曾留存、已清除」与「从未留存」分开。
--
-- 锁与校验：两条约束用 NOT VALID 重建。这是一次**放宽**（新集合是 255 三值集合的超集），
-- 被 DROP 的旧约束已经保证所有已存行满足更窄的旧集合，因此已存行必然满足新集合，不需要
-- 全表校验。迁移运行器把整个文件放在一个事务里执行，DROP／ADD 的 ACCESS EXCLUSIVE 锁一直
-- 持有到提交；带上 NOT VALID 可让 ADD CONSTRAINT 只改目录、不扫描整表，把加锁窗口从
-- O(行数) 降为 O(1)，避免诊断写入/读取被长时间阻塞。NOT VALID 只豁免已存行，
-- PostgreSQL 仍对之后的 INSERT／UPDATE 强制执行这两条 CHECK。
--
-- 回滚：把两个约束改回 255 的三值集合需要先把已写成 purged 的行改回 skipped；本迁移不做
-- 自动回滚（清理写下的 purged 是历史事实）。

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'error_diagnostic_plain_body_allowed') THEN
        ALTER TABLE error_diagnostic_records DROP CONSTRAINT error_diagnostic_plain_body_allowed;
    END IF;
    ALTER TABLE error_diagnostic_records ADD CONSTRAINT error_diagnostic_plain_body_allowed CHECK (
        plain_body_state IN ('not_observed', 'stored', 'skipped', 'expired', 'purged')
        AND plain_body_reason IN ('not_observed', 'plain_body_retained',
            'skipped_not_text_json', 'skipped_too_large', 'skipped_attachment',
            'skipped_known_credential', 'skipped_incomplete_read', 'skipped_body_retention_disabled')
    ) NOT VALID;

    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'error_diagnostic_plain_header_allowed') THEN
        ALTER TABLE error_diagnostic_records DROP CONSTRAINT error_diagnostic_plain_header_allowed;
    END IF;
    ALTER TABLE error_diagnostic_records ADD CONSTRAINT error_diagnostic_plain_header_allowed CHECK (
        plain_header_state IN ('not_observed', 'stored', 'skipped', 'expired', 'purged')
        AND plain_header_reason IN ('not_observed', 'plain_header_retained',
            'skipped_header_retention_disabled', 'skipped_invalid_values')
    ) NOT VALID;
END
$$;
