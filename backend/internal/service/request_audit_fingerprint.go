package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"unicode"
)

const RequestAuditFingerprintKeyVersion = 1

type RequestAuditFingerprinter interface {
	// Begin is retained for compatibility with legacy callers but fails closed because
	// request fingerprints must be scoped to an authenticated usage user.
	Begin() (*RequestAuditFingerprintInput, error)
	BeginForUser(authenticatedUserID int64) (*RequestAuditFingerprintInput, error)
}

type auditFingerprinter struct{ key []byte }

func NewRequestAuditFingerprinter(key string) (RequestAuditFingerprinter, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf("request audit fingerprint key must be at least 32 bytes")
	}
	return &auditFingerprinter{key: []byte(key)}, nil
}

// Begin never creates an unscoped fingerprint. Use BeginForUser with the authenticated
// usage user ID; this compatibility method fails closed to prevent cross-user correlation.
func (f *auditFingerprinter) Begin() (*RequestAuditFingerprintInput, error) {
	return nil, fmt.Errorf("request audit fingerprint requires an authenticated user ID")
}

// BeginForUser derives a user-specific key and creates a fresh per-logical-request salt.
func (f *auditFingerprinter) BeginForUser(authenticatedUserID int64) (*RequestAuditFingerprintInput, error) {
	if f == nil || len(f.key) == 0 {
		return nil, fmt.Errorf("request audit fingerprint key unavailable")
	}
	if authenticatedUserID <= 0 {
		return nil, fmt.Errorf("request audit fingerprint requires a positive authenticated user ID")
	}

	userID := make([]byte, 8)
	binary.BigEndian.PutUint64(userID, uint64(authenticatedUserID))
	userKey := hmac.New(sha256.New, f.key)
	userKey.Write(lengthPrefixed([]byte("request-audit/user-key/v1")))
	userKey.Write(lengthPrefixed(userID))

	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate request audit fingerprint salt: %w", err)
	}
	return &RequestAuditFingerprintInput{
		KeyVersion: RequestAuditFingerprintKeyVersion,
		record:     &requestAuditFingerprintRecord{key: userKey.Sum(nil), salt: salt},
	}, nil
}

// Salt returns a copy of the per-record salt for internal persistence.
// Callers must not expose it in admin or user-facing DTOs.
func (in *RequestAuditFingerprintInput) Salt() []byte {
	if in == nil || in.record == nil {
		return nil
	}
	return append([]byte(nil), in.record.salt...)
}

type requestAuditFingerprintRecord struct{ key, salt []byte }
type requestAuditFingerprintContextKey struct{}

func WithRequestAuditFingerprint(ctx context.Context, fp *RequestAuditFingerprintInput) context.Context {
	return context.WithValue(ctx, requestAuditFingerprintContextKey{}, fp)
}
func RequestAuditFingerprintFromContext(ctx context.Context) *RequestAuditFingerprintInput {
	if ctx == nil {
		return nil
	}
	fp, _ := ctx.Value(requestAuditFingerprintContextKey{}).(*RequestAuditFingerprintInput)
	return fp
}
func (in *RequestAuditFingerprintInput) DigestRequest(body []byte) {
	if in == nil || in.record == nil {
		return
	}
	in.RequestDigest = in.record.digest("request/v1", body)
}
func (in *RequestAuditFingerprintInput) DigestEvent(index int, typ string, data []byte) string {
	if in == nil || in.record == nil {
		return ""
	}
	var idx [8]byte
	binary.BigEndian.PutUint64(idx[:], uint64(index))
	payload := append(idx[:], lengthPrefixed([]byte(typ))...)
	payload = append(payload, lengthPrefixed(data)...)
	digest := in.record.digest("event/v1", payload)
	if in.Events == nil {
		in.Events = make(map[int]string)
	}
	in.Events[index] = digest
	return digest
}

// DigestModel fingerprints a model alias at any request-audit stage. One fixed kind
// keeps equal aliases comparable within a logical request without exposing the alias.
func (in *RequestAuditFingerprintInput) DigestModel(model string) string {
	return in.DigestIdentifier("model", model)
}

// DigestIdentifier returns a record-scoped fingerprint for a supported protocol identifier.
// Unknown kinds and empty values fail closed so arbitrary data cannot be fingerprinted here.
const maxRequestAuditFingerprintIdentifierBytes = 256

func safeRequestAuditIdentifier(value string) bool {
	if value == "" || len(value) > maxRequestAuditFingerprintIdentifierBytes {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (in *RequestAuditFingerprintInput) DigestIdentifier(kind, value string) string {
	if in == nil || in.record == nil || !safeRequestAuditIdentifier(value) {
		return ""
	}
	switch kind {
	case "model", "session", "response", "local_request", "upstream_request":
	default:
		return ""
	}
	payload := append(lengthPrefixed([]byte(kind)), lengthPrefixed([]byte(value))...)
	return in.record.digest("identifier/v1", payload)
}

func (r *requestAuditFingerprintRecord) digest(domain string, data []byte) string {
	mac := hmac.New(sha256.New, r.key)
	mac.Write(r.salt)
	mac.Write(lengthPrefixed([]byte(domain)))
	mac.Write(lengthPrefixed(data))
	return hex.EncodeToString(mac.Sum(nil))
}
func lengthPrefixed(data []byte) []byte {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(data)))
	return append(n[:], data...)
}
