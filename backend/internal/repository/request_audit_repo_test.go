//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRequestAuditRepositoryRoundTripsProtocolFields(t *testing.T) {
	ctx := context.Background()
	client := newRequestAuditReservationTestClient(t)
	usage, err := client.UsageLog.Create().
		SetUserID(1).
		SetAPIKeyID(2).
		SetAccountID(3).
		SetRequestID("request-audit-protocol-fields").
		SetModel("gpt-5.1").
		Save(ctx)
	require.NoError(t, err)

	stream := false
	rec := &service.RequestAuditRecord{
		UsageLogID: usage.ID,
		Metadata: service.RequestAuditMetadata{
			ProtocolFields: &service.RequestAuditProtocolFields{
				Stream:           &stream,
				ThinkingType:     "adaptive",
				PresentFields:    []string{"model", "messages", "tools", "thinking", "tool_name:raw-tool-name-canary", "unknown-field-canary"},
				NormalizedFields: []string{"thinking.extra_fields_removed", "thinking.enum-canary"},
			},
		},
	}

	repo := NewRequestAuditRepository(client)
	require.NoError(t, repo.CreateRequestAudit(ctx, rec))
	stored, err := repo.GetByUsageLogID(ctx, usage.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.Metadata.ProtocolFields)
	require.NotNil(t, stored.Metadata.ProtocolFields.Stream)
	require.False(t, *stored.Metadata.ProtocolFields.Stream)
	require.Equal(t, "adaptive", stored.Metadata.ProtocolFields.ThinkingType)
	require.Equal(t, []string{"model", "messages", "tools", "thinking"}, stored.Metadata.ProtocolFields.PresentFields)
	require.Equal(t, []string{"thinking.extra_fields_removed"}, stored.Metadata.ProtocolFields.NormalizedFields)

	encoded, err := json.Marshal(stored)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"protocol_fields"`)
	require.Contains(t, string(encoded), `"thinking_type":"adaptive"`)
	require.NotContains(t, string(encoded), "raw-tool-name-canary")
	require.NotContains(t, string(encoded), "unknown-field-canary")
	require.NotContains(t, string(encoded), "enum-canary")
}

func TestRequestAuditRepositoryRoundTripsAllowlistedTokenCounts(t *testing.T) {
	ctx := context.Background()
	client := newRequestAuditReservationTestClient(t)
	usage, err := client.UsageLog.Create().
		SetUserID(1).
		SetAPIKeyID(2).
		SetAccountID(3).
		SetRequestID("request-audit-token-counts").
		SetModel("gpt-5.1").
		Save(ctx)
	require.NoError(t, err)

	rec := &service.RequestAuditRecord{
		UsageLogID: usage.ID,
		Metadata: service.RequestAuditMetadata{
			Status: map[string]int{"upstream": 200},
			Tokens: map[string]int{
				"input_tokens":  123,
				"output_tokens": 45,
				"status":        200,
				"negative":      -1,
			},
		},
	}

	repo := NewRequestAuditRepository(client)
	require.NoError(t, repo.CreateRequestAudit(ctx, rec))
	stored, err := repo.GetByUsageLogID(ctx, usage.ID)
	require.NoError(t, err)
	require.Equal(t, map[string]int{"input_tokens": 123, "output_tokens": 45}, stored.Metadata.Tokens)
	require.Empty(t, stored.Metadata.Status)
}

func TestRequestAuditRepositorySanitizesHistoricalJSONOnRead(t *testing.T) {
	ctx := context.Background()
	client := newRequestAuditReservationTestClient(t)
	usage, err := client.UsageLog.Create().
		SetUserID(1).
		SetAPIKeyID(2).
		SetAccountID(3).
		SetRequestID("request-audit-historical").
		SetModel("gpt-5.1").
		Save(ctx)
	require.NoError(t, err)

	const secret = "sk-historical-secret"
	_, err = client.RequestAudit.Create().
		SetUsageLogID(usage.ID).
		SetHeaders(map[string]any{
			"X-Stainless-Lang": "js",
			"Authorization":    "Bearer " + secret,
			"Cookie":           "session=" + secret,
			"X-Untrusted":      secret,
		}).
		SetEvents([]map[string]any{
			{"type": "message_start", "index": 0, "bytes": 12},
			{"type": "secret-event-" + secret, "index": 1, "bytes": 15, "data": secret},
		}).
		SetAttempts([]map[string]any{{
			"account_id": 3,
			"model":      secret,
			"protocol":   service.RequestAuditProtocolAnthropic,
			"stage":      service.RequestAuditStageWire,
		}}).
		SetCaptureCompleteness(service.RequestAuditCaptureComplete).
		SetCaptureReason("secret-reason-" + secret).
		SetMetadata(map[string]any{
			"routes": map[string]any{
				"inbound":  "/v1/messages",
				"upstream": "/v1/messages?api_key=" + secret,
				"internal": secret,
			},
			"ids":    map[string]any{"session": secret, "response": "resp-" + secret},
			"status": map[string]any{"upstream": 200, "secret": 201},
			"bytes":  map[string]any{"upstream": 42, "secret": 99},
		}).
		Save(ctx)
	require.NoError(t, err)

	rec, err := NewRequestAuditRepository(client).GetByUsageLogID(ctx, usage.ID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, "js", rec.Headers["X-Stainless-Lang"])
	require.NotContains(t, rec.Headers, "X-Untrusted")
	require.Len(t, rec.Events, 2)
	require.Equal(t, "message_start", rec.Events[0].Type)
	require.Equal(t, "unknown", rec.Events[1].Type)
	require.Equal(t, 1, rec.Events[1].Index)
	require.Equal(t, 15, rec.Events[1].Bytes)
	require.Len(t, rec.Attempts, 1)
	require.Empty(t, rec.Attempts[0].Model)
	require.Equal(t, service.RequestAuditProtocolAnthropic, rec.Attempts[0].Protocol)
	require.Equal(t, map[string]string{"inbound": "/v1/messages"}, rec.Metadata.Routes)
	require.Empty(t, rec.Metadata.IDs)
	require.Empty(t, rec.Metadata.Status)
	require.Empty(t, rec.Metadata.Bytes)

	encoded, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), secret)
	require.NotContains(t, string(encoded), "api_key")
}

func TestRequestAuditRepositorySanitizesLinkedReservationFallback(t *testing.T) {
	ctx := context.Background()
	client := newRequestAuditReservationTestClient(t)
	usage, err := client.UsageLog.Create().
		SetUserID(1).
		SetAPIKeyID(2).
		SetAccountID(3).
		SetRequestID("request-audit-linked-reservation").
		SetModel("gpt-5.1").
		Save(ctx)
	require.NoError(t, err)

	const secret = "sk-linked-reservation-secret"
	_, err = client.RequestAuditReservation.Create().
		SetLogicalKey("linked-reservation-secret").
		SetRouteFamily(string(service.RequestAuditRouteMessages)).
		SetHeaders(map[string]any{
			"X-Stainless-Lang": "go",
			"Authorization":    map[string]any{"present": true},
			"X-Untrusted":      secret,
		}).
		SetAttempts([]map[string]any{{
			"account_id": 3,
			"model":      secret,
			"protocol":   service.RequestAuditProtocolAnthropic,
			"stage":      service.RequestAuditStageWire,
		}}).
		SetUsageLogID(usage.ID).
		SetCaptureCompleteness(service.RequestAuditCaptureIncomplete).
		SetCaptureReason("secret-reason-" + secret).
		SetExpiresAt(time.Now().Add(time.Hour)).
		Save(ctx)
	require.NoError(t, err)

	rec, err := NewRequestAuditRepository(client).GetByUsageLogID(ctx, usage.ID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, "go", rec.Headers["X-Stainless-Lang"])
	require.NotContains(t, rec.Headers, "X-Untrusted")
	require.Equal(t, map[string]any{"present": true}, rec.Headers["Authorization"])
	require.Len(t, rec.Attempts, 1)
	require.Empty(t, rec.Attempts[0].Model)
	require.Equal(t, int64(3), rec.Attempts[0].AccountID)
	require.Equal(t, service.RequestAuditProtocolAnthropic, rec.Attempts[0].Protocol)
	require.Equal(t, service.RequestAuditCaptureIncomplete, rec.CaptureCompleteness)
	require.Empty(t, rec.CaptureReason)

	encoded, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), secret)
}

func TestSanitizeRequestAuditRecordReturnsDeepCopy(t *testing.T) {
	input := &service.RequestAuditRecord{
		UsageLogID: 4,
		Headers:    map[string]any{"X-Stainless-Lang": "js", "Authorization": map[string]any{"present": true}},
		Events:     []service.RequestAuditEventSkeleton{{Type: "message_start", Index: 0, Bytes: 12}},
		Attempts: []service.RequestAuditAttempt{{
			AccountID: 9,
			Model:     "gpt-5.1",
			Protocol:  service.RequestAuditProtocolAnthropic,
			Stage:     service.RequestAuditStageWire,
		}},
		CaptureCompleteness: service.RequestAuditCaptureComplete,
		Metadata: service.RequestAuditMetadata{
			Routes: map[string]string{"inbound": "/v1/messages"},
			IDs:    map[string]string{"session": "discarded"},
		},
	}

	sanitized := service.SanitizeRequestAuditRecord(input)
	require.NotNil(t, sanitized)
	require.NotSame(t, input, sanitized)
	require.Equal(t, map[string]string{"inbound": "/v1/messages"}, sanitized.Metadata.Routes)
	require.Empty(t, sanitized.Metadata.IDs)
	require.Empty(t, sanitized.Attempts[0].Model)

	input.Headers["X-Stainless-Lang"] = "changed"
	input.Events[0].Type = "response.failed"
	input.Attempts[0].Protocol = service.RequestAuditProtocolOpenAIChat
	input.Metadata.Routes["inbound"] = "/v1/responses"

	require.Equal(t, "js", sanitized.Headers["X-Stainless-Lang"])
	require.Equal(t, "message_start", sanitized.Events[0].Type)
	require.Equal(t, service.RequestAuditProtocolAnthropic, sanitized.Attempts[0].Protocol)
	require.Equal(t, map[string]string{"inbound": "/v1/messages"}, sanitized.Metadata.Routes)
}

func TestRequestAuditRepositorySanitizesBeforeWriteWithoutMutatingInput(t *testing.T) {
	ctx := context.Background()
	client := newRequestAuditReservationTestClient(t)
	usage, err := client.UsageLog.Create().
		SetUserID(1).
		SetAPIKeyID(2).
		SetAccountID(3).
		SetRequestID("request-audit-write-boundary").
		SetModel("gpt-5.1").
		Save(ctx)
	require.NoError(t, err)

	const secret = "sk-write-boundary-secret"
	input := &service.RequestAuditRecord{
		UsageLogID: usage.ID,
		Headers: map[string]any{
			"X-Stainless-Lang": "js",
			"Authorization":    map[string]any{"token": secret},
			"X-Untrusted":      secret,
		},
		Events: []service.RequestAuditEventSkeleton{
			{Type: "message_start", Index: 0, Bytes: 12},
			{Type: "untrusted-" + secret, Index: 1, Bytes: 20},
		},
		Attempts: []service.RequestAuditAttempt{{
			AccountID: 3,
			Model:     secret,
			Protocol:  service.RequestAuditProtocolAnthropic,
			Stage:     service.RequestAuditStageWire,
		}},
		CaptureCompleteness:   service.RequestAuditCaptureComplete,
		CaptureReason:         "untrusted-" + secret,
		FingerprintSalt:       []byte{0x01, 0x02, 0x03, 0x04},
		RequestFingerprint:    strings.Repeat("a", 64),
		FingerprintKeyVersion: service.RequestAuditFingerprintKeyVersion,
		Metadata: service.RequestAuditMetadata{
			Routes: map[string]string{"inbound": "/v1/messages", "upstream": "/v1/messages?token=" + secret},
			IDs:    map[string]string{"session": secret},
		},
	}

	repo := NewRequestAuditRepository(client)
	require.NotPanics(t, func() {
		err = repo.CreateRequestAudit(ctx, input)
	})
	require.NoError(t, err)
	// The repository's defense-in-depth sanitizer must work on a copy.
	require.Equal(t, secret, input.Headers["X-Untrusted"])
	require.Equal(t, secret, input.Attempts[0].Model)
	require.Equal(t, secret, input.Metadata.IDs["session"])

	stored, err := repo.GetByUsageLogID(ctx, usage.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, "js", stored.Headers["X-Stainless-Lang"])
	require.NotContains(t, stored.Headers, "X-Untrusted")
	require.Equal(t, []byte{0x01, 0x02, 0x03, 0x04}, stored.FingerprintSalt)
	require.Len(t, stored.Events, 2)
	require.Equal(t, "message_start", stored.Events[0].Type)
	require.Equal(t, "unknown", stored.Events[1].Type)
	require.Equal(t, 1, stored.Events[1].Index)
	require.Equal(t, 20, stored.Events[1].Bytes)
	require.Empty(t, stored.Attempts[0].Model)
	require.Equal(t, map[string]string{"inbound": "/v1/messages"}, stored.Metadata.Routes)
	require.Empty(t, stored.Metadata.IDs)

	encoded, err := json.Marshal(stored)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), secret)
	require.NotContains(t, string(encoded), "FingerprintSalt")
	require.NotContains(t, string(encoded), "AQIDBA==")
}
