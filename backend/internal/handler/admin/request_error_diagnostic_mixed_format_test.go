//go:build unit

package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestMixedDiagnosticDisclosesEachLayerByItsOwnFormat(t *testing.T) {
	record := service.ErrorDiagnosticRecord{
		ID: errorDiagnosticTestID, Protocol: service.ErrorDiagnosticProtocolMessages,
		AttemptIndex: 1, UpstreamStatusCode: 429,
		CreatedAt:         errorDiagnosticTestNow.Add(-time.Hour),
		MetadataExpiresAt: errorDiagnosticTestNow.Add(24 * time.Hour),
		PlainRecord:       true, BodyState: service.ErrorDiagnosticBodyStateStored,
		BodyReason: service.ErrorDiagnosticBodyRetained, BodyStored: true,
		BodyExpiresAt:     errorDiagnosticTestNow.Add(4 * 24 * time.Hour),
		PlainHeaderState:  service.ErrorDiagnosticHeaderStateStored,
		PlainHeaderReason: service.ErrorDiagnosticPlainHeaderRetained,
		PlainHeaderStored: true, PlainHeaderEntryCount: 1,
	}
	reader := &errorDiagnosticReaderStub{record: record, body: []byte(`{"model":"claude"}`)}
	router := newErrorDiagnosticTestRouter(reader)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID, nil))
	require.Equal(t, http.StatusOK, response.Code)
	var payload struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	require.Equal(t, service.ErrorDiagnosticFormatEncrypted, payload.Data["body_format"])
	require.Equal(t, service.ErrorDiagnosticFormatPlaintext, payload.Data["header_format"])
	require.Equal(t, service.ErrorDiagnosticBodyStateStored, payload.Data["body_state"])
	require.Equal(t, service.ErrorDiagnosticHeaderStateStored, payload.Data["header_state"])

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/body", nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	require.Equal(t, service.ErrorDiagnosticFormatEncrypted, payload.Data["body_format"])
}
