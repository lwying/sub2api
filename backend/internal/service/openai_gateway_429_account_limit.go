package service

import "context"

func (s *OpenAIGatewayService) GetRateLimit429AccountLimit(ctx context.Context) int {
	if s != nil && s.settingService != nil {
		if limit, err := s.settingService.GetRateLimit429AccountLimit(ctx); err == nil {
			return limit
		}
	}
	return defaultRateLimit429AccountLimit
}
