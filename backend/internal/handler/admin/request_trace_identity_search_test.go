//go:build unit

package admin

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestTraceAdminFiltersAuthenticatedIdentityAndMetadataKeyword(t *testing.T) {
	recorder, filter, calls := listTracesWithQuery(t, "user_id=23&api_key_id=19&q=Claude")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, calls)
	require.NotNil(t, filter.UserID)
	require.Equal(t, int64(23), *filter.UserID)
	require.NotNil(t, filter.APIKeyID)
	require.Equal(t, int64(19), *filter.APIKeyID)
	require.Equal(t, "Claude", filter.Keyword)
}

func TestRequestTraceAdminRejectsAmbiguousIdentityAndUnboundedKeyword(t *testing.T) {
	for _, query := range []string{
		"user_id=0", "api_key_id=-1", "user_id=3&user_unknown=true",
		"api_key_id=4&api_key_unknown=true", "q=%20", "q=%25%25", "q=" + strings.Repeat("x", 129),
	} {
		t.Run(query, func(t *testing.T) {
			recorder, _, calls := listTracesWithQuery(t, query)
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Zero(t, calls)
		})
	}
}
