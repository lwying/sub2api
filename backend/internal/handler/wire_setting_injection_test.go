//go:build unit

package handler

// 生产装配必须把设置服务交给两个网关处理器：下游测试请求 mock 与 Trace 采集范围
// 都只从 settingService 读取，漏注入时相关分支会静默变成空操作——功能看起来
// "已实现"、测试也可能全绿（测试夹具自己注入了），线上却从未生效。
//
// 这类缺陷不会编译报错，也不会在夹具里复现，因此用一条针对装配函数签名的断言
// 把它钉住：两个 Provide 函数都必须接收 *service.SettingService。

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 只钉住"装配函数签名里有这个类型"是不够的：把 `h.SetSettingService(...)` 那一行
// 删掉，签名断言照样通过，而功能又回到静默失效。这条用例钉的是**注入之后的可见行为**：
// 未注入时判定为不命中，注入启用规则的设置服务后同一次调用必须命中。
func TestSettingServiceInjectionMakesTheMockGateLive(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newContext := func() *gin.Context {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		return c
	}
	body := []byte(gatewayMockMessagesBody("hi", false))

	settingRepo := &gatewayMockSettingsRepo{values: map[string]string{}}
	startGatewayMockRules(t, settingRepo, "hi", "本地回复")
	settingService := gatewayMockSettingService(t, settingRepo)

	t.Run("openai handler", func(t *testing.T) {
		handler := NewOpenAIGatewayHandler(nil, nil, nil, nil, nil, nil, nil, nil, &config.Config{RunMode: config.RunModeSimple})
		require.False(t, handler.maybeServeDownstreamTestMock(newContext(), service.GatewayMockProtocolMessages, "claude-sonnet-4-5", 1, body),
			"未注入设置服务时必须按关闭处理")

		handler.SetSettingService(settingService)
		require.True(t, handler.maybeServeDownstreamTestMock(newContext(), service.GatewayMockProtocolMessages, "claude-sonnet-4-5", 1, body),
			"注入启用规则的设置服务后必须真正命中——只断言签名会让漏转发再次静默失效")
	})

	t.Run("anthropic handler", func(t *testing.T) {
		handler := &GatewayHandler{}
		require.False(t, handler.maybeServeDownstreamTestMock(newContext(), service.GatewayMockProtocolMessages, "claude-sonnet-4-5", 1, body))

		handler.settingService = settingService
		require.True(t, handler.maybeServeDownstreamTestMock(newContext(), service.GatewayMockProtocolMessages, "claude-sonnet-4-5", 1, body))
	})
}

// 这条用例直接调用**生产装配函数**，断言它返回的处理器上 mock 判定是活的。
//
// 为什么必须这样测：只断言"签名里有 SettingService 参数"挡不住最危险的回归——
// 参数还在、转发那一行被删掉。Go 允许未使用的参数，所以那样改会照常编译、签名断言
// 也照常通过，于是功能再次静默失效（本次事故正是这个形态）。只有把装配结果拿起来
// 用一次，才能真正钉住转发。
func TestProvideOpenAIGatewayHandlerForwardsTheSettingService(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settingRepo := &gatewayMockSettingsRepo{values: map[string]string{}}
	startGatewayMockRules(t, settingRepo, "hi", "本地回复")

	// 装配函数会在网关服务上挂插件与审计依赖，因此这两者必须是真的对象；
	// 其余依赖只被存进字段，传 nil 即可。
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gatewayService := service.NewOpenAIGatewayService(
		nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	handler := ProvideOpenAIGatewayHandler(
		gatewayService,
		nil, // plugin manager
		nil, // concurrency service
		nil, // billing cache
		nil, // api key service
		nil, // usage record worker pool
		nil, // error passthrough
		nil, // content moderation
		nil, // ops service
		nil, // grok quota service
		cfg,
		nil, // security audit coordinator
		nil, // request audit repository
		nil, // request audit fingerprinter
		nil, // composite route resolver
		gatewayMockSettingService(t, settingRepo),
		nil, // minimal mock event store: not wired here, the reply must still work
	)
	require.NotNil(t, handler)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	require.True(t, handler.maybeServeDownstreamTestMock(c, service.GatewayMockProtocolMessages, "claude-sonnet-4-5", 1,
		[]byte(gatewayMockMessagesBody("hi", false))),
		"装配函数必须把设置服务转发给处理器：否则 OpenAI 侧三个入口在生产上又会变成空操作")
}

func TestGatewayProvidersReceiveSettingService(t *testing.T) {
	settingServiceType := reflect.TypeOf((*service.SettingService)(nil))

	for _, testCase := range []struct {
		name     string
		provider any
	}{
		{name: "ProvideGatewayHandler", provider: ProvideGatewayHandler},
		{name: "ProvideOpenAIGatewayHandler", provider: ProvideOpenAIGatewayHandler},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			signature := reflect.TypeOf(testCase.provider)
			require.Equal(t, reflect.Func, signature.Kind())

			var found bool
			for index := 0; index < signature.NumIn(); index++ {
				if signature.In(index) == settingServiceType {
					found = true
					break
				}
			}
			require.True(t, found,
				"%s 必须接收 *service.SettingService：漏注入会让 mock 与 Trace 采集范围在生产上静默失效", testCase.name)
		})
	}
}
