package migrations

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestAuditFingerprintMigrationAddsKeyedDigestMetadata(t *testing.T) {
	content, err := FS.ReadFile("246_request_audit_fingerprint.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS request_fingerprint TEXT")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS fingerprint_key_version INTEGER NOT NULL DEFAULT 0")
	require.NotContains(t, strings.ToLower(sql), "request_body")
}

func TestRequestAuditFingerprintSaltMigrationAddsInternalPerRecordSalt(t *testing.T) {
	content, err := FS.ReadFile("248_request_audit_fingerprint_salt.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS fingerprint_salt BYTEA")
	require.NotContains(t, strings.ToUpper(sql), "NOT NULL", "legacy rows must be allowed to have no salt")
	require.Contains(t, strings.ToLower(sql), "per-record")
	require.NotContains(t, strings.ToLower(sql), "master key")
	require.NotContains(t, strings.ToLower(sql), "request_body")
}

func TestRequestAuditEntFingerprintSaltIsInternalNullableBytes(t *testing.T) {
	content, err := os.ReadFile("../ent/schema/request_audit.go")
	require.NoError(t, err)
	schema := string(content)
	require.Contains(t, schema, `field.Bytes("fingerprint_salt").Optional().Nillable().StructTag(`+"`json:\"-\"`"+`).SchemaType(map[string]string{dialect.Postgres: "bytea"})`)

	generated, err := os.ReadFile("../ent/requestaudit.go")
	require.NoError(t, err)
	require.Contains(t, string(generated), "FingerprintSalt *[]byte `json:\"-\"`")
}
