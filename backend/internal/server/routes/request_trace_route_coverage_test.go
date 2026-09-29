package routes

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Route coverage guard for the request Trace rollout gate (local ticket 11,
// spec docs/request-trace-spec-20260928.md §覆盖矩阵与采集边界).
//
// The first Trace phase claims exactly three HTTP ingress families — Claude
// Messages, Chat Completions and Responses (including their bare and Codex
// direct aliases) — and only for entrypoints that really are registered on the
// gateway. Bedrock/SigV4, Responses WebSocket, Gemini, count_tokens and the
// media/utility entrypoints are declared *uncovered* gaps; they must never be
// reported as captured. name-only similarity (for example the Antigravity
// Claude alias) is not proof of coverage.
//
// The guard reads the real registrations out of gateway.go and compares them
// with handler.ClassifyRequestTraceRoute, so a new, moved or removed route
// cannot silently change what Trace claims to cover, and an excluded route
// cannot smuggle in a false coverage claim.

// gatewayRouteGroupPrefixes maps every gin group variable used in gateway.go to
// the client-visible prefix of the templates registered on it. "r" is the root
// engine and rootRoute registers through r.Handle, so its prefix is empty.
var gatewayRouteGroupPrefixes = map[string]string{
	"r":                 "",
	"gateway":           "/v1",
	"gemini":            "/v1beta",
	"codexDirect":       "/backend-api/codex",
	"antigravityV1":     "/antigravity/v1",
	"antigravityV1Beta": "/antigravity/v1beta",
}

// rootRoutePrefixes is the prefix set gateway.go iterates when it calls
// rootRoute from inside the Seedance task loop.
var rootRoutePrefixes = []string{"", "/v1", "/v3", "/api/v3"}

var (
	gatewayGroupPostRoutePattern = regexp.MustCompile(`([A-Za-z_]\w*)\.POST\("([^"]+)"[,)]`)
	gatewayGroupGetRoutePattern  = regexp.MustCompile(`([A-Za-z_]\w*)\.GET\("([^"]+)"[,)]`)
	rootRoutePostLiteralPattern  = regexp.MustCompile(`rootRoute\(http\.MethodPost,\s*"([^"]+)"`)
	rootRoutePostLoopPattern     = regexp.MustCompile(`rootRoute\(http\.MethodPost,\s*prefix\+\s*"([^"]+)"`)
	rootRouteGetLiteralPattern   = regexp.MustCompile(`rootRoute\(http\.MethodGet,\s*"([^"]+)"`)
	rootRouteGetLoopPattern      = regexp.MustCompile(`rootRoute\(http\.MethodGet,\s*prefix\+\s*"([^"]+)"`)
	gatewayGroupTraceMiddleware  = regexp.MustCompile(`([A-Za-z_]\w*)\.Use\([^)]*requestTraceCapture`)
	rootRouteTraceMiddleware     = regexp.MustCompile(`r\.Handle\([^)]*requestTraceCapture`)
)

// gatewayTraceCoveredRoute is a registered route the classifier must report as
// a Trace HTTP candidate, plus the concrete request path that stands in for its
// template when the classifier is exercised.
type gatewayTraceCoveredRoute struct {
	probe    string
	family   service.RequestTraceRouteFamily
	endpoint string
}

// gatewayTraceCoveredRoutes is the only set of entrypoints the first Trace
// phase may claim. Keys are the client-visible route templates as registered by
// gateway.go, not the classifier's canonical endpoints.
var gatewayTraceCoveredRoutes = map[string]gatewayTraceCoveredRoute{
	"/v1/messages":                          {probe: "/v1/messages", family: service.RequestTraceMessages, endpoint: "/v1/messages"},
	"/v1/chat/completions":                  {probe: "/v1/chat/completions", family: service.RequestTraceChatCompletions, endpoint: "/v1/chat/completions"},
	"/v1/responses":                         {probe: "/v1/responses", family: service.RequestTraceResponses, endpoint: "/v1/responses"},
	"/v1/responses/*subpath":                {probe: "/v1/responses/compact", family: service.RequestTraceResponses, endpoint: "/v1/responses/compact"},
	"/chat/completions":                     {probe: "/chat/completions", family: service.RequestTraceChatCompletions, endpoint: "/v1/chat/completions"},
	"/responses":                            {probe: "/responses", family: service.RequestTraceResponses, endpoint: "/v1/responses"},
	"/responses/*subpath":                   {probe: "/responses/compact", family: service.RequestTraceResponses, endpoint: "/v1/responses/compact"},
	"/backend-api/codex/responses":          {probe: "/backend-api/codex/responses", family: service.RequestTraceResponses, endpoint: "/v1/responses"},
	"/backend-api/codex/responses/*subpath": {probe: "/backend-api/codex/responses/compact", family: service.RequestTraceResponses, endpoint: "/v1/responses/compact"},
}

// gatewayTraceExcludedRouteGroups lists every registered POST route the first
// Trace phase does NOT cover, each carrying the explicit reason it is a gap so
// the exclusion is a recorded decision rather than a silent omission.
var gatewayTraceExcludedRouteGroups = []struct {
	reason string
	routes []string
}{
	{
		reason: "count_tokens only estimates tokens and never produces a per-attempt upstream model wire to trace",
		routes: []string{
			"/v1/messages/count_tokens",
			"/messages/count_tokens",
			"/antigravity/v1/messages/count_tokens",
		},
	},
	{
		reason: "Gemini native v1beta and its Antigravity alias are outside the first-phase HTTP Trace matrix",
		routes: []string{
			"/v1beta/models/*modelAction",
			"/antigravity/v1beta/models/*modelAction",
		},
	},
	{
		reason: "the Antigravity Claude alias has no proven per-attempt wire; uncovered until proven",
		routes: []string{
			"/antigravity/v1/messages",
		},
	},
	{
		reason: "Live/realtime session endpoints are not text model attempts",
		routes: []string{
			"/v1/live",
			"/backend-api/codex/realtime/calls",
		},
	},
	{
		reason: "server-side search tools are not one of the three per-attempt-audited text protocols",
		routes: []string{
			"/v1/alpha/search",
			"/alpha/search",
			"/backend-api/codex/alpha/search",
			"/v1/web_search",
			"/web_search",
			"/v1/x_search",
			"/x_search",
		},
	},
	{
		reason: "embeddings are not one of the three per-attempt-audited text protocols",
		routes: []string{
			"/v1/embeddings",
			"/embeddings",
		},
	},
	{
		reason: "image generation endpoints are media, not text prompt attempts",
		routes: []string{
			"/v1/images/generations",
			"/v1/images/edits",
			"/v1/images/generations/async",
			"/v1/images/edits/async",
			"/v1/images/batches",
			"/v1/images/batches/:id/cancel",
			"/images/generations",
			"/images/edits",
			"/images/generations/async",
			"/images/edits/async",
		},
	},
	{
		reason: "video generation/task endpoints are media, not text prompt attempts",
		routes: []string{
			"/v1/videos",
			"/v1/videos/generations",
			"/v1/videos/edits",
			"/v1/videos/extensions",
			"/videos",
			"/videos/generations",
			"/videos/edits",
			"/videos/extensions",
			"/contents/generations/tasks",
			"/v1/contents/generations/tasks",
			"/v3/contents/generations/tasks",
			"/api/v3/contents/generations/tasks",
		},
	},
	{
		reason: "speech/voice endpoints carry audio rather than a text model prompt",
		routes: []string{
			"/v1/tts",
			"/v1/stt",
			"/v1/custom-voices",
			"/tts",
			"/stt",
			"/custom-voices",
		},
	},
}

func TestGatewayRequestTraceRouteCoverageMatchesRegisteredRoutes(t *testing.T) {
	source, err := os.ReadFile("gateway.go")
	require.NoError(t, err)
	routeSource := string(source)

	postRoutes := gatewayRegisteredPostRoutes(t, routeSource)
	getRoutes := gatewayRegisteredGetRoutes(t, routeSource)

	traceMiddlewareGroups := map[string]struct{}{}
	for _, match := range gatewayGroupTraceMiddleware.FindAllStringSubmatch(routeSource, -1) {
		traceMiddlewareGroups[match[1]] = struct{}{}
	}
	rootRouteHasTraceMiddleware := rootRouteTraceMiddleware.MatchString(routeSource)

	excluded := map[string]string{}
	for _, group := range gatewayTraceExcludedRouteGroups {
		for _, route := range group.routes {
			excluded[route] = group.reason
		}
	}

	// Every registered POST route must be either a deliberate coverage claim or
	// a named gap. A new gateway route that is neither fails this guard.
	unclassified := make([]string, 0)
	for route := range postRoutes {
		if _, ok := gatewayTraceCoveredRoutes[route]; ok {
			continue
		}
		if _, ok := excluded[route]; ok {
			continue
		}
		unclassified = append(unclassified, route)
	}
	sort.Strings(unclassified)
	require.Empty(t, unclassified, "new gateway POST routes must be claimed as Trace-covered or listed as an explicit uncovered gap")

	coveredFamilies := map[service.RequestTraceRouteFamily]struct{}{}
	coveredRoutes := sortedKeys(gatewayTraceCoveredRoutes)
	for _, route := range coveredRoutes {
		expectation := gatewayTraceCoveredRoutes[route]
		declaringGroup, registered := postRoutes[route]
		require.Truef(t, registered, "stale Trace coverage entry %s: gateway.go no longer registers it", route)
		coveredFamilies[expectation.family] = struct{}{}

		family, endpoint, ok := handler.ClassifyRequestTraceRoute(http.MethodPost, expectation.probe)
		require.Truef(t, ok, "%s must be classified as a Trace HTTP candidate", route)
		require.Equalf(t, expectation.family, family, "%s classified into the wrong route family", route)
		require.Equalf(t, expectation.endpoint, endpoint, "%s classified as the wrong normalized endpoint", route)

		// The claim is only real when the capture middleware is actually on the
		// chain that serves the route.
		if declaringGroup == "rootRoute" {
			require.Truef(t, rootRouteHasTraceMiddleware, "%s is served by rootRoute, which must install requestTraceCapture", route)
			continue
		}
		_, wired := traceMiddlewareGroups[declaringGroup]
		require.Truef(t, wired, "%s is declared on group %q, which must install requestTraceCapture", route, declaringGroup)
	}

	require.ElementsMatch(t,
		[]service.RequestTraceRouteFamily{
			service.RequestTraceMessages,
			service.RequestTraceChatCompletions,
			service.RequestTraceResponses,
		},
		sortedFamilies(coveredFamilies),
		"first-phase Trace coverage must be exactly the Messages, Chat Completions and Responses HTTP families",
	)

	excludedRoutes := sortedKeys(excluded)
	require.NotEmpty(t, excludedRoutes)
	for _, route := range excludedRoutes {
		require.NotEmptyf(t, strings.TrimSpace(excluded[route]), "excluded route %s needs an explicit uncovered reason", route)
		_, registered := postRoutes[route]
		require.Truef(t, registered, "stale excluded route %s: gateway.go no longer registers it", route)

		family, endpoint, ok := handler.ClassifyRequestTraceRoute(http.MethodPost, gatewayRouteProbe(route))
		require.Falsef(t, ok,
			"excluded route %s must stay unclassified; returning family %q endpoint %q would be a false coverage claim",
			route, family, endpoint)
	}

	// Responses WebSocket is registered as GET only. Trace covers HTTP attempts,
	// so the classifier must reject the WS entrypoints instead of reporting them.
	for _, route := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		_, registered := getRoutes[route]
		require.Truef(t, registered, "%s must exist as a GET registration (Responses WebSocket)", route)
		family, endpoint, ok := handler.ClassifyRequestTraceRoute(http.MethodGet, route)
		require.Falsef(t, ok, "Responses WebSocket %s must not be classified as an HTTP Trace route", route)
		require.Empty(t, family)
		require.Empty(t, endpoint)
	}

	// Bedrock/SigV4 is a declared gap: this gateway does not register it at all,
	// and the classifier must not claim it even for its request shape.
	for route := range postRoutes {
		lowered := strings.ToLower(route)
		require.NotContainsf(t, lowered, "bedrock", "Bedrock/SigV4 is a declared uncovered gap; register it as covered first")
		require.NotContainsf(t, lowered, "sigv4", "Bedrock/SigV4 is a declared uncovered gap; register it as covered first")
		require.NotContainsf(t, lowered, "/invoke", "Bedrock/SigV4 invoke paths are a declared uncovered gap")
	}
	for _, route := range []string{
		"/model/anthropic.claude-3-5-sonnet/invoke",
		"/model/anthropic.claude-3-5-sonnet/invoke-with-response-stream",
	} {
		family, endpoint, ok := handler.ClassifyRequestTraceRoute(http.MethodPost, route)
		require.Falsef(t, ok, "Bedrock/SigV4 shape %s must never be classified as Trace-covered", route)
		require.Empty(t, family)
		require.Empty(t, endpoint)
	}
}

// gatewayRegisteredPostRoutes resolves the concrete client-visible path of
// every POST route registered in gateway.go, mapped to the variable the
// registration was declared on ("rootRoute" for the shared root helper).
func gatewayRegisteredPostRoutes(t *testing.T, source string) map[string]string {
	t.Helper()
	registered := map[string]string{}
	record := func(route, declaredOn string) {
		require.NotContainsf(t, registered, route, "gateway.go registers POST %s more than once", route)
		registered[route] = declaredOn
	}
	for _, match := range gatewayGroupPostRoutePattern.FindAllStringSubmatch(source, -1) {
		prefix, known := gatewayRouteGroupPrefixes[match[1]]
		require.Truef(t, known, "gateway.go registers POST routes on unknown group %q; extend the guard", match[1])
		record(prefix+match[2], match[1])
	}
	for _, match := range rootRoutePostLiteralPattern.FindAllStringSubmatch(source, -1) {
		record(match[1], "rootRoute")
	}
	for _, match := range rootRoutePostLoopPattern.FindAllStringSubmatch(source, -1) {
		for _, prefix := range rootRoutePrefixes {
			record(prefix+match[1], "rootRoute")
		}
	}
	return registered
}

// gatewayRegisteredGetRoutes mirrors gatewayRegisteredPostRoutes for GET, which
// the guard only needs to prove the Responses WebSocket registrations exist.
func gatewayRegisteredGetRoutes(t *testing.T, source string) map[string]string {
	t.Helper()
	registered := map[string]string{}
	record := func(route, declaredOn string) {
		require.NotContainsf(t, registered, route, "gateway.go registers GET %s more than once", route)
		registered[route] = declaredOn
	}
	for _, match := range gatewayGroupGetRoutePattern.FindAllStringSubmatch(source, -1) {
		prefix, known := gatewayRouteGroupPrefixes[match[1]]
		require.Truef(t, known, "gateway.go registers GET routes on unknown group %q; extend the guard", match[1])
		record(prefix+match[2], match[1])
	}
	for _, match := range rootRouteGetLiteralPattern.FindAllStringSubmatch(source, -1) {
		record(match[1], "rootRoute")
	}
	for _, match := range rootRouteGetLoopPattern.FindAllStringSubmatch(source, -1) {
		for _, prefix := range rootRoutePrefixes {
			record(prefix+match[1], "rootRoute")
		}
	}
	return registered
}

// gatewayRouteProbe turns a registered route template into a concrete request
// path so the classifier can be exercised. Parameters and wildcards only need
// to be plausible, never real: every caller asserts an uncovered verdict.
func gatewayRouteProbe(template string) string {
	parts := strings.Split(template, "/")
	for i, part := range parts {
		switch {
		case strings.HasPrefix(part, ":"):
			parts[i] = "probe"
		case strings.HasPrefix(part, "*"):
			parts[i] = "wildcard"
		}
	}
	return strings.Join(parts, "/")
}

func sortedKeys[V any](entries map[string]V) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedFamilies(families map[service.RequestTraceRouteFamily]struct{}) []service.RequestTraceRouteFamily {
	sorted := make([]service.RequestTraceRouteFamily, 0, len(families))
	for family := range families {
		sorted = append(sorted, family)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted
}
