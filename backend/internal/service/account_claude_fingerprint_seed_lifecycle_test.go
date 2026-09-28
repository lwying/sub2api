package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

const userSuppliedClaudeFingerprintSeed = "44444444-4444-4444-8444-444444444444"

func requireValidClaudeFingerprintSeed(t *testing.T, extra map[string]any) string {
	t.Helper()
	seed, ok := claudeFingerprintSeed(extra)
	require.True(t, ok, "expected valid canonical Claude fingerprint seed")
	return seed
}

func TestAdminCreateAccountStripsUserClaudeSeedAndCreatesFreshSeedWhenEnabled(t *testing.T) {
	svc := &adminServiceImpl{accountRepo: &upstreamBillingProbeAccountRepo{}}

	created, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "claude-oauth",
		Platform:             PlatformAnthropic,
		Type:                 AccountTypeOAuth,
		SkipDefaultGroupBind: true,
		Extra: map[string]any{
			claudeFingerprintModeExtraKey: "session",
			claudeFingerprintSeedExtraKey: userSuppliedClaudeFingerprintSeed,
		},
	})

	require.NoError(t, err)
	seed := requireValidClaudeFingerprintSeed(t, created.Extra)
	require.NotEqual(t, userSuppliedClaudeFingerprintSeed, seed)
	require.Equal(t, "session", created.Extra[claudeFingerprintModeExtraKey])
}

func TestAdminCreateAccountMintsClaudeSeedForSetupToken(t *testing.T) {
	svc := &adminServiceImpl{accountRepo: &upstreamBillingProbeAccountRepo{}}

	created, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "claude-setup-token",
		Platform:             PlatformAnthropic,
		Type:                 AccountTypeSetupToken,
		SkipDefaultGroupBind: true,
		Extra:                map[string]any{claudeFingerprintModeExtraKey: "device"},
	})

	require.NoError(t, err)
	requireValidClaudeFingerprintSeed(t, created.Extra)
}

func TestAdminCreateAccountDoesNotMintClaudeSeedForApiKey(t *testing.T) {
	svc := &adminServiceImpl{accountRepo: &upstreamBillingProbeAccountRepo{}}

	created, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "claude-apikey",
		Platform:             PlatformAnthropic,
		Type:                 AccountTypeAPIKey,
		SkipDefaultGroupBind: true,
		Extra:                map[string]any{claudeFingerprintModeExtraKey: "session"},
	})

	require.NoError(t, err)
	_, ok := claudeFingerprintSeed(created.Extra)
	require.False(t, ok, "API-key 账号不走 Claude 收敛，不应拿到 seed")
}

func TestAdminUpdateAccountPreservesExistingClaudeSeedAndStripsUserSeed(t *testing.T) {
	accountID := int64(2001)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:       accountID,
			Name:     "before",
			Platform: PlatformAnthropic,
			Type:     AccountTypeOAuth,
			Status:   StatusActive,
			Extra: map[string]any{
				claudeFingerprintModeExtraKey: "session",
				claudeFingerprintSeedExtraKey: claudeFingerprintTestSeed,
			},
		},
	}}

	updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{
			claudeFingerprintModeExtraKey: "full",
			claudeFingerprintSeedExtraKey: userSuppliedClaudeFingerprintSeed,
			"custom":                      "value",
		},
	})

	require.NoError(t, err)
	require.Equal(t, claudeFingerprintTestSeed, requireValidClaudeFingerprintSeed(t, updated.Extra))
	require.Equal(t, "full", updated.Extra[claudeFingerprintModeExtraKey])
	require.Equal(t, "value", updated.Extra["custom"])
}

func TestAdminUpdateAccountInitializesClaudeSeedWhenFullEditEnables(t *testing.T) {
	accountID := int64(2002)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:       accountID,
			Platform: PlatformAnthropic,
			Type:     AccountTypeOAuth,
			Status:   StatusActive,
			Extra: map[string]any{
				claudeFingerprintModeExtraKey: "off",
				claudeFingerprintSeedExtraKey: "not-a-seed",
			},
		},
	}}

	updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{claudeFingerprintModeExtraKey: "device"},
	})

	require.NoError(t, err)
	require.NotEqual(t, "not-a-seed", requireValidClaudeFingerprintSeed(t, updated.Extra))
}

func TestAdminUpdateAccountDisableReenablePreservesValidClaudeSeed(t *testing.T) {
	accountID := int64(2003)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:       accountID,
			Platform: PlatformAnthropic,
			Type:     AccountTypeOAuth,
			Status:   StatusActive,
			Extra: map[string]any{
				claudeFingerprintModeExtraKey: "session",
				claudeFingerprintSeedExtraKey: claudeFingerprintTestSeed,
			},
		},
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	disabled, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{claudeFingerprintModeExtraKey: "off"},
	})
	require.NoError(t, err)
	require.Equal(t, claudeFingerprintTestSeed, requireValidClaudeFingerprintSeed(t, disabled.Extra))

	reenabled, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{claudeFingerprintModeExtraKey: "session"},
	})
	require.NoError(t, err)
	require.Equal(t, claudeFingerprintTestSeed, requireValidClaudeFingerprintSeed(t, reenabled.Extra),
		"关闭再开启不得轮换 seed，否则上游看到的设备身份会重置")
}

func TestAdminUpdateAccountExtraStripsClaudeSeedAndLeavesAtomicEnsureToRepository(t *testing.T) {
	accountID := int64(2004)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {ID: accountID, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Extra: map[string]any{}},
	}}

	err := (&adminServiceImpl{accountRepo: repo}).UpdateAccountExtra(context.Background(), accountID, map[string]any{
		claudeFingerprintModeExtraKey: "device",
		claudeFingerprintSeedExtraKey: userSuppliedClaudeFingerprintSeed,
	})

	require.NoError(t, err)
	require.Len(t, repo.updates[accountID], 1)
	require.Equal(t, "device", repo.updates[accountID][0][claudeFingerprintModeExtraKey])
	require.NotContains(t, repo.updates[accountID][0], claudeFingerprintSeedExtraKey)
}

func TestBulkUpdateAccountsDoesNotPrewriteClaudeSeed(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{}

	result, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{3001, 3002},
		Extra: map[string]any{
			claudeFingerprintModeExtraKey: "session",
			claudeFingerprintSeedExtraKey: userSuppliedClaudeFingerprintSeed,
		},
	})

	require.NoError(t, err)
	require.Equal(t, 2, result.Success)
	require.Empty(t, repo.updates, "bulk enable must not loop through UpdateExtra before BulkUpdate")
	require.Len(t, repo.bulkUpdates, 1)
	require.True(t, repo.bulkUpdates[0].EnsureClaudeFingerprintSeed)
	require.False(t, repo.bulkUpdates[0].EnsureCodexFingerprintSeed,
		"Claude 模式不得让 Codex 的 seed 被浇筑到 Anthropic 行上")
	require.Equal(t, "session", repo.bulkUpdates[0].Extra[claudeFingerprintModeExtraKey])
	require.NotContains(t, repo.bulkUpdates[0].Extra, claudeFingerprintSeedExtraKey)
}

func TestBulkUpdateAccountsOffModeSkipsClaudeSeedEnsure(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{}

	_, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{3003},
		Extra:      map[string]any{claudeFingerprintModeExtraKey: "off"},
	})

	require.NoError(t, err)
	require.Len(t, repo.bulkUpdates, 1)
	require.False(t, repo.bulkUpdates[0].EnsureClaudeFingerprintSeed, "off 模式不得浇筑 seed")
}

func TestDuplicateAccountDoesNotCopyClaudeFingerprintSeed(t *testing.T) {
	ctx := context.Background()
	repo := &codexSeedDuplicateRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: make(map[int64]*Account)}}
	svc := &adminServiceImpl{accountRepo: repo, accountDuplicateRepo: repo}
	source := &Account{
		Name:     "claude-source",
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Extra: map[string]any{
			claudeFingerprintModeExtraKey: "session",
			claudeFingerprintSeedExtraKey: claudeFingerprintTestSeed,
		},
	}
	require.NoError(t, repo.Create(ctx, source))

	duplicate, err := svc.DuplicateAccount(ctx, source.ID, "admin:1", "")

	require.NoError(t, err)
	require.NotEqual(t, source.ID, duplicate.ID)
	require.NotContains(t, duplicate.Extra, claudeFingerprintSeedExtraKey,
		"复制账号必须重新铸造 seed，共用 seed 会让两份凭据在上游呈现同一台设备")
	require.Equal(t, "session", duplicate.Extra[claudeFingerprintModeExtraKey])
}

func TestAccountServiceCreateAndUpdateClaudeSeedLifecycle(t *testing.T) {
	ctx := context.Background()
	repo := &upstreamBillingProbeAccountRepo{accounts: make(map[int64]*Account)}
	svc := NewAccountService(repo, nil)

	created, err := svc.Create(ctx, CreateAccountRequest{
		Name:     "legacy-claude-create",
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			claudeFingerprintModeExtraKey: "session",
			claudeFingerprintSeedExtraKey: userSuppliedClaudeFingerprintSeed,
		},
	})
	require.NoError(t, err)
	createdSeed := requireValidClaudeFingerprintSeed(t, created.Extra)
	require.NotEqual(t, userSuppliedClaudeFingerprintSeed, createdSeed)

	updated, err := svc.Update(ctx, created.ID, UpdateAccountRequest{
		Extra: &map[string]any{
			claudeFingerprintModeExtraKey: "full",
			claudeFingerprintSeedExtraKey: userSuppliedClaudeFingerprintSeed,
		},
	})
	require.NoError(t, err)
	require.Equal(t, createdSeed, requireValidClaudeFingerprintSeed(t, updated.Extra))
	require.Equal(t, "full", updated.Extra[claudeFingerprintModeExtraKey])
}
