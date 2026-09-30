//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceGateRequiresNewWrittenRiskAndDeploymentSupport(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{
		"request_audit_value_detail_settings": `{"enabled":true,"risk_acknowledged":true}`,
	}}
	svc := NewSettingService(repo, &config.Config{})
	require.False(t, svc.RequestTraceGate(ctx).CaptureAllowed, "old switches must not authorize a trace")

	settings, err := json.Marshal(RequestTraceSettings{Enabled: true, RiskAcknowledged: true})
	require.NoError(t, err)
	repo.values[SettingKeyRequestTrace] = string(settings)
	require.False(t, svc.RequestTraceGate(ctx).CaptureAllowed, "a boolean switch alone is insufficient")

	status, err := svc.UpdateRequestTraceOperatorSettings(ctx, RequestTraceOperatorUpdateInput{
		Enabled: true, AdminUserID: 7, Language: "zh", Phrase: RequestTraceRiskAcknowledgementPhraseZH,
	})
	require.ErrorIs(t, err, ErrRequestTraceDeploymentUnsupported, "the trace table is not yet deployed")
	require.False(t, status.CaptureAllowed)
	require.False(t, svc.RequestTraceGate(ctx).CaptureAllowed)
}

type traceSupportProbeStub struct {
	support PlaintextCaptureSupport
	calls   int
}

func (p *traceSupportProbeStub) ProbeRequestTraceSupport(context.Context) (PlaintextCaptureSupport, error) {
	p.calls++
	return p.support, nil
}

func TestRequestTraceWrittenAcknowledgementRequiresCurrentVerbatimStatement(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})
	probe := &traceSupportProbeStub{support: PlaintextCaptureSupport{Supported: true, Reason: PlaintextCaptureSupportReasonSupported}}
	svc.SetRequestTraceSupportProbe(probe)

	status, err := svc.UpdateRequestTraceOperatorSettings(ctx, RequestTraceOperatorUpdateInput{
		Enabled: true, AdminUserID: 7, Language: "en", Phrase: RequestTraceRiskAcknowledgementPhraseEN,
	})
	require.NoError(t, err)
	require.True(t, status.CaptureAllowed)
	require.True(t, svc.RequestTraceGate(ctx).CaptureAllowed)
	require.Positive(t, probe.calls)

	ack := RequestTraceRiskAcknowledgement{
		Version:     RequestTraceRiskAcknowledgementVersion,
		Phrase:      "not the statement",
		AdminUserID: 7,
		AcceptedAt:  time.Now().UTC(),
	}
	payload, err := json.Marshal(ack)
	require.NoError(t, err)
	repo.values[SettingKeyRequestTraceRiskAcknowledgement] = string(payload)
	// 直接用夹具改库等价于"另一个实例写入了坏值"：本实例要重新求值才能看到。
	svc.invalidateRequestTraceGate()
	require.False(t, svc.RequestTraceGate(ctx).CaptureAllowed)

	ack.Phrase = RequestTraceRiskAcknowledgementPhraseEN
	ack.Version = "obsolete"
	payload, err = json.Marshal(ack)
	require.NoError(t, err)
	repo.values[SettingKeyRequestTraceRiskAcknowledgement] = string(payload)
	svc.invalidateRequestTraceGate()
	require.False(t, svc.RequestTraceGate(ctx).CaptureAllowed)
}

type traceProbeErrorStub struct{}

func (traceProbeErrorStub) ProbeRequestTraceSupport(context.Context) (PlaintextCaptureSupport, error) {
	return PlaintextCaptureSupport{}, errors.New("database unavailable")
}

func TestRequestTraceSupportExplainsProbeFailure(t *testing.T) {
	svc := NewSettingService(&traceSettingRepoStub{}, &config.Config{})
	status, err := svc.GetRequestTraceOperatorStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, PlaintextCaptureSupportReasonProbeUnavailable, status.PlaintextCaptureSupportReason)
	_, err = svc.UpdateRequestTraceOperatorSettings(context.Background(), RequestTraceOperatorUpdateInput{
		Enabled: true, AdminUserID: 7, Phrase: RequestTraceRiskAcknowledgementPhraseEN,
	})
	require.ErrorIs(t, err, ErrRequestTraceDeploymentUnsupported)
	require.Equal(t, PlaintextCaptureSupportReasonProbeUnavailable, svc.requestTraceSupportFresh(context.Background()).Reason)

	svc.SetRequestTraceSupportProbe(traceProbeErrorStub{})
	_, err = svc.UpdateRequestTraceOperatorSettings(context.Background(), RequestTraceOperatorUpdateInput{
		Enabled: true, AdminUserID: 7, Phrase: RequestTraceRiskAcknowledgementPhraseEN,
	})
	require.ErrorIs(t, err, ErrRequestTraceDeploymentUnsupported)
	status, err = svc.GetRequestTraceOperatorStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, PlaintextCaptureSupportReasonProbeFailed, status.PlaintextCaptureSupportReason)
}

func TestRequestTraceDisableSucceedsWithCorruptAcknowledgement(t *testing.T) {
	repo := &traceSettingRepoStub{values: map[string]string{
		SettingKeyRequestTrace:                    `{"enabled":true,"risk_acknowledged":true}`,
		SettingKeyRequestTraceRiskAcknowledgement: `this is not JSON`,
	}}
	svc := NewSettingService(repo, &config.Config{})
	status, err := svc.UpdateRequestTraceOperatorSettings(context.Background(), RequestTraceOperatorUpdateInput{})
	require.NoError(t, err)
	require.False(t, status.Enabled)
	require.False(t, status.CaptureAllowed)
	stored, err := svc.readRequestTraceSettings(context.Background())
	require.NoError(t, err)
	require.False(t, stored.Enabled)
}

func TestRequestTraceSettingsAllowEmergencyDisableWithoutRiskOrProbe(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})

	_, err := svc.UpdateRequestTraceOperatorSettings(ctx, RequestTraceOperatorUpdateInput{
		Enabled: true, AdminUserID: 3, Phrase: "something else",
	})
	require.ErrorIs(t, err, ErrRequestTraceRiskAcknowledgementInvalid)

	repo.values[SettingKeyRequestTrace] = `{"enabled":true,"risk_acknowledged":true}`
	status, err := svc.UpdateRequestTraceOperatorSettings(ctx, RequestTraceOperatorUpdateInput{})
	require.NoError(t, err)
	require.False(t, status.Enabled)
	require.False(t, status.CaptureAllowed)
}

// 票据 10：旧的「三类新明文采集部署探针」随旧值明细／错误诊断采集一起退役。
//
// 退役的判据不是「探针不再被调用」，而是**接缝本身不存在**：SettingService 上不再有旧探针的
// 字段与方法，装配构造器也不再要求旧探针参数。只要还留着任何一条接缝，后来者就能再一次
// 把旧采集接回门控——这正是本票据要禁止的形态。Trace 只保留自己的 RequestTraceSupportProbe。
//
// 用反射而不是「引用不存在的符号」来断言：引用会直接编译失败，本用例也就无法在退役前
// 以失败（红）的形式证明自己确实在盯着这件事。
func TestLegacyPlaintextCaptureSupportProbeIsRetired(t *testing.T) {
	svcType := reflect.TypeOf(SettingService{})
	for i := 0; i < svcType.NumField(); i++ {
		name := svcType.Field(i).Name
		require.NotContains(t, strings.ToLower(name), "plaintextcapturesupport",
			"旧探针的字段不得留在 SettingService 上：%s", name)
	}
	for _, method := range []string{"PlaintextCaptureSupport", "SetPlaintextCaptureSupportProbe"} {
		_, ok := reflect.TypeOf(&SettingService{}).MethodByName(method)
		require.False(t, ok, "旧探针的方法不得保留：%s", method)
	}

	providerType := reflect.TypeOf(ProvideSettingService)
	traceProbeType := reflect.TypeOf((*RequestTraceSupportProbe)(nil)).Elem()
	traceProbes := 0
	for i := 0; i < providerType.NumIn(); i++ {
		param := providerType.In(i)
		require.NotContains(t, param.String(), "PlaintextCaptureSupportProbe",
			"装配构造器不得再要求旧探针：第 %d 个参数是 %s", i, param)
		if param == traceProbeType {
			traceProbes++
		}
	}
	require.Equal(t, 1, traceProbes, "装配构造器必须且只能注入 Trace 自己的探针")
}

// TestRequestTraceFreshSupportComesFromItsOwnProbe 锁定 Trace 的支持结论只来自它自己的探针，
// 与已退役的旧探针无关，并且新鲜结论同样收敛到闭集原因码（闭集外的形态不得泄进运维状态）。
func TestRequestTraceFreshSupportComesFromItsOwnProbe(t *testing.T) {
	ctx := context.Background()
	svc := NewSettingService(&traceSettingRepoStub{}, &config.Config{})
	require.Equal(t, PlaintextCaptureSupportReasonProbeUnavailable, svc.requestTraceSupportFresh(ctx).Reason,
		"没有 Trace 探针时不得给出任何「支持」结论")

	probe := &traceSupportProbeStub{support: PlaintextCaptureSupport{Reason: "unrecognized_shape"}}
	svc.SetRequestTraceSupportProbe(probe)
	fresh := svc.requestTraceSupportFresh(ctx)
	require.False(t, fresh.Supported)
	require.Equal(t, PlaintextCaptureSupportReasonUnknownDeployment, fresh.Reason)
	require.Equal(t, 1, probe.calls)

	status, err := svc.GetRequestTraceOperatorStatus(ctx)
	require.NoError(t, err)
	require.False(t, status.PlaintextCaptureSupported)
	require.Equal(t, PlaintextCaptureSupportReasonUnknownDeployment, status.PlaintextCaptureSupportReason,
		"运维状态必须与采集侧同一套收敛，不得回显探针自报的原始原因码")
}

// 关闭是默认状态，也是绝大多数部署的常态：热路径门控不得每个请求都回读设置表，
// 否则每条推理请求都要多付一次 SELECT（仓库既有的面板限流门控就是带缓存的）。
func TestRequestTraceGateCachesTheDisabledState(t *testing.T) {
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})

	require.False(t, svc.RequestTraceGate(context.Background()).CaptureAllowed)
	reads := repo.getValueCalls
	require.Positive(t, reads, "the first decision must consult the stored setting")
	for i := 0; i < 25; i++ {
		require.False(t, svc.RequestTraceGate(context.Background()).CaptureAllowed)
	}
	require.Equal(t, reads, repo.getValueCalls,
		"a disabled gate must not re-read the settings table on every request")
}

// 运维点开启后必须立即生效，不能等缓存过期才开采集。
func TestRequestTraceGateWriteTakesEffectImmediately(t *testing.T) {
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})
	svc.SetRequestTraceSupportProbe(&traceSupportProbeStub{
		support: PlaintextCaptureSupport{Supported: true, Reason: PlaintextCaptureSupportReasonSupported},
	})
	require.False(t, svc.RequestTraceGate(context.Background()).CaptureAllowed)

	_, err := svc.UpdateRequestTraceOperatorSettings(context.Background(), RequestTraceOperatorUpdateInput{
		Enabled: true, AdminUserID: 7, Language: "zh", Phrase: RequestTraceRiskAcknowledgementPhraseZH,
	})
	require.NoError(t, err)
	require.True(t, svc.RequestTraceGate(context.Background()).CaptureAllowed,
		"the write must invalidate the cached disabled state")

	_, err = svc.UpdateRequestTraceOperatorSettings(context.Background(), RequestTraceOperatorUpdateInput{Enabled: false})
	require.NoError(t, err)
	require.False(t, svc.RequestTraceGate(context.Background()).CaptureAllowed,
		"emergency disable must invalidate an enabled decision immediately")
}
