//go:build unit

package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRequestAuditSafeMetadataStoresOnlyDigestIdentifiers(t *testing.T) {
	fingerprinter, err := service.NewRequestAuditFingerprinter(strings.Repeat("k", 32))
	require.NoError(t, err)
	fp, err := fingerprinter.BeginForUser(7)
	require.NoError(t, err)

	sessionID := "session-canary-raw-value"
	responseID := "resp-canary-raw-value"
	routes := map[string]string{"inbound": "/v1/responses", "upstream": "/v1/responses"}
	localRequestID := "middleware-request-canary"
	upstreamRequestID := "configured-upstream-request-canary"
	metadata := requestAuditSafeMetadata(routes, fp, sessionID, responseID, localRequestID, upstreamRequestID)

	routes["inbound"] = "/changed"
	require.Equal(t, "/v1/responses", metadata.Routes["inbound"], "route inputs are copied before crossing the async boundary")
	require.Equal(t, fp.DigestIdentifier("session", sessionID), metadata.IDs["session_fingerprint"])
	require.Equal(t, fp.DigestIdentifier("response", responseID), metadata.IDs["response_fingerprint"])
	require.Equal(t, fp.DigestIdentifier("local_request", localRequestID), metadata.IDs["local_request_fingerprint"])
	require.Equal(t, fp.DigestIdentifier("upstream_request", upstreamRequestID), metadata.IDs["upstream_request_fingerprint"])
	require.Len(t, metadata.IDs, 4)

	encoded, err := json.Marshal(metadata)
	require.NoError(t, err)
	serialized := string(encoded)
	require.NotContains(t, serialized, sessionID)
	require.NotContains(t, serialized, responseID)
	require.NotContains(t, serialized, localRequestID)
	require.NotContains(t, serialized, upstreamRequestID)
}

func TestRequestAuditSafeMetadataOmitsUntrustedOrUnavailableIdentifiers(t *testing.T) {
	fingerprinter, err := service.NewRequestAuditFingerprinter(strings.Repeat("k", 32))
	require.NoError(t, err)
	fp, err := fingerprinter.BeginForUser(7)
	require.NoError(t, err)

	cases := []struct {
		name              string
		sessionID         string
		responseID        string
		localRequestID    string
		upstreamRequestID string
	}{
		{name: "empty"},
		{name: "whitespace", sessionID: "  ", responseID: "\t", localRequestID: " ", upstreamRequestID: "\t"},
		{name: "control byte", sessionID: "session\x00id", responseID: "resp\n-id", localRequestID: "local\x00id", upstreamRequestID: "upstream\nid"},
		{name: "too long", sessionID: strings.Repeat("s", maxRequestAuditIdentifierBytes+1), responseID: strings.Repeat("r", maxRequestAuditIdentifierBytes+1), localRequestID: strings.Repeat("l", maxLocalRequestAuditIdentifierBytes+1), upstreamRequestID: strings.Repeat("u", maxRequestAuditIdentifierBytes+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metadata := requestAuditSafeMetadata(nil, fp, tc.sessionID, tc.responseID, tc.localRequestID, tc.upstreamRequestID)
			require.Empty(t, metadata.IDs)
		})
	}

	require.Empty(t, requestAuditSafeMetadata(nil, nil, "session", "response", "local", "upstream").IDs,
		"metadata must fail closed when no user-scoped request fingerprinter exists")
}

func TestRequestAuditSafeMetadataKeepsNewKindsDigestOnly(t *testing.T) {
	fingerprinter, err := service.NewRequestAuditFingerprinter(strings.Repeat("k", 32))
	require.NoError(t, err)
	fp, err := fingerprinter.BeginForUser(7)
	require.NoError(t, err)

	const localID = "local-request-canary"
	const upstreamID = "upstream-request-canary"
	metadata := requestAuditSafeMetadata(nil, fp, "", "", localID, upstreamID)
	encoded, err := json.Marshal(metadata)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), localID)
	require.NotContains(t, string(encoded), upstreamID)
	require.Equal(t, fp.DigestIdentifier("local_request", localID), metadata.IDs["local_request_fingerprint"])
	require.Equal(t, fp.DigestIdentifier("upstream_request", upstreamID), metadata.IDs["upstream_request_fingerprint"])
}
