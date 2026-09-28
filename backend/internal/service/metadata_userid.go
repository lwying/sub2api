package service

import (
	"encoding/json"
	"regexp"
	"strings"
)

// NewMetadataFormatMinVersion is the minimum Claude Code version that uses
// JSON-formatted metadata.user_id instead of the legacy concatenated string.
const NewMetadataFormatMinVersion = "2.1.78"

// ParsedUserID represents the components extracted from a metadata.user_id value.
type ParsedUserID struct {
	DeviceID    string // 64-char hex (or arbitrary client id)
	AccountUUID string // may be empty
	SessionID   string // UUID
	IsNewFormat bool   // true if the original was JSON format
}

// legacyUserIDRegex matches the legacy user_id format:
//
//	user_{64hex}_account_{optional_uuid}_session_{uuid}
var legacyUserIDRegex = regexp.MustCompile(`^user_([a-fA-F0-9]{64})_account_([a-fA-F0-9-]*)_session_([a-fA-F0-9-]{36})$`)

// jsonUserID is the JSON structure for the new metadata.user_id format.
type jsonUserID struct {
	DeviceID    string `json:"device_id"`
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

// ParseMetadataUserID parses a metadata.user_id string in either format.
// Returns nil if the input cannot be parsed.
func ParseMetadataUserID(raw string) *ParsedUserID {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	// Try JSON format first (starts with '{')
	if raw[0] == '{' {
		var j jsonUserID
		if err := json.Unmarshal([]byte(raw), &j); err != nil {
			return nil
		}
		if j.DeviceID == "" || j.SessionID == "" {
			return nil
		}
		return &ParsedUserID{
			DeviceID:    j.DeviceID,
			AccountUUID: j.AccountUUID,
			SessionID:   j.SessionID,
			IsNewFormat: true,
		}
	}

	// Try legacy format
	matches := legacyUserIDRegex.FindStringSubmatch(raw)
	if matches == nil {
		return nil
	}
	return &ParsedUserID{
		DeviceID:    matches[1],
		AccountUUID: matches[2],
		SessionID:   matches[3],
		IsNewFormat: false,
	}
}

// FormatMetadataUserID builds a metadata.user_id string in the format
// appropriate for the given CLI version. Components are the rewritten values
// (not necessarily the originals).
func FormatMetadataUserID(deviceID, accountUUID, sessionID, uaVersion string) string {
	return FormatMetadataUserIDInFormat(deviceID, accountUUID, sessionID, IsNewMetadataFormatVersion(uaVersion))
}

// FormatMetadataUserIDInFormat 按调用方**显式给定**的格式重建 metadata.user_id。
//
// 与 FormatMetadataUserID 的区别只有一个：格式从哪来。前者从 UA 里的 CLI 版本推断，
// 这里由调用方给——用于按「正文里客户端的原始形态」回写（ParsedUserID.IsNewFormat）。
// 两者可能不一致（UA 报新版本、正文却仍是旧拼接格式，见 ParseMetadataUserID 的注释），
// 此时按正文回写才不会让同一请求的格式与 UA 自相矛盾。
//
// 两套模板只在这里各写一份，FormatMetadataUserID 也复用本函数。
func FormatMetadataUserIDInFormat(deviceID, accountUUID, sessionID string, newFormat bool) string {
	if newFormat {
		b, _ := json.Marshal(jsonUserID{
			DeviceID:    deviceID,
			AccountUUID: accountUUID,
			SessionID:   sessionID,
		})
		return string(b)
	}
	// Legacy format
	return "user_" + deviceID + "_account_" + accountUUID + "_session_" + sessionID
}

// IsNewMetadataFormatVersion returns true if the given CLI version uses the
// new JSON metadata.user_id format (>= 2.1.78).
func IsNewMetadataFormatVersion(version string) bool {
	if version == "" {
		return false
	}
	return CompareVersions(version, NewMetadataFormatMinVersion) >= 0
}

// ExtractCLIVersion extracts the Claude Code version from a User-Agent string.
// Returns "" if the UA doesn't match the expected pattern.
func ExtractCLIVersion(ua string) string {
	matches := claudeCodeUAVersionPattern.FindStringSubmatch(ua)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}
