ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS thinking_disabled_strict BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN groups.thinking_disabled_strict IS
    'When thinking.type=disabled, extra keys are rejected with a local 400 instead of being stripped';
