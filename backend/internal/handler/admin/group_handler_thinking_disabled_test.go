package admin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGroupRequestsDecodeThinkingDisabledStrict(t *testing.T) {
	var createReq CreateGroupRequest
	require.NoError(t, json.Unmarshal([]byte(`{"name":"x","thinking_disabled_strict":true}`), &createReq))
	require.True(t, createReq.ThinkingDisabledStrict)

	var updateReq UpdateGroupRequest
	require.NoError(t, json.Unmarshal([]byte(`{"thinking_disabled_strict":false}`), &updateReq))
	require.NotNil(t, updateReq.ThinkingDisabledStrict)
	require.False(t, *updateReq.ThinkingDisabledStrict)

	var omitted UpdateGroupRequest
	require.NoError(t, json.Unmarshal([]byte(`{}`), &omitted))
	require.Nil(t, omitted.ThinkingDisabledStrict)
}
