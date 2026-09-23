package repository

import (
	"context"
	"encoding/json"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/requestaudit"
	"github.com/Wei-Shaw/sub2api/ent/requestauditreservation"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type requestAuditRepository struct {
	client *ent.Client
}

// NewRequestAuditRepository persists request audit rows 1:1 with usage_logs.
func NewRequestAuditRepository(client *ent.Client) service.RequestAuditRepository {
	return &requestAuditRepository{client: client}
}

func (r *requestAuditRepository) CreateRequestAudit(ctx context.Context, rec *service.RequestAuditRecord) error {
	if r == nil || r.client == nil || rec == nil {
		return nil
	}
	rec = service.SanitizeRequestAuditRecord(rec)
	if rec == nil {
		return nil
	}
	headers := rec.Headers
	if headers == nil {
		headers = map[string]any{}
	}
	_, err := r.client.RequestAudit.Create().
		SetUsageLogID(rec.UsageLogID).
		SetHeaders(headers).
		SetEvents(requestAuditEventsToMaps(rec.Events)).
		SetAttempts(requestAuditAttemptsToMaps(rec.Attempts)).
		SetCaptureCompleteness(requestAuditCaptureCompletenessOrDefault(rec.CaptureCompleteness)).
		SetCaptureReason(rec.CaptureReason).
		SetRequestFingerprint(rec.RequestFingerprint).
		SetFingerprintKeyVersion(rec.FingerprintKeyVersion).
		SetFingerprintSalt(rec.FingerprintSalt).
		SetMetadata(requestAuditMetadataToMap(rec.Metadata)).
		Save(ctx)
	return err
}

func (r *requestAuditRepository) GetByUsageLogID(ctx context.Context, usageLogID int64) (*service.RequestAuditRecord, error) {
	if r == nil || r.client == nil || usageLogID <= 0 {
		return nil, nil
	}
	row, err := r.client.RequestAudit.Query().
		Where(requestaudit.UsageLogID(usageLogID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			reservation, reservationErr := r.client.RequestAuditReservation.Query().
				Where(requestauditreservation.UsageLogIDEQ(usageLogID)).
				Only(ctx)
			if ent.IsNotFound(reservationErr) {
				return nil, nil
			}
			if reservationErr != nil {
				return nil, reservationErr
			}
			return service.SanitizeRequestAuditRecord(&service.RequestAuditRecord{
				UsageLogID:          usageLogID,
				Headers:             reservation.Headers,
				Attempts:            requestAuditMapsToAttempts(reservation.Attempts),
				CaptureCompleteness: reservation.CaptureCompleteness,
				CaptureReason:       reservation.CaptureReason,
			}), nil
		}
		return nil, err
	}
	return service.SanitizeRequestAuditRecord(&service.RequestAuditRecord{
		UsageLogID:            row.UsageLogID,
		Headers:               row.Headers,
		Events:                requestAuditMapsToEvents(row.Events),
		Attempts:              requestAuditMapsToAttempts(row.Attempts),
		CaptureCompleteness:   row.CaptureCompleteness,
		CaptureReason:         row.CaptureReason,
		RequestFingerprint:    optionalRequestFingerprint(row.RequestFingerprint),
		FingerprintKeyVersion: row.FingerprintKeyVersion,
		FingerprintSalt:       requestAuditFingerprintSalt(row.FingerprintSalt),
		Metadata:              requestAuditMetadataFromMap(row.Metadata),
	}), nil
}

func requestAuditMetadataToMap(v service.RequestAuditMetadata) map[string]any {
	sanitized := service.SanitizeRequestAuditMetadata(v)
	out := map[string]any{"routes": sanitized.Routes, "ids": sanitized.IDs, "status": sanitized.Status, "bytes": sanitized.Bytes, "tokens": sanitized.Tokens}
	if fields := sanitized.ProtocolFields; fields != nil {
		out["protocol_fields"] = fields
	}
	return out
}

func requestAuditMetadataFromMap(raw map[string]any) service.RequestAuditMetadata {
	metadata := service.RequestAuditMetadata{
		Routes: mapStringString(raw["routes"]),
		IDs:    mapStringString(raw["ids"]),
		Status: mapStringInt(raw["status"]),
		Bytes:  mapStringInt64(raw["bytes"]),
		Tokens: mapStringInt(raw["tokens"]),
	}
	if fields, ok := raw["protocol_fields"].(map[string]any); ok {
		var stream *bool
		if value, exists := fields["stream"]; exists {
			if streamValue, valid := value.(bool); valid {
				stream = &streamValue
			}
		}
		thinkingType, _ := fields["thinking_type"].(string)
		metadata.ProtocolFields = service.SanitizeRequestAuditProtocolFields(service.RequestAuditProtocolFields{
			Stream:           stream,
			ThinkingType:     thinkingType,
			PresentFields:    mapStringSlice(fields["present_fields"]),
			NormalizedFields: mapStringSlice(fields["normalized_fields"]),
		})
	}
	return service.SanitizeRequestAuditMetadata(metadata)
}

func mapStringSlice(v any) []string {
	out := []string{}
	switch values := v.(type) {
	case []any:
		for _, value := range values {
			if text, ok := value.(string); ok {
				out = append(out, text)
			}
		}
	case []string:
		out = append(out, values...)
	}
	return out
}

func mapStringString(v any) map[string]string {
	out := map[string]string{}
	m, _ := v.(map[string]any)
	for k, x := range m {
		if s, ok := x.(string); ok {
			out[k] = s
		}
	}
	return out
}
func mapStringInt(v any) map[string]int {
	out := map[string]int{}
	m, _ := v.(map[string]any)
	for k, x := range m {
		if f, ok := x.(float64); ok {
			out[k] = int(f)
		}
	}
	return out
}
func mapStringInt64(v any) map[string]int64 {
	out := map[string]int64{}
	m, _ := v.(map[string]any)
	for k, x := range m {
		if f, ok := x.(float64); ok {
			out[k] = int64(f)
		}
	}
	return out
}

func optionalRequestFingerprint(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func requestAuditFingerprintSalt(v *[]byte) []byte {
	if v == nil {
		return nil
	}
	return append([]byte(nil), (*v)...)
}

func requestAuditCaptureCompletenessOrDefault(v string) string {
	if v == "" {
		return service.RequestAuditCaptureComplete
	}
	return v
}

func requestAuditEventsToMaps(events []service.RequestAuditEventSkeleton) []map[string]any {
	if len(events) == 0 {
		return []map[string]any{}
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		return []map[string]any{}
	}
	var out []map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil || out == nil {
		return []map[string]any{}
	}
	return out
}

func requestAuditMapsToEvents(raw []map[string]any) []service.RequestAuditEventSkeleton {
	if len(raw) == 0 {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []service.RequestAuditEventSkeleton
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil
	}
	return out
}

func requestAuditAttemptsToMaps(attempts []service.RequestAuditAttempt) []map[string]any {
	if len(attempts) == 0 {
		return []map[string]any{}
	}
	encoded, err := json.Marshal(attempts)
	if err != nil {
		return []map[string]any{}
	}
	var out []map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil || out == nil {
		return []map[string]any{}
	}
	return out
}

func requestAuditMapsToAttempts(raw []map[string]any) []service.RequestAuditAttempt {
	if len(raw) == 0 {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []service.RequestAuditAttempt
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil
	}
	return out
}
