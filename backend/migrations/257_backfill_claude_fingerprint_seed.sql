-- Backfill system-managed Claude fingerprint seeds for enabled Anthropic OAuth accounts.
-- Idempotent: valid canonical seeds are preserved on rerun.
--
-- Mirrors 225_backfill_codex_fingerprint_seed.sql. Only accounts that already had a
-- convergence mode set are touched, so this never enables convergence for a legacy
-- account (explicit opt-in stays explicit). Setup-token accounts are included because
-- Claude fingerprint convergence covers both OAuth and SetupToken credentials.
UPDATE accounts
SET extra = jsonb_set(
    COALESCE(extra, '{}'::jsonb),
    '{claude_fingerprint_seed}',
    to_jsonb(gen_random_uuid()::text),
    true
)
WHERE deleted_at IS NULL
  AND platform = 'anthropic'
  AND type IN ('oauth', 'setup-token')
  AND COALESCE(extra->>'claude_fingerprint_mode', '') IN ('device', 'session', 'full')
  AND (
      extra->>'claude_fingerprint_seed' IS NULL
      OR btrim(extra->>'claude_fingerprint_seed') = ''
      OR NOT (
          extra->>'claude_fingerprint_seed' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
          AND extra->>'claude_fingerprint_seed' <> '00000000-0000-0000-0000-000000000000'
      )
  );
