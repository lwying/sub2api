package routes

// 票据 10（旧敏感采集退役）的路由面补集断言。
//
// 既有用例 legacy_sensitive_capture_retirement_routes_test.go 逐条列出**已知**的旧路径，
// 并且只用 HTTP 状态码判断可达性。本文件改问它的补集，覆盖既有用例问不到的三种形态：
//
//  1. 整张已注册路由表里不得再出现承载旧敏感采集的路径段——这能抓住「换个名字把同一能力
//     重新挂回来」，而不只是「列出来的那几条没了」；
//  2. 旧入口的直接访问必须落到 **gin 的 NoRoute**，而不只是「返回 404」。状态码 404 也可能是
//     某个处理器自己的业务结论：本仓库里 `GET /api/v1/admin/usage/123/request-audit` 对不存在的
//     记录就返回 404（"Request audit not found"）。因此这里把 NoRoute 换成一个显式的哨兵
//     处理器，用哨兵而不是状态码区分「没有路由」与「有路由但查无此记录」；
//  3. 未被顺带删除的运维入口不只是「在路由表里」，把参数具体化后仍然**被分派到处理器**
//     （不是 NoRoute）。第 3 条是既有用例没做的：只查路由表无法区分「挂上了」和「挂在一个
//     永远不会被匹配到的模式上」。
//
// 第 3 条里处理器是空壳（AdminHandlers 的依赖为空），分派到它只会得到业务错误码；这里断言
// 的是「被分派了」，不是处理器的业务行为，因此任何非哨兵响应都算通过。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 旧敏感采集在路径上的承载段。用路径段而不是整条路径匹配：换一层前缀或改名重挂同样会被抓到。
var legacyCapturePathSegments = []string{
	"value-detail",
	"value_details",
	"error-diagnostic",
	"error_diagnostics",
}

const (
	noRouteSentinelStatus = http.StatusTeapot
	noRouteSentinelBody   = "no-route-sentinel"
)

func TestNoRegisteredRouteCarriesLegacySensitiveCaptureSegments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	// NoRoute 哨兵：命中它就证明「没有任何已注册路由匹配」，而不是某个处理器自己回了 404。
	router.NoRoute(func(c *gin.Context) { c.String(noRouteSentinelStatus, noRouteSentinelBody) })
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() })
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)

	registered := map[string]bool{}
	registeredPaths := make([]string, 0, len(router.Routes()))
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
		registeredPaths = append(registeredPaths, route.Method+" "+route.Path)
		lower := strings.ToLower(route.Path)
		for _, segment := range legacyCapturePathSegments {
			require.NotContains(t, lower, segment,
				"旧敏感采集路径段不得再出现在路由表：%s %s", route.Method, route.Path)
		}
	}
	// 阳性对照：路由表确实被枚举到了，否则上面的遍历是空集。
	require.NotEmpty(t, registeredPaths)
	require.False(t, registered[http.MethodGet+" /api/v1/admin/usage/:id/request-audit"],
		"旧请求审计读取入口已退役")
	require.True(t, registered[http.MethodGet+" /api/v1/admin/usage/:id/request-audit/forced"],
		"阳性对照：仅强制审计元数据运维入口保留")

	// 参数具体化后的直接访问必须落到 NoRoute 哨兵。比既有用例多出的形态：
	// 真实 id、比登记路径更深的子路径（value-detail/reveal）。
	legacyDirectHits := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/usage/123/request-audit"},
		{http.MethodGet, "/api/v1/admin/usage/123/request-audit/value-detail"},
		{http.MethodPost, "/api/v1/admin/usage/123/request-audit/value-detail"},
		{http.MethodGet, "/api/v1/admin/usage/123/request-audit/value-detail/reveal"},
		{http.MethodGet, "/api/v1/admin/usage/request-audit-value-detail-settings"},
		{http.MethodPut, "/api/v1/admin/usage/request-audit-value-detail-settings"},
		{http.MethodGet, "/api/v1/admin/settings/error-diagnostic"},
		{http.MethodPut, "/api/v1/admin/settings/error-diagnostic"},
		{http.MethodGet, "/api/v1/admin/error-diagnostics"},
		{http.MethodGet, "/api/v1/admin/error-diagnostics/abc123"},
		{http.MethodPost, "/api/v1/admin/error-diagnostics/abc123/body"},
		{http.MethodPost, "/api/v1/admin/error-diagnostics/abc123/headers"},
	}
	for _, legacy := range legacyDirectHits {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(legacy.method, legacy.path, nil))
		require.Equal(t, noRouteSentinelStatus, recorder.Code,
			"旧入口必须没有匹配到任何路由（不是「匹配到但业务上查无此记录」）：%s %s", legacy.method, legacy.path)
		require.Equal(t, noRouteSentinelBody, recorder.Body.String(),
			"旧入口必须是 NoRoute，而不是某个处理器自己回了 404：%s %s", legacy.method, legacy.path)
	}

	// 未被顺带删除的入口：既在路由表里，也能被真正分派到（不是 NoRoute）。
	kept := []struct {
		method, pattern, concrete string
	}{
		{http.MethodGet, "/api/v1/admin/usage/:id/request-audit/forced", "/api/v1/admin/usage/123/request-audit/forced"},
		{http.MethodGet, "/api/v1/admin/settings/request-trace", "/api/v1/admin/settings/request-trace"},
		{http.MethodPut, "/api/v1/admin/settings/request-trace", "/api/v1/admin/settings/request-trace"},
	}
	for _, route := range kept {
		require.True(t, registered[route.method+" "+route.pattern],
			"不得顺带删除：%s %s", route.method, route.pattern)
	}
	for _, route := range kept {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(route.method, route.concrete, nil))
		require.NotEqual(t, noRouteSentinelStatus, recorder.Code,
			"仍在挂载的入口必须被分派到处理器，而不是落到 NoRoute：%s %s", route.method, route.concrete)
		require.NotEqual(t, noRouteSentinelBody, recorder.Body.String(),
			"仍在挂载的入口必须被分派到处理器，而不是落到 NoRoute：%s %s", route.method, route.concrete)
	}
}
