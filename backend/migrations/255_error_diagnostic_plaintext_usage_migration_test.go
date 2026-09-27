package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestErrorDiagnosticPlaintextUsageMigration(t *testing.T) {
	migration, err := FS.ReadFile("255_error_diagnostic_plaintext_usage.sql")
	require.NoError(t, err)
	text := string(migration)
	for _, required := range []string{
		"plain_owner_usage_log_id", "ON DELETE CASCADE", "plain_link_digest",
		"plain_body_payload BYTEA", "plain_header_payload BYTEA", "metadata_expires_at",
		"plain_body_state", "plain_header_state", "plain_body_retained",
		"plain_header_retained", "CHECK", "IF NOT EXISTS",
	} {
		require.Contains(t, text, required)
	}
	for _, forbidden := range []string{"ALTER TABLE request_audits", "DROP TABLE error_diagnostic_records"} {
		require.NotContains(t, text, forbidden)
	}
}
