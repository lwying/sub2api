//go:build unit

package handler

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type gatewayMockAdmissionRPMCache struct {
	service.UserRPMCache
	groupCalls atomic.Int64
}

func (r *gatewayMockAdmissionRPMCache) IncrementUserGroupRPM(context.Context, int64, int64) (int, error) {
	return int(r.groupCalls.Add(1)), nil
}

// 真实标准模式 BillingCacheService 与 HTTP 入口的组合：只替换外部余额／RPM 缓存，
// 不用预先返回 ErrGroupRPMExceeded 的准入桩冒充真实限流。
func TestGatewayMock_StandardModeCountsRPMOnceAndRejectsExcess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newGatewayMockEnv(t)
	startGatewayMockRules(t, env.settingRepo, "hi", "本地回复")
	env.group.RPMLimit = 1
	rpm := &gatewayMockAdmissionRPMCache{}
	billing := service.NewBillingCacheService(newHandlerInflightCache(100), nil, nil, nil, rpm, nil, &config.Config{RunMode: config.RunModeStandard}, nil)
	t.Cleanup(billing.Stop)
	env.handler.billingCacheService = billing
	engine := blockingHandlerPromptEngine()
	env.handler.securityAuditCoordinator = securityaudit.NewCoordinator(nil, engine)
	events := &gatewayMockAuditEventStore{}
	env.handler.SetGatewayMockEventStore(events)

	first := env.serve(t, "/v1/messages", gatewayMockMessagesBody("hi", false))
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	require.Equal(t, service.GatewayMockReplyHeaderValue, first.Header().Get(service.GatewayMockReplyHeader))
	require.Equal(t, int64(1), rpm.groupCalls.Load(), "命中请求不能重复计算 RPM")

	second := env.serve(t, "/v1/messages", gatewayMockMessagesBody("hi", false))
	require.Equal(t, http.StatusTooManyRequests, second.Code, second.Body.String())
	require.Empty(t, second.Header().Get(service.GatewayMockReplyHeader))
	require.Equal(t, int64(2), rpm.groupCalls.Load())
	require.Equal(t, int64(1), events.calls.Load(), "限流拒绝不应产生命中记录")
	evaluated, enqueued, _ := engine.snapshot()
	require.Zero(t, evaluated)
	require.Zero(t, enqueued)
	require.Empty(t, upstreamPaths(env.capture))
}
