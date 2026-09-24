//go:build unit

package repository

import (
	"context"
	"regexp"
	"testing"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestKeyBillingSnapshotSettingsAtomicSeedsThenLocksMissingRow(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()
	client := ent.NewClient(ent.Driver(entsql.OpenDB("postgres", db)))
	defer client.Close()
	repo := &settingRepository{client: client}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, $3)\n\t\tON CONFLICT (key) DO NOTHING")).
		WithArgs(service.SettingKeyKeyBillingSnapshot, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM settings WHERE key = $1 FOR UPDATE")).
		WithArgs(service.SettingKeyKeyBillingSnapshot).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(`{"enabled":false,"max_stale_hours":72,"generation":"disabled"}`))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, $3)\n\t\tON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at")).
		WithArgs(service.SettingKeyKeyBillingSnapshot, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = repo.SetKeyBillingSnapshotSettingsAtomic(context.Background(), true, 72)

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

var _ interface {
	SetKeyBillingSnapshotSettingsAtomic(context.Context, bool, int) error
} = (*settingRepository)(nil)
