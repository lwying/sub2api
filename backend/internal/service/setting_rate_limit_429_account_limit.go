package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

const (
	defaultRateLimit429AccountLimit = 2
	minRateLimit429AccountLimit     = 1
	maxRateLimit429AccountLimit     = 100
)

func (s *SettingService) GetRateLimit429AccountLimit(ctx context.Context) (int, error) {
	value, err := s.settingRepo.GetValue(ctx, SettingKeyRateLimit429AccountLimit)
	if errors.Is(err, ErrSettingNotFound) {
		return defaultRateLimit429AccountLimit, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get 429 account limit: %w", err)
	}
	if value == "" {
		return defaultRateLimit429AccountLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < minRateLimit429AccountLimit || limit > maxRateLimit429AccountLimit {
		return defaultRateLimit429AccountLimit, nil
	}
	return limit, nil
}

func (s *SettingService) SetRateLimit429AccountLimit(ctx context.Context, limit int) error {
	if limit < minRateLimit429AccountLimit || limit > maxRateLimit429AccountLimit {
		return fmt.Errorf("max_accounts must be between 1-100")
	}
	return s.settingRepo.SetMultiple(ctx, map[string]string{SettingKeyRateLimit429AccountLimit: strconv.Itoa(limit)})
}
