package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSettingServiceRequestAuditForceSettingsDefaultOffAndParseValues(t *testing.T) {
	t.Run("missing values default off", func(t *testing.T) {
		svc := NewSettingService(&settingGetAllRepoStub{values: map[string]string{}}, &config.Config{})

		settings, err := svc.GetAllSettings(context.Background())
		require.NoError(t, err)
		require.False(t, settings.RequestAuditForceEnabled)
		require.False(t, settings.RequestAuditForceMessages)
		require.False(t, settings.RequestAuditForceChatCompletions)
		require.False(t, settings.RequestAuditForceResponses)
	})

	t.Run("stored values are parsed independently", func(t *testing.T) {
		svc := NewSettingService(&settingGetAllRepoStub{values: map[string]string{
			SettingKeyRequestAuditForceEnabled:         "false",
			SettingKeyRequestAuditForceMessages:        "true",
			SettingKeyRequestAuditForceChatCompletions: "false",
			SettingKeyRequestAuditForceResponses:       "true",
		}}, &config.Config{})

		settings, err := svc.GetAllSettings(context.Background())
		require.NoError(t, err)
		require.False(t, settings.RequestAuditForceEnabled)
		require.True(t, settings.RequestAuditForceMessages)
		require.False(t, settings.RequestAuditForceChatCompletions)
		require.True(t, settings.RequestAuditForceResponses)
	})
}

func TestSettingServiceRequestAuditForceCacheRefreshesAfterWrite(t *testing.T) {
	repo := &contentModerationTestSettingRepo{values: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})

	before, err := svc.GetRequestAuditForceSettings(context.Background())
	require.NoError(t, err)
	require.False(t, before.Required(RequestAuditRouteResponses))

	all, err := svc.GetAllSettings(context.Background())
	require.NoError(t, err)
	all.RequestAuditForceResponses = true
	require.NoError(t, svc.UpdateSettings(context.Background(), all))

	after, err := svc.GetRequestAuditForceSettings(context.Background())
	require.NoError(t, err)
	require.True(t, after.Required(RequestAuditRouteResponses))
}

func TestRequestAuditForceSettingsRequiredUsesGlobalOrFamily(t *testing.T) {
	settings := RequestAuditForceSettings{Messages: true}
	require.True(t, settings.Required(RequestAuditRouteMessages))
	require.False(t, settings.Required(RequestAuditRouteChatCompletions))
	require.False(t, settings.Required(RequestAuditRouteResponses))

	settings.Enabled = true
	require.True(t, settings.Required(RequestAuditRouteMessages))
	require.True(t, settings.Required(RequestAuditRouteChatCompletions))
	require.True(t, settings.Required(RequestAuditRouteResponses))
	require.False(t, settings.Required(RequestAuditRouteUnknown))
}
