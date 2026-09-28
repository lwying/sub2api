//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestMigration257BackfillsOnlyEnabledAnthropicOAuthMissingOrMalformedSeeds(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	migrationSQL, err := dbmigrations.FS.ReadFile("257_backfill_claude_fingerprint_seed.sql")
	require.NoError(t, err)

	var oauthMissingID, setupTokenMissingID, blankID, malformedID, nilUUIDID, validID, offID, apiKeyID, openAIID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type, extra)
VALUES ('migration-257-oauth-missing', 'anthropic', 'oauth', '{"claude_fingerprint_mode":"session"}'::jsonb)
RETURNING id
`).Scan(&oauthMissingID))
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type, extra)
VALUES ('migration-257-setup-token-missing', 'anthropic', 'setup-token', '{"claude_fingerprint_mode":"full"}'::jsonb)
RETURNING id
`).Scan(&setupTokenMissingID))
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type, extra)
VALUES ('migration-257-blank', 'anthropic', 'oauth', '{"claude_fingerprint_mode":"device","claude_fingerprint_seed":""}'::jsonb)
RETURNING id
`).Scan(&blankID))
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type, extra)
VALUES ('migration-257-malformed', 'anthropic', 'oauth', '{"claude_fingerprint_mode":"full","claude_fingerprint_seed":"BAD"}'::jsonb)
RETURNING id
`).Scan(&malformedID))
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type, extra)
VALUES ('migration-257-nil-uuid', 'anthropic', 'oauth', '{"claude_fingerprint_mode":"session","claude_fingerprint_seed":"00000000-0000-0000-0000-000000000000"}'::jsonb)
RETURNING id
`).Scan(&nilUUIDID))
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type, extra)
VALUES ('migration-257-valid', 'anthropic', 'oauth', '{"claude_fingerprint_mode":"session","claude_fingerprint_seed":"11111111-1111-4111-8111-111111111111"}'::jsonb)
RETURNING id
`).Scan(&validID))
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type, extra)
VALUES ('migration-257-off', 'anthropic', 'oauth', '{"claude_fingerprint_mode":"off"}'::jsonb)
RETURNING id
`).Scan(&offID))
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type, extra)
VALUES ('migration-257-apikey', 'anthropic', 'apikey', '{"claude_fingerprint_mode":"session"}'::jsonb)
RETURNING id
`).Scan(&apiKeyID))
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type, extra)
VALUES ('migration-257-openai', 'openai', 'oauth', '{"claude_fingerprint_mode":"session"}'::jsonb)
RETURNING id
`).Scan(&openAIID))

	_, err = tx.ExecContext(ctx, string(migrationSQL))
	require.NoError(t, err)

	seedsAfterFirst := map[int64]string{}
	for _, id := range []int64{oauthMissingID, setupTokenMissingID, blankID, malformedID, nilUUIDID, validID} {
		var seed string
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT extra->>'claude_fingerprint_seed' FROM accounts WHERE id = $1`, id).Scan(&seed))
		requireCanonicalUUIDString(t, seed)
		seedsAfterFirst[id] = seed
	}
	require.Equal(t, "11111111-1111-4111-8111-111111111111", seedsAfterFirst[validID], "合法 seed 必须原样保留")
	require.NotEqual(t, seedsAfterFirst[oauthMissingID], seedsAfterFirst[setupTokenMissingID], "每行独立铸造")

	for _, id := range []int64{offID, apiKeyID, openAIID} {
		var hasSeed bool
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT extra ? 'claude_fingerprint_seed' FROM accounts WHERE id = $1`, id).Scan(&hasSeed))
		require.False(t, hasSeed, "未开启收敛 / 非 Anthropic OAuth 的行不得拿到 seed")
	}

	// 幂等：重跑不得轮换已存在的合法 seed
	_, err = tx.ExecContext(ctx, string(migrationSQL))
	require.NoError(t, err)

	for id, want := range seedsAfterFirst {
		var got string
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT extra->>'claude_fingerprint_seed' FROM accounts WHERE id = $1`, id).Scan(&got))
		require.Equal(t, want, got)
	}
}

func TestBulkUpdateGeneratesDistinctStableClaudeFingerprintSeedsPerEligibleRow(t *testing.T) {
	ctx := context.Background()
	testName := "bulk-claude-seed-" + uuid.NewString()
	type fixture struct {
		name        string
		accountType string
		extra       string
	}
	fixtures := []fixture{
		{name: testName + "-oauth-missing", accountType: service.AccountTypeOAuth, extra: `{}`},
		{name: testName + "-setup-token-malformed", accountType: service.AccountTypeSetupToken, extra: `{"claude_fingerprint_seed":"BAD"}`},
		{name: testName + "-oauth-valid", accountType: service.AccountTypeOAuth, extra: `{"claude_fingerprint_seed":"11111111-1111-4111-8111-111111111111"}`},
		{name: testName + "-apikey", accountType: service.AccountTypeAPIKey, extra: `{}`},
	}

	ids := make([]int64, 0, len(fixtures))
	for _, f := range fixtures {
		var id int64
		require.NoError(t, integrationDB.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type, extra)
VALUES ($1, 'anthropic', $2, $3::jsonb)
RETURNING id
`, f.name, f.accountType, f.extra).Scan(&id))
		ids = append(ids, id)
	}
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM scheduler_outbox WHERE account_id = ANY($1)`, pq.Array(ids))
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id = ANY($1)`, pq.Array(ids))
	})

	repo := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	updates := service.AccountBulkUpdate{
		Extra: map[string]any{
			"claude_fingerprint_mode": "session",
		},
		EnsureClaudeFingerprintSeed: true,
	}
	rows, err := repo.BulkUpdate(ctx, ids, updates)
	require.NoError(t, err)
	require.Equal(t, int64(len(ids)), rows)

	readSeed := func(id int64) string {
		t.Helper()
		var seed string
		require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COALESCE(extra->>'claude_fingerprint_seed', '') FROM accounts WHERE id = $1`, id).Scan(&seed))
		return seed
	}
	firstSeeds := []string{readSeed(ids[0]), readSeed(ids[1]), readSeed(ids[2]), readSeed(ids[3])}
	requireCanonicalUUIDString(t, firstSeeds[0])
	requireCanonicalUUIDString(t, firstSeeds[1])
	require.NotEqual(t, firstSeeds[0], firstSeeds[1], "gen_random_uuid must be evaluated per eligible row")
	require.Equal(t, "11111111-1111-4111-8111-111111111111", firstSeeds[2])
	require.Empty(t, firstSeeds[3], "API-key accounts must not receive a Claude fingerprint seed")

	rows, err = repo.BulkUpdate(ctx, ids, updates)
	require.NoError(t, err)
	require.Equal(t, int64(len(ids)), rows)
	for i, want := range firstSeeds {
		require.Equal(t, want, readSeed(ids[i]), "retry must not rotate an existing valid seed")
	}
}
