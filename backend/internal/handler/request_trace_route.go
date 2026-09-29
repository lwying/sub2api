package handler

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// ClassifyRequestTraceRoute identifies candidate HTTP ingress endpoints. It does
// not prove the selected account uses a supported wire protocol or that an actual
// upstream attempt was observed; the capture layer must check those separately.
// RouteFamily describes inbound HTTP shape only, not the eventual wire protocol.
func ClassifyRequestTraceRoute(method, path string) (service.RequestTraceRouteFamily, string, bool) {
	if method != http.MethodPost {
		return "", "", false
	}

	switch path {
	case EndpointMessages:
		return service.RequestTraceMessages, EndpointMessages, true
	case EndpointChatCompletions, "/chat/completions":
		return service.RequestTraceChatCompletions, EndpointChatCompletions, true
	case EndpointResponses, "/responses", "/backend-api/codex/responses":
		return service.RequestTraceResponses, EndpointResponses, true
	case EndpointResponsesCompact, "/responses/compact", "/backend-api/codex/responses/compact":
		return service.RequestTraceResponses, EndpointResponsesCompact, true
	}

	for _, root := range []string{EndpointResponsesCompact, "/responses/compact", "/backend-api/codex/responses/compact"} {
		if strings.HasPrefix(path, root+"/") && safeRequestTraceCompactSuffix(path[len(root)+1:]) {
			return service.RequestTraceResponses, EndpointResponsesCompact, true
		}
	}
	return "", "", false
}

// Match the upstream's segment-only path contract for supported compact aliases.
// Limit each segment and the entire suffix so an unbounded attacker-controlled
// path cannot be treated as a successful Trace capture candidate.
func safeRequestTraceCompactSuffix(suffix string) bool {
	parts := strings.Split(suffix, "/")
	if len(parts) == 0 || len(parts) > 8 {
		return false
	}
	for _, part := range parts {
		if len(part) == 0 || len(part) > 128 {
			return false
		}
		dotsOnly := true
		for i := 0; i < len(part); i++ {
			c := part[i]
			if c != '.' {
				dotsOnly = false
			}
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.' {
				continue
			}
			return false
		}
		if dotsOnly {
			return false
		}
	}
	return true
}
