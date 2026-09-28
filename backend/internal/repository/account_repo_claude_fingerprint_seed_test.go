package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

const claudeFingerprintTestSeedValue = "33333333-3333-4333-8333-333333333333"

func TestBulkUpdateEnsuresClaudeFingerprintSeedWithPerRowSQL(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	_, err := repo.BulkUpdate(context.Background(), []int64{27, 28}, service.AccountBulkUpdate{
		Extra: map[string]any{
			"claude_fingerprint_mode": "session",
			"claude_fingerprint_seed": claudeFingerprintTestSeedValue,
		},
		EnsureClaudeFingerprintSeed: true,
	})

	require.NoError(t, err)
	require.NotEmpty(t, exec.execQueries)
	query := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, query, "jsonb_set")
	require.Contains(t, query, "gen_random_uuid()::text")
	// Setup-token 与 oauth 都可开收敛，门控必须同时覆盖两者
	require.Contains(t, query, "platform = 'anthropic' AND type IN ('oauth', 'setup-token')")
	require.Contains(t, query, "to_jsonb(extra ->> 'claude_fingerprint_seed')")
	require.Contains(t, query, codexFingerprintSeedCanonicalPattern)
	require.NotContains(t, query, claudeFingerprintTestSeedValue)
	require.NotEmpty(t, exec.execArgs)
	payload, ok := exec.execArgs[0][0].([]byte)
	require.True(t, ok)
	require.Equal(t, `{"claude_fingerprint_mode":"session"}`, string(payload))
}

func TestBulkUpdateWithoutClaudeModeLeavesExtraExpressionUntouched(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	_, err := repo.BulkUpdate(context.Background(), []int64{27}, service.AccountBulkUpdate{
		Extra: map[string]any{"claude_fingerprint_mode": "off"},
	})

	require.NoError(t, err)
	require.NotEmpty(t, exec.execQueries)
	require.NotContains(t, normalizeSQLWhitespace(exec.execQueries[0]), "gen_random_uuid()::text",
		"off 模式不得铸造 seed")
}

func TestUpdateExtraEnsuresClaudeFingerprintSeedAtomicallyWhenEnabling(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = .*jsonb_set.*gen_random_uuid\(\)::text.*WHERE id = \$2 AND deleted_at IS NULL`).
		WithArgs(`{"claude_fingerprint_mode":"device"}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	repo := newAccountRepositoryWithSQL(client, db, nil)

	err = repo.UpdateExtra(context.Background(), 27, map[string]any{
		"claude_fingerprint_mode": "device",
		"claude_fingerprint_seed": claudeFingerprintTestSeedValue,
	})

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBulkUpdateClaudeFingerprintSeedRollsBackWhenUpdateFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = .*gen_random_uuid\(\)::text.*WHERE id = ANY\(\$2\)`).
		WithArgs(sqlmock.AnyArg(), `{27,28}`).
		WillReturnError(errors.New("update failed"))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	rows, err := repo.BulkUpdate(context.Background(), []int64{27, 28}, service.AccountBulkUpdate{
		Extra: map[string]any{
			"claude_fingerprint_mode": "session",
		},
		EnsureClaudeFingerprintSeed: true,
	})

	require.EqualError(t, err, "update failed")
	require.Zero(t, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}
