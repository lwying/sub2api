package handler

import (
	"strings"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	maxRequestAuditIdentifierBytes      = 256
	maxLocalRequestAuditIdentifierBytes = 64
)

// requestAuditSafeMetadata copies allowed route input and stores only keyed digests
// of IDs whose provenance is established. Raw identifiers must never enter audit
// metadata or an asynchronous usage task.
func requestAuditSafeMetadata(
	routes map[string]string,
	fp *service.RequestAuditFingerprintInput,
	sessionID string,
	responseID string,
	correlationIDs ...string,
) service.RequestAuditMetadata {
	metadata := service.RequestAuditMetadata{Routes: cloneRequestAuditRoutes(routes)}
	if fp == nil {
		return metadata
	}

	var localRequestID, upstreamRequestID string
	if len(correlationIDs) > 0 {
		localRequestID = correlationIDs[0]
	}
	if len(correlationIDs) > 1 {
		upstreamRequestID = correlationIDs[1]
	}

	ids := make(map[string]string, 4)
	if digest, ok := digestRequestAuditIdentifier(fp, "session", sessionID); ok {
		ids["session_fingerprint"] = digest
	}
	if digest, ok := digestRequestAuditIdentifier(fp, "response", responseID); ok {
		ids["response_fingerprint"] = digest
	}
	// This is the observed local correlation ID after RequestLogger normalization;
	// it may originate from an incoming X-Request-ID and is never stored raw.
	if digest, ok := digestRequestAuditIdentifierWithLimit(fp, "local_request", localRequestID, maxLocalRequestAuditIdentifierBytes); ok {
		ids["local_request_fingerprint"] = digest
	}
	// Caller supplies this only from the account-configured direct-upstream response header.
	if digest, ok := digestRequestAuditIdentifier(fp, "upstream_request", upstreamRequestID); ok {
		ids["upstream_request_fingerprint"] = digest
	}
	if len(ids) > 0 {
		metadata.IDs = ids
	}
	return metadata
}

func digestRequestAuditIdentifier(fp *service.RequestAuditFingerprintInput, kind, value string) (string, bool) {
	return digestRequestAuditIdentifierWithLimit(fp, kind, value, maxRequestAuditIdentifierBytes)
}

func digestRequestAuditIdentifierWithLimit(fp *service.RequestAuditFingerprintInput, kind, value string, maxBytes int) (string, bool) {
	if fp == nil || !safeRequestAuditIdentifierWithLimit(value, maxBytes) {
		return "", false
	}
	digest := fp.DigestIdentifier(kind, value)
	if digest == "" {
		return "", false
	}
	return digest, true
}

func safeRequestAuditIdentifier(value string) bool {
	return safeRequestAuditIdentifierWithLimit(value, maxRequestAuditIdentifierBytes)
}

func safeRequestAuditIdentifierWithLimit(value string, maxBytes int) bool {
	if len(value) == 0 || len(value) > maxBytes || strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func cloneRequestAuditRoutes(routes map[string]string) map[string]string {
	if len(routes) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(routes))
	for key, value := range routes {
		cloned[key] = value
	}
	return cloned
}
