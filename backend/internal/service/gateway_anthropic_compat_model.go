package service

import "github.com/Wei-Shaw/sub2api/internal/pkg/claude"

// ResolveAnthropicCompatUpstreamModel resolves the account-specific model used
// by the OpenAI Chat Completions and Responses compatibility paths.
func ResolveAnthropicCompatUpstreamModel(account *Account, originalModel string) string {
	mappedModel := originalModel
	if account.Type == AccountTypeAPIKey || account.Type == AccountTypeServiceAccount {
		mappedModel = account.GetMappedModel(originalModel)
	}
	if mappedModel == originalModel && account.Platform == PlatformAnthropic && account.Type == AccountTypeServiceAccount {
		normalized := normalizeVertexAnthropicModelID(claude.NormalizeModelID(originalModel))
		if normalized != originalModel {
			mappedModel = normalized
		}
	} else if mappedModel == originalModel && account.Platform == PlatformAnthropic && account.Type != AccountTypeAPIKey {
		normalized := claude.NormalizeModelID(originalModel)
		if normalized != originalModel {
			mappedModel = normalized
		}
	}
	return mappedModel
}
