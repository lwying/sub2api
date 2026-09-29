//go:build unit

package service

// 票据 10（旧敏感采集退役）在服务层的聚焦验收。
//
// 本文件只做「新增文件」的断言，不改动任何既有测试：
//   - 旧揭示入口的路由面在
//     backend/internal/server/routes/legacy_sensitive_capture_retirement_routes_test.go
//     与 backend/internal/server/routes/legacy_sensitive_capture_writes_test.go；
//   - 旧「三类新明文采集部署探针」接缝在 request_trace_settings_test.go；
//   - 强制审计门禁的预留失败路径在 request_audit_forced_test.go。
//
// 这里回答既有用例没有覆盖的三件事：
//
//	1. 值明细／错误诊断的**采集开关**在服务层是否还留有接缝（方法或字段）；
//	2. 退役后剩下的存储接口是否只剩到期清理，任何调用方都无法再写入新行；
//	3. 强制审计的发送前门禁是否仍然存在、并且探针／预留失败一律 fail closed，
//	   与新的 Trace 总开关是否开启无关。
//
// 判据用反射而不是「引用不存在的符号」：引用会直接编译失败，用例也就无法在退役前
// 以失败（红）的形式证明自己确实在盯着这件事。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 退役前 SettingService 上的值明细门控／运维开关与错误诊断运维开关。
// 只要其中任何一个还能被查到，后来者就能把旧采集重新接回门控。
var legacyRetirementSettingMethods = []string{
	// 值明细（request_audit_value_details）
	"ReadStoredRequestAuditValueDetailSettings",
	"RequestAuditValueDetailGate",
	"GetRequestAuditValueDetailOperatorStatus",
	"UpdateRequestAuditValueDetailOperatorSettings",
	"RequestAuditValueDetailRiskAcknowledgementCurrent",
	"RequestAuditValueDetailEncryptionKeyAvailable",
	// 错误诊断（error_diagnostic_records）
	"ReadStoredErrorDiagnosticSettings",
	"GetErrorDiagnosticSettings",
	"GetErrorDiagnosticOperatorStatus",
	"UpdateErrorDiagnosticOperatorSettings",
	"ErrorDiagnosticRiskAcknowledgementCurrent",
	"ErrorDiagnosticBodyEncryptionKeyAvailable",
	"ErrorDiagnosticPlainBodyRiskAcknowledgementCurrent",
	"ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementCurrent",
}

// 退役前 GatewayService／OpenAIGatewayService 上的采集写入器注入点与启用判定。
var legacyRetirementGatewaySetters = []string{
	"SetRequestAuditValueDetailCapture",
	"SetErrorDiagnosticRecorder",
	"SetErrorDiagnosticUsageAttacher",
	"ClaudeRequestAuditValueCaptureEnabled",
	"RequestAuditValueCaptureEnabled",
}

// 段名取自退役前的字段名（errorDiagnosticRecorder、requestAuditValueDetailCapture、
// errorDiagnosticUsageAttacher…）。这里不用裸 "capture"／"diagnostic" 做子串匹配：
// 新的 Trace 采集与旧清理积压观测都合法，裸匹配会误伤。
var legacyRetirementFieldMarkers = []string{
	"valuedetail",
	"errordiagnostic",
	"diagnosticrecorder",
	"diagnosticusageattacher",
}

func legacyRetirementMethodNames(t reflect.Type) []string {
	names := make([]string, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		names = append(names, t.Method(i).Name)
	}
	return names
}

// TestLegacySensitiveCaptureEnableSeamsAreRetired 断言旧采集开关在服务层没有接缝。
//
// 覆盖范围说明（如实记录缺口）：反射的 MethodByName 只能看到**导出**方法。
// 退役前这些开关全部是导出方法，所以「导出方法不存在」正好覆盖了曾经对外可用、
// 可被再次装配的开关面；未导出的内部辅助（例如退役前的 captureRequestAuditValueDetail、
// rejectPlaintextCaptureEnablement）不在本用例的覆盖范围内，也无法用反射断言其不存在。
func TestLegacySensitiveCaptureEnableSeamsAreRetired(t *testing.T) {
	settingType := reflect.TypeOf(&SettingService{})
	for _, name := range legacyRetirementSettingMethods {
		_, ok := settingType.MethodByName(name)
		require.False(t, ok, "SettingService 不得再暴露旧采集开关：%s", name)
	}

	// 阳性对照：同一套反射必须能查到**保留下来**的接缝，否则下面的「查不到」可能只是
	// 探针本身失效（例如类型写错），而不是真的退役了。
	for _, name := range []string{
		"RequestTraceGate",
		"GetRequestTraceOperatorStatus",
		"UpdateRequestTraceOperatorSettings",
		"SetRequestTraceSupportProbe",
	} {
		_, ok := settingType.MethodByName(name)
		require.True(t, ok, "阳性对照：SettingService 仍应暴露新 Trace 接缝 %s", name)
	}

	for _, gatewayType := range []reflect.Type{
		reflect.TypeOf(&GatewayService{}),
		reflect.TypeOf(&OpenAIGatewayService{}),
	} {
		for _, name := range legacyRetirementGatewaySetters {
			_, ok := gatewayType.MethodByName(name)
			require.False(t, ok, "%s 不得再暴露旧采集写入器注入点：%s", gatewayType, name)
		}
		_, ok := gatewayType.MethodByName("PrepareRequestAudit")
		require.True(t, ok, "阳性对照：%s 仍应暴露强制审计发送前门禁", gatewayType)
	}

	// 字段比方法更隐蔽：留着字段就能被再次装配进网关图。
	settingTypeValue := reflect.TypeOf(SettingService{})
	settingFieldCount := 0
	traceSupportFieldSeen := false
	for _, structType := range []reflect.Type{
		settingTypeValue,
		reflect.TypeOf(GatewayService{}),
		reflect.TypeOf(OpenAIGatewayService{}),
	} {
		for i := 0; i < structType.NumField(); i++ {
			field := structType.Field(i)
			lower := strings.ToLower(field.Name)
			for _, marker := range legacyRetirementFieldMarkers {
				require.NotContains(t, lower, marker,
					"%s 不得保留旧采集字段：%s", structType, field.Name)
			}
			if structType == settingTypeValue {
				settingFieldCount++
				if strings.Contains(lower, "requesttracesupport") {
					traceSupportFieldSeen = true
				}
			}
		}
	}
	// 阳性对照：字段枚举必须真的看到未导出字段，否则上面的「没找到旧字段」可能只是空集。
	require.Positive(t, settingFieldCount, "阳性对照：应当枚举到 SettingService 的字段")
	require.True(t, traceSupportFieldSeen,
		"阳性对照：应当枚举到保留的 requestTraceSupportCache 字段")

	// 名字可以改，类型改不了：只要某个字段或某个导出方法的入参／返回值仍然是旧采集类型，
	// 那条接缝就还在。反射查不到「某个类型是否存在」，但能查到「谁还在用它」。
	legacyTypeMarkers := []string{"ErrorDiagnostic", "ValueDetail"}
	typeHolders := []reflect.Type{
		reflect.TypeOf(SettingService{}),
		reflect.TypeOf(GatewayService{}),
		reflect.TypeOf(OpenAIGatewayService{}),
	}
	typeSightings := 0
	for _, structType := range typeHolders {
		for i := 0; i < structType.NumField(); i++ {
			field := structType.Field(i)
			typeSightings++
			for _, marker := range legacyTypeMarkers {
				require.NotContains(t, field.Type.String(), marker,
					"%s 的字段 %s 不得仍然持有旧采集类型", structType, field.Name)
			}
		}
	}
	for _, methodHolder := range []reflect.Type{
		reflect.TypeOf(&SettingService{}),
		reflect.TypeOf(&GatewayService{}),
		reflect.TypeOf(&OpenAIGatewayService{}),
	} {
		for i := 0; i < methodHolder.NumMethod(); i++ {
			method := methodHolder.Method(i)
			typeSightings++
			for j := 0; j < method.Type.NumIn(); j++ {
				for _, marker := range legacyTypeMarkers {
					require.NotContains(t, method.Type.In(j).String(), marker,
						"%s.%s 的入参不得仍然是旧采集类型", methodHolder, method.Name)
				}
			}
			for j := 0; j < method.Type.NumOut(); j++ {
				for _, marker := range legacyTypeMarkers {
					require.NotContains(t, method.Type.Out(j).String(), marker,
						"%s.%s 的返回值不得仍然是旧采集类型", methodHolder, method.Name)
				}
			}
		}
	}
	require.Positive(t, typeSightings, "阳性对照：确实遍历到了字段与方法签名")
}

// TestLegacySensitiveCaptureRetentionSeamsCannotWriteNewRecords 锁定退役后剩下的存储接口。
//
// 对两张旧表各自的「留存契约」做**精确方法集**比对：精确集比「名字不含 create」更强——
// 只要有人往接口上加回任何写入或揭示方法（无论叫什么名字），本用例都会失败。
// 这正好对应验收判据「不再能产生新的 request_audit_value_details／error_diagnostic_records 写入」：
// 服务层唯一能触达这两张表的入口就是这两个接口。
func TestLegacySensitiveCaptureRetentionSeamsCannotWriteNewRecords(t *testing.T) {
	require.ElementsMatch(t,
		[]string{"ClearExpiredRequestAuditValueDetails"},
		legacyRetirementMethodNames(reflect.TypeOf((*RequestAuditValueDetailRepository)(nil)).Elem()),
		"值明细存储只应剩下到期物理清除")

	require.ElementsMatch(t,
		[]string{
			"ClearExpiredErrorDiagnosticBodies",
			"ClearExpiredErrorDiagnosticHeaderValues",
			"DeleteExpiredErrorDiagnostics",
		},
		legacyRetirementMethodNames(reflect.TypeOf((*ErrorDiagnosticRepository)(nil)).Elem()),
		"错误诊断存储只应剩下到期物理清除")

	// 可选能力（明文载荷清理、补关联、积压观测）同样不得携带「新建一行」的动作。
	optionalInterfaces := []struct {
		name string
		typ  reflect.Type
	}{
		{"ErrorDiagnosticPlaintextReader", reflect.TypeOf((*ErrorDiagnosticPlaintextReader)(nil)).Elem()},
		{"ErrorDiagnosticLinkReconciler", reflect.TypeOf((*ErrorDiagnosticLinkReconciler)(nil)).Elem()},
		{"ErrorDiagnosticCleanupBacklogReader", reflect.TypeOf((*ErrorDiagnosticCleanupBacklogReader)(nil)).Elem()},
	}
	writeActionMarkers := []string{"create", "insert", "upsert", "reveal", "store", "persist"}
	for _, iface := range optionalInterfaces {
		for _, method := range legacyRetirementMethodNames(iface.typ) {
			lower := strings.ToLower(method)
			for _, marker := range writeActionMarkers {
				require.NotContains(t, lower, marker,
					"%s 不得保留写入／揭示动作：%s", iface.name, method)
			}
		}
	}
}

// legacyRetirementSettingRepo 是只服务本文件的内存设置仓储，可按需让批量读取失败。
// 复用既有替身无法注入 GetMultiple 级故障，因此单独定义（名字带前缀避免与既有测试冲突）。
type legacyRetirementSettingRepo struct {
	values      map[string]string
	multipleErr error
}

func (r *legacyRetirementSettingRepo) Get(ctx context.Context, key string) (*Setting, error) {
	if value, ok := r.values[key]; ok {
		return &Setting{Key: key, Value: value}, nil
	}
	return nil, ErrSettingNotFound
}

func (r *legacyRetirementSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	setting, err := r.Get(ctx, key)
	if err != nil {
		return "", err
	}
	return setting.Value, nil
}

func (r *legacyRetirementSettingRepo) Set(ctx context.Context, key, value string) error {
	return r.SetMultiple(ctx, map[string]string{key: value})
}

func (r *legacyRetirementSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	if r.multipleErr != nil {
		return nil, r.multipleErr
	}
	out := map[string]string{}
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *legacyRetirementSettingRepo) SetMultiple(_ context.Context, settings map[string]string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	for key, value := range settings {
		r.values[key] = value
	}
	return nil
}

func (r *legacyRetirementSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return nil, nil
}

func (r *legacyRetirementSettingRepo) Delete(context.Context, string) error { return nil }

// TestForcedRequestAuditGateFailsClosedWithoutTheTraceSwitch 锁定强制审计的发送前门禁：
// 它是既有运维面的独立能力，新 Trace 开关关闭（默认态）不改变它，且它的失败一律 fail closed。
//
// 「阻断发送」在本层的形态是 PrepareRequestAudit 返回 *httpattempt.RequiredAuditError：
// 处理器侧由 handler.prepareRequestAuditOrReject 转成 503 并放弃上游调用
// （见 backend/internal/handler/request_audit_forced.go 及其单测），因此本用例只断言
// 「门禁返回了必须阻断的错误」，不去重复那条处理器层用例。
func TestForcedRequestAuditGateFailsClosedWithoutTheTraceSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newChatCompletionsContext := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		return c
	}

	t.Run("force settings probe failure blocks the attempt", func(t *testing.T) {
		settings := NewSettingService(&legacyRetirementSettingRepo{
			values:      map[string]string{},
			multipleErr: errors.New("settings store down"),
		}, &config.Config{})
		svc := &OpenAIGatewayService{settingService: settings, requestAuditRepo: &forcedAuditRepoStub{}}
		c := newChatCompletionsContext()

		require.False(t, settings.RequestTraceGate(c.Request.Context()).CaptureAllowed,
			"新 Trace 默认关闭是本用例的前提")

		_, err := svc.prepareForcedRequestAudit(c.Request.Context(), c, RequestAuditRouteChatCompletions)
		require.Error(t, err, "读不到强制审计开关时必须阻断，而不是当作「未开启」放行")
		require.True(t, IsRequestAuditRequiredError(err),
			"必须是「必须审计」类错误，处理器才会以 503 拒绝并放弃上游调用")
		var required *httpattempt.RequiredAuditError
		require.ErrorAs(t, err, &required)
	})

	t.Run("readable settings that enable nothing still allow the attempt", func(t *testing.T) {
		settings := NewSettingService(&legacyRetirementSettingRepo{values: map[string]string{}}, &config.Config{})
		svc := &OpenAIGatewayService{settingService: settings, requestAuditRepo: &forcedAuditRepoStub{}}
		c := newChatCompletionsContext()

		ctx, err := svc.prepareForcedRequestAudit(c.Request.Context(), c, RequestAuditRouteChatCompletions)
		require.NoError(t, err, "对照组：能读到设置且未开启强制审计时不得阻断")
		require.False(t, RequestAuditForcedFromContext(ctx))
		// 未开启强制审计时不得留下去写审计行的前置回调。
		require.NoError(t, httpattempt.Before(httpattempt.WithMetadata(ctx, httpattempt.Metadata{})))
		require.False(t, c.Writer.Written())
	})

	t.Run("forced reservation failure blocks before any response byte", func(t *testing.T) {
		settings := NewSettingService(&legacyRetirementSettingRepo{values: map[string]string{
			SettingKeyRequestAuditForceChatCompletions: "true",
		}}, &config.Config{})
		repo := &forcedAuditRepoStub{err: errors.New("audit store down")}
		svc := &OpenAIGatewayService{settingService: settings, requestAuditRepo: repo}
		c := newChatCompletionsContext()

		require.False(t, settings.RequestTraceGate(c.Request.Context()).CaptureAllowed,
			"Trace 开关关闭不得关掉强制审计")

		ctx, err := svc.prepareForcedRequestAudit(c.Request.Context(), c, RequestAuditRouteChatCompletions)
		require.NoError(t, err)
		require.True(t, RequestAuditForcedFromContext(ctx), "该路由族被要求强制审计")

		ctx = httpattempt.WithMetadata(ctx, httpattempt.Metadata{
			AccountID: 1, Protocol: RequestAuditProtocolOpenAIResp,
		})
		require.False(t, c.Writer.Written(), "此时还没有写出任何响应字节，属于发送前")
		require.True(t, IsRequestAuditRequiredError(httpattempt.Before(ctx)),
			"发送前的预留失败必须阻断发送")
		require.Len(t, repo.reserved, 1, "预留确实被尝试过：阻断不是因为回调没挂上")
		require.True(t, repo.scopes[0].Forced)
		require.False(t, c.Writer.Written(), "阻断必须发生在写出任何响应字节之前")
	})
}
