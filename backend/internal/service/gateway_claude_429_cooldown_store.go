package service

// Claude429CooldownStore returns the optional cross-node TTL store attached to
// the gateway cache. A cache that does not support this feature leaves the
// request-scoped 429 account limit intact without enabling cross-request state.
func (s *GatewayService) Claude429CooldownStore() Claude429CooldownStore {
	if s == nil {
		return nil
	}
	return Claude429CooldownStoreFromCache(s.cache)
}

func (s *OpenAIGatewayService) Claude429CooldownSettings() *SettingService {
	if s == nil {
		return nil
	}
	return s.settingService
}

func (s *OpenAIGatewayService) Claude429CooldownStore() Claude429CooldownStore {
	if s == nil {
		return nil
	}
	return Claude429CooldownStoreFromCache(s.cache)
}
