package service

import "testing"

func TestResolveAnthropicCompatUpstreamModel(t *testing.T) {
	tests := []struct {
		name        string
		accountType string
		model       string
		mapping     map[string]any
		want        string
	}{
		{
			name:        "api key mapping does not normalize target",
			accountType: AccountTypeAPIKey,
			model:       "alias",
			mapping:     map[string]any{"alias": "claude-haiku-4-5"},
			want:        "claude-haiku-4-5",
		},
		{
			name:        "service account identity mapping still normalizes",
			accountType: AccountTypeServiceAccount,
			model:       "claude-haiku-4-5",
			mapping:     map[string]any{"claude-haiku-4-5": "claude-haiku-4-5"},
			want:        "claude-haiku-4-5@20251001",
		},
		{
			name:        "service account explicit mapping bypasses vertex normalization",
			accountType: AccountTypeServiceAccount,
			model:       "alias",
			mapping:     map[string]any{"alias": "claude-haiku-4-5-20251001"},
			want:        "claude-haiku-4-5-20251001",
		},
		{
			name:        "oauth ignores account mapping and normalizes",
			accountType: AccountTypeOAuth,
			model:       "claude-haiku-4-5",
			mapping:     map[string]any{"claude-haiku-4-5": "ignored"},
			want:        "claude-haiku-4-5-20251001",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				Platform:    PlatformAnthropic,
				Type:        tt.accountType,
				Credentials: map[string]any{"model_mapping": tt.mapping},
			}
			if got := ResolveAnthropicCompatUpstreamModel(account, tt.model); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
