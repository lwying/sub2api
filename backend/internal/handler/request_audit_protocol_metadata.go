package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/tidwall/gjson"
)

// requestAuditProtocolFields extracts only allowlisted protocol shape from a request body.
// It deliberately ignores field values other than the closed thinking.type enum and a
// JSON boolean stream value; model content and arbitrary caller-controlled names never
// enter the returned audit metadata.
func requestAuditProtocolFields(body []byte) service.RequestAuditProtocolFields {
	var fields service.RequestAuditProtocolFields
	if !gjson.ValidBytes(body) {
		return fields
	}

	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return fields
	}

	// Keep this list closed and ordered. Presence is useful protocol metadata, while
	// recording arbitrary top-level names could leak prompt or caller-controlled data.
	for _, key := range [...]string{"model", "messages", "input", "tools", "stream", "thinking"} {
		if root.Get(key).Exists() {
			fields.PresentFields = append(fields.PresentFields, key)
		}
	}

	stream := root.Get("stream")
	if stream.Type == gjson.True || stream.Type == gjson.False {
		value := stream.Bool()
		fields.Stream = &value
	}

	thinking := root.Get("thinking")
	if !thinking.IsObject() {
		return fields
	}

	thinkingType := thinking.Get("type")
	if thinkingType.Type != gjson.String {
		return fields
	}
	switch thinkingType.String() {
	case "disabled", "enabled", "adaptive":
		fields.ThinkingType = thinkingType.String()
	}

	return fields
}
