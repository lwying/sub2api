//go:build integration

package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestErrorDiagnosticMixedFormatsHaveIndependentReadWindows(t *testing.T) {
	ctx := context.Background()
	cipher, err := NewErrorDiagnosticBodyCipher([]byte(strings.Repeat("k", 32)), 1)
	require.NoError(t, err)
	repo := NewErrorDiagnosticRepository(integrationDB, cipher)
	body := []byte(`{"model":"claude"}`)
	legacyBody, err := cipher.Encrypt(body)
	require.NoError(t, err)
	id, err := service.NewErrorDiagnosticID()
	require.NoError(t, err)
	now := time.Now().UTC()
	write := service.ErrorDiagnosticWrite{
		ID: id,
		Attempt: service.ErrorDiagnosticAttempt{
			Protocol: service.ErrorDiagnosticProtocolMessages, Stage: service.ErrorDiagnosticStageWire,
			AttemptIndex: 1, UpstreamStatusCode: 429, Body: body,
		},
		PlainRecord: true,
		BodyState:   service.ErrorDiagnosticBodyStateStored, BodyReason: service.ErrorDiagnosticBodyRetained,
		BodyCiphertext: legacyBody, BodyKeyVersion: 1,
		PlainHeaderState:      service.ErrorDiagnosticHeaderStateStored,
		PlainHeaderReason:     service.ErrorDiagnosticPlainHeaderRetained,
		PlainHeaderPayload:    []byte(`{"request":{"Content-Type":"application/json"},"response":{"Retry-After":"30"}}`),
		PlainHeaderEntryCount: 2,
	}
	_, err = repo.CreateErrorDiagnostic(ctx, write, now)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM error_diagnostic_records WHERE diagnostic_id = $1`, id)
	})
	record, err := repo.GetErrorDiagnostic(ctx, id)
	require.NoError(t, err)
	require.Equal(t, service.ErrorDiagnosticFormatEncrypted, record.BodyFormat())
	require.Equal(t, service.ErrorDiagnosticFormatPlaintext, record.HeaderFormat())
	gotBody, err := repo.ReadErrorDiagnosticBody(ctx, id, now)
	require.NoError(t, err)
	require.Equal(t, body, gotBody)
	gotHeaders, err := repo.(service.ErrorDiagnosticPlaintextReader).ReadErrorDiagnosticPlainHeaderValues(ctx, id, now)
	require.NoError(t, err)
	require.Equal(t, "30", gotHeaders.Response["Retry-After"])

	_, err = repo.ClearExpiredErrorDiagnosticBodies(ctx, now.Add(8*24*time.Hour), 10)
	require.NoError(t, err)
	_, err = repo.ReadErrorDiagnosticBody(ctx, id, now.Add(8*24*time.Hour))
	require.ErrorIs(t, err, service.ErrErrorDiagnosticBodyGone)
	gotHeaders, err = repo.(service.ErrorDiagnosticPlaintextReader).ReadErrorDiagnosticPlainHeaderValues(ctx, id, now.Add(8*24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, "30", gotHeaders.Response["Retry-After"])
}
