package service

import (
	"context"
	"fmt"
)

type RequestAuditRouteFamily string

const (
	RequestAuditRouteUnknown         RequestAuditRouteFamily = ""
	RequestAuditRouteMessages        RequestAuditRouteFamily = "messages"
	RequestAuditRouteChatCompletions RequestAuditRouteFamily = "chat_completions"
	RequestAuditRouteResponses       RequestAuditRouteFamily = "responses"
)

type RequestAuditForceSettings struct {
	Enabled         bool
	Messages        bool
	ChatCompletions bool
	Responses       bool
}

func (s RequestAuditForceSettings) Required(family RequestAuditRouteFamily) bool {
	switch family {
	case RequestAuditRouteMessages:
		return s.Enabled || s.Messages
	case RequestAuditRouteChatCompletions:
		return s.Enabled || s.ChatCompletions
	case RequestAuditRouteResponses:
		return s.Enabled || s.Responses
	default:
		return false
	}
}

func (s *SystemSettings) RequestAuditForceSettings() RequestAuditForceSettings {
	if s == nil {
		return RequestAuditForceSettings{}
	}
	return RequestAuditForceSettings{
		Enabled:         s.RequestAuditForceEnabled,
		Messages:        s.RequestAuditForceMessages,
		ChatCompletions: s.RequestAuditForceChatCompletions,
		Responses:       s.RequestAuditForceResponses,
	}
}

// GetRequestAuditForceSettings reads the four enforcement switches directly
// from the shared settings store. Forced-mode enablement must propagate across
// application instances immediately; a process-local TTL cache could fail open.
func (s *SettingService) GetRequestAuditForceSettings(ctx context.Context) (RequestAuditForceSettings, error) {
	if s == nil || s.settingRepo == nil {
		return RequestAuditForceSettings{}, fmt.Errorf("request audit force settings store unavailable")
	}
	values, err := s.settingRepo.GetMultiple(ctx, []string{
		SettingKeyRequestAuditForceEnabled,
		SettingKeyRequestAuditForceMessages,
		SettingKeyRequestAuditForceChatCompletions,
		SettingKeyRequestAuditForceResponses,
	})
	if err != nil {
		return RequestAuditForceSettings{}, err
	}
	return RequestAuditForceSettings{
		Enabled:         values[SettingKeyRequestAuditForceEnabled] == "true",
		Messages:        values[SettingKeyRequestAuditForceMessages] == "true",
		ChatCompletions: values[SettingKeyRequestAuditForceChatCompletions] == "true",
		Responses:       values[SettingKeyRequestAuditForceResponses] == "true",
	}, nil
}
