//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
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

// 票据 05/06：**提交**侧的范围必须自带可用的选定值。
//
// 读取侧的归一化会把手滑悄悄抹平：`"include"` + 空列表会被原样存下，门控随后对任何
// 请求都不命中——采集开关仍显示"已开启"，实际却什么都不采；未知取值落回 `all` 则会
// 把一次限制悄悄放宽成采集全部。两种都不是"拒绝"，所以保存接缝必须显式报错，
// 并且一个字节都不写。
func TestRequestTraceScopeSaveRejectsEmptyOrInvalidSelections(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		input RequestTraceOperatorUpdateInput
		want  error
	}{
		{
			name: "model include without models",
			input: RequestTraceOperatorUpdateInput{
				ScopeProvided: true, AllGroups: true, ModelScope: RequestTraceScopeInclude,
			},
			want: ErrRequestTraceScopeListEmpty,
		},
		{
			name: "model exclude without models",
			input: RequestTraceOperatorUpdateInput{
				ScopeProvided: true, AllGroups: true, ModelScope: RequestTraceScopeExclude, Models: []string{},
			},
			want: ErrRequestTraceScopeListEmpty,
		},
		{
			name: "model include with blank entries only",
			input: RequestTraceOperatorUpdateInput{
				ScopeProvided: true, AllGroups: true, ModelScope: RequestTraceScopeInclude, Models: []string{"", "  ", "\t"},
			},
			want: ErrRequestTraceScopeEntryInvalid,
		},
		{
			name: "model exclude with a blank entry",
			input: RequestTraceOperatorUpdateInput{
				ScopeProvided: true, AllGroups: true, ModelScope: RequestTraceScopeExclude, Models: []string{"gpt-5", " "},
			},
			want: ErrRequestTraceScopeEntryInvalid,
		},
		{
			name: "platform include without platforms",
			input: RequestTraceOperatorUpdateInput{
				ScopeProvided: true, AllGroups: true, ModelScope: RequestTraceScopeAll, PlatformScope: RequestTraceScopeInclude,
			},
			want: ErrRequestTraceScopeListEmpty,
		},
		{
			name: "platform exclude with blank entries only",
			input: RequestTraceOperatorUpdateInput{
				ScopeProvided: true, AllGroups: true, ModelScope: RequestTraceScopeAll,
				PlatformScope: RequestTraceScopeExclude, Platforms: []string{"  "},
			},
			want: ErrRequestTraceScopeEntryInvalid,
		},
		{
			name: "unknown model scope kind must not widen to all",
			input: RequestTraceOperatorUpdateInput{
				ScopeProvided: true, AllGroups: true, ModelScope: "only", Models: []string{"gpt-5"},
			},
			want: ErrRequestTraceScopeKindInvalid,
		},
		{
			name: "unknown platform scope kind must not widen to all",
			input: RequestTraceOperatorUpdateInput{
				ScopeProvided: true, AllGroups: true, ModelScope: RequestTraceScopeAll,
				PlatformScope: "none", Platforms: []string{"anthropic"},
			},
			want: ErrRequestTraceScopeKindInvalid,
		},
		{
			name: "selected groups without group ids",
			input: RequestTraceOperatorUpdateInput{
				ScopeProvided: true, AllGroups: false, GroupIDs: nil,
				ModelScope: RequestTraceScopeAll, PlatformScope: RequestTraceScopeAll,
			},
			want: ErrRequestTraceScopeListEmpty,
		},
		{
			name: "selected groups with a non positive id",
			input: RequestTraceOperatorUpdateInput{
				ScopeProvided: true, AllGroups: false, GroupIDs: []int64{0, 7},
				ModelScope: RequestTraceScopeAll, PlatformScope: RequestTraceScopeAll,
			},
			want: ErrRequestTraceGroupScopeEntryInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &traceSettingRepoStub{values: map[string]string{}}
			svc := NewSettingService(repo, &config.Config{})
			_, err := svc.UpdateRequestTraceOperatorSettings(ctx, tc.input)
			require.ErrorIs(t, err, tc.want)
			require.NotContains(t, repo.values, SettingKeyRequestTrace,
				"a rejected scope submission must not be persisted")
			require.NotContains(t, repo.values, SettingKeyRequestTraceRiskAcknowledgement,
				"a rejected scope submission must not be persisted")
		})
	}
}

// 校验只针对**本次提交**的范围：合法的收窄照常保存，并沿用既有归一化口径
// （去空白、去重、保留操作者自己的大小写）。
func TestRequestTraceScopeSaveAcceptsValidNarrowedSelections(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})
	status, err := svc.UpdateRequestTraceOperatorSettings(ctx, RequestTraceOperatorUpdateInput{
		ScopeProvided: true, AllGroups: false, GroupIDs: []int64{7, 7, 8},
		ModelScope: RequestTraceScopeInclude, Models: []string{" gpt-5 ", "GPT-5"},
		PlatformScope: RequestTraceScopeExclude, Platforms: []string{"anthropic"},
	})
	require.NoError(t, err)
	require.False(t, status.AllGroups)
	require.Equal(t, []int64{7, 8}, status.GroupIDs)
	require.Equal(t, RequestTraceScopeInclude, status.ModelScope)
	require.Equal(t, []string{"gpt-5"}, status.Models)
	require.Equal(t, RequestTraceScopeExclude, status.PlatformScope)
	require.Equal(t, []string{"anthropic"}, status.Platforms)

	stored, err := svc.readRequestTraceSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, status.Models, stored.Models)
	require.Equal(t, status.Platforms, stored.Platforms)

	// "全部"仍然不需要列表，且是最省事的缺省。
	status, err = svc.UpdateRequestTraceOperatorSettings(ctx, RequestTraceOperatorUpdateInput{
		ScopeProvided: true, AllGroups: true, ModelScope: "all", PlatformScope: "ALL",
	})
	require.NoError(t, err)
	require.True(t, status.AllGroups)
	require.Equal(t, RequestTraceScopeAll, status.ModelScope, "取值大小写仍按既有口径归一")
	require.Equal(t, RequestTraceScopeAll, status.PlatformScope)
	require.Empty(t, status.Models)
	require.Empty(t, status.Platforms)
}

// 历史配置可能带着旧版本写下的"指定但为空"的范围。读取语义不得被新的提交侧校验追认成非法：
// 旧记录照原样可读（门控仍按既有语义不采集），未提交范围的紧急关闭也不得被卡住或改写范围。
func TestRequestTraceLegacyEmptyNarrowedScopeStaysReadable(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{
		SettingKeyRequestTrace: `{"enabled":true,"risk_acknowledged":true,"all_groups":true,"model_scope":"include","models":[]}`,
	}}
	svc := NewSettingService(repo, &config.Config{})

	stored, err := svc.readRequestTraceSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, RequestTraceScopeInclude, stored.ModelScope)
	require.Empty(t, stored.Models, "旧记录按读取口径原样保留，不被回填或改写")

	status, err := svc.UpdateRequestTraceOperatorSettings(ctx, RequestTraceOperatorUpdateInput{Enabled: false})
	require.NoError(t, err, "紧急关闭不得被新增的范围校验卡住")
	require.False(t, status.Enabled)
	require.Equal(t, RequestTraceScopeInclude, status.ModelScope, "未提交范围时保留既有范围")
	require.Empty(t, status.Models)
}

// 规格 2.2「缺省均为全部」：本版本之前写下的旧配置只有
// `{"enabled":true,"risk_acknowledged":true}`，没有任何范围键。
//
// bool 与空切片的 Go 零值恰好是"指定分组、且一个分组都没选"——读取侧最严格的范围，
// 对任何请求都不命中。若把它当缺省，升级后 all_groups 会被读成 false：采集在管理员
// 什么都没改的情况下静默停止，界面也看不出范围被收窄。范围缺省必须按"键是否存在"判定：
// 键缺失 = 该维度的缺省（全部/所有），只有键确实出现过的显式选择才覆盖缺省。
func TestRequestTraceLegacySettingsWithoutScopeKeysDefaultToAll(t *testing.T) {
	ctx := context.Background()
	ack, err := json.Marshal(RequestTraceRiskAcknowledgement{
		Version:     RequestTraceRiskAcknowledgementVersion,
		Phrase:      RequestTraceRiskAcknowledgementPhraseEN,
		AdminUserID: 7,
		AcceptedAt:  time.Now().UTC(),
	})
	require.NoError(t, err)

	repo := &traceSettingRepoStub{values: map[string]string{
		SettingKeyRequestTrace:                    `{"enabled":true,"risk_acknowledged":true}`,
		SettingKeyRequestTraceRiskAcknowledgement: string(ack),
	}}
	svc := NewSettingService(repo, &config.Config{})
	svc.SetRequestTraceSupportProbe(&traceSupportProbeStub{
		support: PlaintextCaptureSupport{Supported: true, Reason: PlaintextCaptureSupportReasonSupported},
	})

	stored, err := svc.readRequestTraceSettings(ctx)
	require.NoError(t, err)
	require.True(t, stored.AllGroups, "缺少 all_groups 键的旧配置必须缺省为全部分组")
	require.Equal(t, RequestTraceScopeAll, stored.ModelScope, "缺少 model_scope 键时缺省为所有模型")
	require.Equal(t, RequestTraceScopeAll, stored.PlatformScope, "缺少 platform_scope 键时缺省为所有平台")

	gate := svc.RequestTraceGate(ctx)
	require.True(t, gate.CaptureAllowed, "旧配置的开关与确认必须继续授权采集")

	groupID := int64(4242)
	require.True(t, gate.Scope.InScope(RequestTraceScopeFacts{
		GroupID: &groupID, RequestedModel: "claude-sonnet-4-5", Platforms: []string{"anthropic"},
	}), "缺省范围必须覆盖具体的分组/模型/平台")
	require.True(t, gate.Scope.InScope(RequestTraceScopeFacts{}),
		"三个维度缺省均为全部时，观察不到分组/模型/平台的请求同样保留采集")
}

// 旧设置未存采集范围时，服务端虽按"全部"执行，也必须把范围列表
// 编码成数组而不是 null；否则管理端会把已读到的状态误判成不可读。
func TestRequestTraceOperatorStatusLegacyScopeUsesArraysOnWire(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{
		SettingKeyRequestTrace: `{"enabled":true,"risk_acknowledged":true}`,
	}}
	svc := NewSettingService(repo, &config.Config{})
	for _, tc := range []struct {
		name string
		read func() (RequestTraceOperatorStatus, error)
	}{
		{"read", func() (RequestTraceOperatorStatus, error) { return svc.GetRequestTraceOperatorStatus(ctx) }},
		{"emergency disable", func() (RequestTraceOperatorStatus, error) {
			return svc.UpdateRequestTraceOperatorSettings(ctx, RequestTraceOperatorUpdateInput{Enabled: false})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, err := tc.read()
			require.NoError(t, err)
			require.True(t, status.AllGroups)
			require.Equal(t, RequestTraceScopeAll, status.ModelScope)
			require.Equal(t, RequestTraceScopeAll, status.PlatformScope)
			encoded, err := json.Marshal(status)
			require.NoError(t, err)
			var wire map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(encoded, &wire))
			for _, name := range []string{"group_ids", "models", "platforms"} {
				require.JSONEq(t, `[]`, string(wire[name]), "%s must be an empty array", name)
			}
		})
	}
}

// 从未存过设置（采集从未开启）时，读取侧同样给出"关闭 + 范围全部"的缺省。
// 管理端状态必须回显完整的缺省范围，而不是把零值当成"指定分组但为空"。
func TestRequestTraceMissingSettingDefaultsToFullScopeWhileDisabled(t *testing.T) {
	ctx := context.Background()
	svc := NewSettingService(&traceSettingRepoStub{values: map[string]string{}}, &config.Config{})

	status, err := svc.GetRequestTraceOperatorStatus(ctx)
	require.NoError(t, err)
	require.False(t, status.Enabled)
	require.False(t, status.CaptureAllowed)
	require.True(t, status.AllGroups, "缺省范围必须是全部分组")
	require.Empty(t, status.GroupIDs)
	require.Equal(t, RequestTraceScopeAll, status.ModelScope)
	require.Equal(t, RequestTraceScopeAll, status.PlatformScope)
	require.Empty(t, status.Models)
	require.Empty(t, status.Platforms)
}

// 显式选择的范围不得被缺省改写：`all_groups:false` 配非空 group_ids 是管理员真实意图，
// 升级后必须继续只采集这些分组，既不放宽成全部，也不被清空。
func TestRequestTraceExplicitNarrowedGroupScopeIsNotWidened(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{
		SettingKeyRequestTrace: `{"enabled":true,"risk_acknowledged":true,"all_groups":false,"group_ids":[7],"model_scope":"all","platform_scope":"all"}`,
	}}
	svc := NewSettingService(repo, &config.Config{})

	stored, err := svc.readRequestTraceSettings(ctx)
	require.NoError(t, err)
	require.False(t, stored.AllGroups, "显式的 all_groups:false 不得被缺省放宽")
	require.Equal(t, []int64{7}, stored.GroupIDs)

	seven, eight := int64(7), int64(8)
	require.True(t, stored.InGroupScope(&seven))
	require.False(t, stored.InGroupScope(&eight))
	require.False(t, stored.InGroupScope(nil), "指定分组时无法确定分组不采集")
}

// 显式写下的 `all_groups:false` 配空 group_ids 是无效的历史范围：它表达"指定分组"，
// 却没有列出任何分组。读取侧必须按原样保持"谁都不命中"（失败关闭），不能因为列表为空
// 就当作"没配范围"放宽成全部——那是把一次限制悄悄抹掉。
func TestRequestTraceExplicitEmptyGroupScopeFailsClosed(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{
		SettingKeyRequestTrace: `{"enabled":true,"risk_acknowledged":true,"all_groups":false,"group_ids":[]}`,
	}}
	svc := NewSettingService(repo, &config.Config{})

	stored, err := svc.readRequestTraceSettings(ctx)
	require.NoError(t, err)
	require.False(t, stored.AllGroups, "显式的 all_groups:false 不得被放宽成全部分组")
	require.Empty(t, stored.GroupIDs)

	seven := int64(7)
	require.False(t, stored.InGroupScope(nil), "无效的空范围必须什么都不采集")
	require.False(t, stored.InGroupScope(&seven), "无效的空范围必须什么都不采集")
}

// 紧急关闭在未提交范围时必须保留既有范围；旧配置的范围缺省是"全部"，
// 因此关闭不得把范围落成零值（指定分组且为空），否则下一次开启会静默不采集。
func TestRequestTraceEmergencyDisablePreservesDefaultedFullScope(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{
		SettingKeyRequestTrace: `{"enabled":true,"risk_acknowledged":true}`,
	}}
	svc := NewSettingService(repo, &config.Config{})

	status, err := svc.UpdateRequestTraceOperatorSettings(ctx, RequestTraceOperatorUpdateInput{Enabled: false})
	require.NoError(t, err, "紧急关闭不得被范围校验或读取缺省卡住")
	require.False(t, status.Enabled)
	require.False(t, status.CaptureAllowed)
	require.True(t, status.AllGroups, "未提交范围时必须保留缺省的全部分组")
	require.Equal(t, RequestTraceScopeAll, status.ModelScope)
	require.Equal(t, RequestTraceScopeAll, status.PlatformScope)

	stored, err := svc.readRequestTraceSettings(ctx)
	require.NoError(t, err)
	require.False(t, stored.Enabled)
	require.True(t, stored.AllGroups, "关闭后落库的范围仍是全部分组，下一次开启不应静默不采集")
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

func requestTraceBoolPtr(value bool) *bool    { return &value }
func requestTraceIntPtr(value int) *int       { return &value }
func requestTraceInt64Ptr(value int64) *int64 { return &value }

// failingTraceSettingRepo 让设置读取稳定失败，模拟设置表读不可用。
type failingTraceSettingRepo struct{ *traceSettingRepoStub }

func (r failingTraceSettingRepo) GetValue(context.Context, string) (string, error) {
	return "", errors.New("settings store unavailable")
}

func newSupportedTraceSettingService(repo *traceSettingRepoStub) *SettingService {
	svc := NewSettingService(repo, &config.Config{})
	svc.SetRequestTraceSupportProbe(&traceSupportProbeStub{
		support: PlaintextCaptureSupport{Supported: true, Reason: PlaintextCaptureSupportReasonSupported},
	})
	return svc
}

func enableTraceSettings(t *testing.T, svc *SettingService, extra RequestTraceOperatorUpdateInput) RequestTraceOperatorStatus {
	t.Helper()
	extra.Enabled = true
	extra.AdminUserID = 7
	extra.Language = "en"
	extra.Phrase = RequestTraceRiskAcknowledgementPhraseEN
	status, err := svc.UpdateRequestTraceOperatorSettings(context.Background(), extra)
	require.NoError(t, err)
	return status
}

// 本版本之前写下的旧配置没有任何采集内容／限时键。缺字段必须兼容为"两个开关开启、
// 两类采样 100%、1 MiB、持续"，而不是零值（那会读成"正文关闭 + 0% 采样"）。
func TestRequestTraceLegacySettingsDefaultNewCaptureFields(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{
		SettingKeyRequestTrace: `{"enabled":true,"risk_acknowledged":true}`,
	}}
	svc := NewSettingService(repo, &config.Config{})

	stored, err := svc.readRequestTraceSettings(ctx)
	require.NoError(t, err)
	require.True(t, stored.CaptureBody, "缺 capture_body 键必须缺省为开启正文")
	require.True(t, stored.CaptureHTTP200, "缺 capture_http_200 键必须缺省为采集 200")
	require.Equal(t, 100, stored.SampleRateHTTP200)
	require.Equal(t, 100, stored.SampleRateOther)
	require.EqualValues(t, RequestTraceBodyLimit, stored.BodyMaxBytes)
	require.EqualValues(t, 0, stored.CaptureDurationSeconds)
	require.Nil(t, stored.CaptureUntil, "缺省是持续，不应凭空造出截止时间")
}

// 显式 false／0 必须能落库并回读，绝不能被 omitempty 丢掉，也不能在再次读取时
// 被缺省覆盖成开启。
func TestRequestTraceExplicitFalseAndZeroCaptureFieldsPersist(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := newSupportedTraceSettingService(repo)

	status := enableTraceSettings(t, svc, RequestTraceOperatorUpdateInput{
		CaptureBody:       requestTraceBoolPtr(false),
		CaptureHTTP200:    requestTraceBoolPtr(false),
		SampleRateHTTP200: requestTraceIntPtr(0),
		SampleRateOther:   requestTraceIntPtr(0),
	})
	require.False(t, status.CaptureBody)
	require.False(t, status.CaptureHTTP200)
	require.Equal(t, 0, status.SampleRateHTTP200)
	require.Equal(t, 0, status.SampleRateOther)

	raw := repo.values[SettingKeyRequestTrace]
	require.Contains(t, raw, `"capture_body":false`, "显式 false 不得被 omitempty 丢弃")
	require.Contains(t, raw, `"capture_http_200":false`)
	require.Contains(t, raw, `"sample_rate_http_200":0`, "0% 是有效设置，必须落库")

	stored, err := svc.readRequestTraceSettings(ctx)
	require.NoError(t, err)
	require.False(t, stored.CaptureBody)
	require.False(t, stored.CaptureHTTP200)
	require.Zero(t, stored.SampleRateHTTP200)
	require.Zero(t, stored.SampleRateOther)
}

// 旧客户端只改开关／范围时不提交新字段（指针为 nil）：服务端必须保留既有新设置，
// 不得把它们重置成缺省。
func TestRequestTraceUpdateWithoutNewFieldsKeepsExistingCaptureSettings(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := newSupportedTraceSettingService(repo)

	enableTraceSettings(t, svc, RequestTraceOperatorUpdateInput{
		CaptureBody:            requestTraceBoolPtr(false),
		SampleRateOther:        requestTraceIntPtr(25),
		BodyMaxBytes:           requestTraceInt64Ptr(requestTraceBodyMaxBytes256KiB),
		CaptureDurationSeconds: requestTraceInt64Ptr(requestTraceCaptureDuration1Hour),
	})

	// 只提交范围与开关，不带任何新字段。
	status := enableTraceSettings(t, svc, RequestTraceOperatorUpdateInput{
		ScopeProvided: true, AllGroups: false, GroupIDs: []int64{7},
		ModelScope: RequestTraceScopeAll, PlatformScope: RequestTraceScopeAll,
	})
	require.False(t, status.CaptureBody, "未提交 capture_body 时不得被重置成缺省开启")
	require.Equal(t, 25, status.SampleRateOther)
	require.EqualValues(t, requestTraceBodyMaxBytes256KiB, status.BodyMaxBytes)
	require.EqualValues(t, requestTraceCaptureDuration1Hour, status.CaptureDurationSeconds)

	stored, err := svc.readRequestTraceSettings(ctx)
	require.NoError(t, err)
	require.False(t, stored.CaptureBody)
	require.Equal(t, 25, stored.SampleRateOther)
}

// 非法提交必须在写入前整体拒绝：采样率超出 0～100、体积上限不在闭集、时长不在闭集，
// 都不能被静默解释成更宽的缺省。
func TestRequestTraceCaptureSubmissionRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input RequestTraceOperatorUpdateInput
		want  error
	}{
		{"sample rate above max", RequestTraceOperatorUpdateInput{SampleRateHTTP200: requestTraceIntPtr(101)}, ErrRequestTraceSampleRateInvalid},
		{"sample rate below zero", RequestTraceOperatorUpdateInput{SampleRateOther: requestTraceIntPtr(-1)}, ErrRequestTraceSampleRateInvalid},
		{"body size not in closed set", RequestTraceOperatorUpdateInput{BodyMaxBytes: requestTraceInt64Ptr(1 << 19)}, ErrRequestTraceBodyMaxBytesInvalid},
		{"duration not in closed set", RequestTraceOperatorUpdateInput{CaptureDurationSeconds: requestTraceInt64Ptr(120)}, ErrRequestTraceCaptureDurationInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &traceSettingRepoStub{values: map[string]string{}}
			svc := newSupportedTraceSettingService(repo)
			_, err := svc.UpdateRequestTraceOperatorSettings(context.Background(), tc.input)
			require.ErrorIs(t, err, tc.want)
			require.NotContains(t, repo.values, SettingKeyRequestTrace, "非法提交不得落库")
		})
	}
}

// 合法闭集值照常保存。
func TestRequestTraceCaptureSubmissionAcceptsClosedSetValues(t *testing.T) {
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := newSupportedTraceSettingService(repo)
	status := enableTraceSettings(t, svc, RequestTraceOperatorUpdateInput{
		SampleRateHTTP200: requestTraceIntPtr(0), SampleRateOther: requestTraceIntPtr(100),
		BodyMaxBytes:           requestTraceInt64Ptr(requestTraceBodyMaxBytes64KiB),
		CaptureDurationSeconds: requestTraceInt64Ptr(requestTraceCaptureDuration15Min),
	})
	require.Equal(t, 0, status.SampleRateHTTP200)
	require.Equal(t, 100, status.SampleRateOther)
	require.EqualValues(t, requestTraceBodyMaxBytes64KiB, status.BodyMaxBytes)
	require.EqualValues(t, requestTraceCaptureDuration15Min, status.CaptureDurationSeconds)
	require.NotNil(t, status.CaptureUntil, "设置时长后服务端必须算出截止时间")
	require.False(t, status.CaptureExpired)
}

// 限时语义：只有总开关由关到开、明确更改时长、或显式 renew 才重新计时。
func TestRequestTraceCaptureDurationOnlyRenewsOnExplicitAction(t *testing.T) {
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := newSupportedTraceSettingService(repo)

	first := enableTraceSettings(t, svc, RequestTraceOperatorUpdateInput{
		CaptureDurationSeconds: requestTraceInt64Ptr(requestTraceCaptureDuration1Hour),
	})
	require.NotNil(t, first.CaptureUntil)
	firstUntil := *first.CaptureUntil

	// 普通保存：即使再次提交 Enabled=true（已在采集），也不得续期。
	ordinary := enableTraceSettings(t, svc, RequestTraceOperatorUpdateInput{})
	require.NotNil(t, ordinary.CaptureUntil)
	require.True(t, ordinary.CaptureUntil.Equal(firstUntil), "普通保存不得隐式续期")

	// 明确更改时长 → 按当前时间重算。
	changed := enableTraceSettings(t, svc, RequestTraceOperatorUpdateInput{
		CaptureDurationSeconds: requestTraceInt64Ptr(requestTraceCaptureDuration1Day),
	})
	require.NotNil(t, changed.CaptureUntil)
	require.True(t, changed.CaptureUntil.After(firstUntil), "明确改时长必须重算截止")

	// 显式 renew 但时长不变 → 也重算。
	renewed := enableTraceSettings(t, svc, RequestTraceOperatorUpdateInput{RenewCaptureWindow: true})
	require.True(t, renewed.CaptureUntil.After(*changed.CaptureUntil), "显式 renew 必须重算截止")

	// 切换为持续 → 清除截止。
	always := enableTraceSettings(t, svc, RequestTraceOperatorUpdateInput{
		CaptureDurationSeconds: requestTraceInt64Ptr(0),
	})
	require.Nil(t, always.CaptureUntil, "切换为持续必须清除截止")
}

// 已到期的窗口在普通保存时不得重新开始：只有开启／改时长／renew 才允许。
func TestRequestTraceExpiredWindowIsNotRestartedByOrdinarySave(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := newSupportedTraceSettingService(repo)

	enableTraceSettings(t, svc, RequestTraceOperatorUpdateInput{
		CaptureDurationSeconds: requestTraceInt64Ptr(requestTraceCaptureDuration15Min),
	})
	// 直接把存档里的截止时间改到过去，模拟窗口已到期。
	past := time.Now().UTC().Add(-time.Minute)
	raw := repo.values[SettingKeyRequestTrace]
	require.Contains(t, raw, `"capture_until"`)
	stored, err := svc.readRequestTraceSettings(ctx)
	require.NoError(t, err)
	stored.CaptureUntil = &past
	encoded, err := json.Marshal(stored)
	require.NoError(t, err)
	repo.values[SettingKeyRequestTrace] = string(encoded)
	svc.invalidateRequestTraceGate()

	gate := svc.RequestTraceGate(ctx)
	require.False(t, gate.CaptureAllowed, "已到期的窗口必须停止采集")
	require.True(t, gate.Expired)

	status := enableTraceSettings(t, svc, RequestTraceOperatorUpdateInput{})
	require.True(t, status.CaptureExpired, "普通保存不得让到期窗口重新开始")
	require.False(t, status.CaptureAllowed)
	require.NotNil(t, status.CaptureUntil)
	require.True(t, status.CaptureUntil.Equal(past), "普通保存必须保留既有（已到期）截止")
}

// 截止时间只由服务端计算：客户端提交的 input 里没有该字段，状态回显的是服务端值。
func TestRequestTraceCaptureUntilIsServerComputed(t *testing.T) {
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := newSupportedTraceSettingService(repo)
	before := time.Now().UTC()
	status := enableTraceSettings(t, svc, RequestTraceOperatorUpdateInput{
		CaptureDurationSeconds: requestTraceInt64Ptr(requestTraceCaptureDuration1Hour),
	})
	require.NotNil(t, status.CaptureUntil)
	require.False(t, status.CaptureUntil.Before(before), "截止必须不早于服务端当前时间")
	require.WithinDuration(t, before.Add(time.Hour), *status.CaptureUntil, time.Minute)
}

// 缓存路径也必须复核截止：一个在截止前写入的短 TTL 缓存不得把窗口"续"过停采时刻。
func TestRequestTraceGateRechecksDeadlineOnCachedPath(t *testing.T) {
	ctx := context.Background()
	until := time.Now().UTC().Add(60 * time.Millisecond)
	ack, err := json.Marshal(RequestTraceRiskAcknowledgement{
		Version: RequestTraceRiskAcknowledgementVersion, Phrase: RequestTraceRiskAcknowledgementPhraseEN,
		AdminUserID: 7, AcceptedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	settings, err := json.Marshal(RequestTraceSettings{
		Enabled: true, RiskAcknowledged: true,
		CaptureBody: true, CaptureHTTP200: true, SampleRateHTTP200: 100, SampleRateOther: 100,
		BodyMaxBytes: RequestTraceBodyLimit, CaptureDurationSeconds: requestTraceCaptureDuration15Min,
		CaptureUntil: &until, AllGroups: true, ModelScope: RequestTraceScopeAll, PlatformScope: RequestTraceScopeAll,
	})
	require.NoError(t, err)
	repo := &traceSettingRepoStub{values: map[string]string{
		SettingKeyRequestTrace:                    string(settings),
		SettingKeyRequestTraceRiskAcknowledgement: string(ack),
	}}
	svc := newSupportedTraceSettingService(repo)

	require.True(t, svc.RequestTraceGate(ctx).CaptureAllowed, "截止前应允许采集")
	time.Sleep(90 * time.Millisecond)
	gate := svc.RequestTraceGate(ctx) // 仍命中 5 秒缓存
	require.False(t, gate.CaptureAllowed, "命中缓存也必须复核截止时间")
	require.True(t, gate.Expired)
}

// 采样：0% 全跳过、100% 全保留，按客户端最终状态分成两类，且按 Trace ID 稳定。
func TestRequestTraceSamplingIsStableAndStatusAware(t *testing.T) {
	traceID := strings.Repeat("a", 32)

	all := RequestTraceSettings{SampleRateHTTP200: 100, SampleRateOther: 100}
	require.True(t, all.ShouldSampleTrace(traceID, 200))
	require.True(t, all.ShouldSampleTrace(traceID, 201))

	none := RequestTraceSettings{SampleRateHTTP200: 0, SampleRateOther: 0}
	require.False(t, none.ShouldSampleTrace(traceID, 200))
	require.False(t, none.ShouldSampleTrace(traceID, 500))

	// 两类独立：200 全丢、非 200 全留；201／204 属于非 200。
	mixed := RequestTraceSettings{SampleRateHTTP200: 0, SampleRateOther: 100}
	require.False(t, mixed.ShouldSampleTrace(traceID, 200))
	require.True(t, mixed.ShouldSampleTrace(traceID, 201))
	require.True(t, mixed.ShouldSampleTrace(traceID, 204))
	require.True(t, mixed.ShouldSampleTrace(traceID, 404))

	// 稳定：同一 ID 反复判定结果一致。
	half := RequestTraceSettings{SampleRateHTTP200: 50, SampleRateOther: 50}
	decision := half.ShouldSampleTrace(traceID, 200)
	for i := 0; i < 10; i++ {
		require.Equal(t, decision, half.ShouldSampleTrace(traceID, 200))
	}

	// 50% 在大量 ID 上既非全留也非全丢。
	var kept int
	for i := 0; i < 500; i++ {
		if half.ShouldSampleTrace(strconv.Itoa(i), 500) {
			kept++
		}
	}
	require.Positive(t, kept)
	require.Less(t, kept, 500)
}

// 存档里的非法采样率／体积值必须收敛到最保守的一端，绝不因为解析失败而放宽。
func TestRequestTraceNormalizeClampsInvalidCaptureFieldsDown(t *testing.T) {
	normalized := NormalizeRequestTraceSettings(RequestTraceSettings{
		SampleRateHTTP200: 500, SampleRateOther: -3, BodyMaxBytes: 999,
		CaptureDurationSeconds: 7777,
	})
	require.Zero(t, normalized.SampleRateHTTP200)
	require.Zero(t, normalized.SampleRateOther)
	require.EqualValues(t, requestTraceBodyMaxBytes64KiB, normalized.BodyMaxBytes,
		"无法识别的体积上限必须收敛到最小有限值，绝不放宽到 1 MiB")
	require.EqualValues(t, requestTraceCaptureDuration15Min, normalized.CaptureDurationSeconds,
		"无法识别的时长必须收敛到最小有限窗口，不得落回 0=持续")
}

// 有限时长却没有截止时间的畸形存档不得"永不过期"：gate 必须 fail-closed，且状态回显为已到期。
func TestRequestTraceFiniteDurationWithoutDeadlineFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	finiteWithoutDeadline := RequestTraceSettings{
		Enabled: true, RiskAcknowledged: true,
		CaptureBody: true, CaptureHTTP200: true,
		SampleRateHTTP200: 100, SampleRateOther: 100, BodyMaxBytes: RequestTraceBodyLimit,
		CaptureDurationSeconds: requestTraceCaptureDuration1Hour, CaptureUntil: nil,
		AllGroups: true, ModelScope: RequestTraceScopeAll, PlatformScope: RequestTraceScopeAll,
	}
	require.True(t, finiteWithoutDeadline.CaptureWindowExpired(now),
		"有限时长缺截止时间必须视为已到期")

	// 持续（duration<=0）没有截止时间属正常，不能被视为已到期。
	continuous := finiteWithoutDeadline
	continuous.CaptureDurationSeconds = requestTraceCaptureDurationAlways
	require.False(t, continuous.CaptureWindowExpired(now), "持续配置不得被判为到期")
}

// 存档里的非法时长不得把已有截止时间抹掉、变成"持续"：收敛到最小有限窗口并保留
// CaptureUntil，由它继续负责到期停采（fail-closed）。
func TestRequestTraceInvalidStoredDurationKeepsDeadline(t *testing.T) {
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Minute)
	raw, err := json.Marshal(map[string]any{
		"enabled": true, "risk_acknowledged": true,
		"capture_body": true, "capture_http_200": true,
		"sample_rate_http_200": 100, "sample_rate_other": 100,
		"body_max_bytes":           RequestTraceBodyLimit,
		"capture_duration_seconds": 7777,
		"capture_until":            past,
		"all_groups":               true,
		"model_scope":              "all",
		"platform_scope":           "all",
	})
	require.NoError(t, err)
	ack, err := json.Marshal(RequestTraceRiskAcknowledgement{
		Version: RequestTraceRiskAcknowledgementVersion, Phrase: RequestTraceRiskAcknowledgementPhraseEN,
		AdminUserID: 7, AcceptedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	repo := &traceSettingRepoStub{values: map[string]string{
		SettingKeyRequestTrace:                    string(raw),
		SettingKeyRequestTraceRiskAcknowledgement: string(ack),
	}}
	svc := newSupportedTraceSettingService(repo)

	stored, err := svc.readRequestTraceSettings(ctx)
	require.NoError(t, err)
	require.EqualValues(t, requestTraceCaptureDuration15Min, stored.CaptureDurationSeconds)
	require.NotNil(t, stored.CaptureUntil, "非法时长不得抹掉既有截止时间")
	require.True(t, stored.CaptureUntil.Equal(past))

	gate := svc.RequestTraceGate(ctx)
	require.False(t, gate.CaptureAllowed, "已到期的窗口必须 fail-closed")
	require.True(t, gate.Expired)
}

// renew 在关闭状态下必须被拒绝：不能记下一个"待生效"的窗口，也不能开始倒计时。
func TestRequestTraceRenewRejectedWhileDisabled(t *testing.T) {
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := newSupportedTraceSettingService(repo)
	_, err := svc.UpdateRequestTraceOperatorSettings(context.Background(), RequestTraceOperatorUpdateInput{
		Enabled: false, RenewCaptureWindow: true,
	})
	require.ErrorIs(t, err, ErrRequestTraceRenewRequiresEnabled)
	require.NotContains(t, repo.values, SettingKeyRequestTrace, "被拒绝的续期不得落库")
}

// 关闭采集时清除截止时间：关闭状态下不显示、也不倒计时；重新开启会重新计时。
func TestRequestTraceDisableClearsDeadline(t *testing.T) {
	ctx := context.Background()
	repo := &traceSettingRepoStub{values: map[string]string{}}
	svc := newSupportedTraceSettingService(repo)

	enabled := enableTraceSettings(t, svc, RequestTraceOperatorUpdateInput{
		CaptureDurationSeconds: requestTraceInt64Ptr(requestTraceCaptureDuration1Hour),
	})
	require.NotNil(t, enabled.CaptureUntil)

	disabled, err := svc.UpdateRequestTraceOperatorSettings(ctx, RequestTraceOperatorUpdateInput{Enabled: false})
	require.NoError(t, err)
	require.Nil(t, disabled.CaptureUntil, "关闭时必须清除截止时间")
	require.False(t, disabled.CaptureExpired)

	stored, err := svc.readRequestTraceSettings(ctx)
	require.NoError(t, err)
	require.Nil(t, stored.CaptureUntil)

	// 重新开启按当前时间重新计时，而不是恢复关闭前的窗口。
	reenabled := enableTraceSettings(t, svc, RequestTraceOperatorUpdateInput{})
	require.NotNil(t, reenabled.CaptureUntil)
	require.WithinDuration(t, time.Now().UTC().Add(time.Hour), *reenabled.CaptureUntil, time.Minute)
}

// 既有设置读失败时必须 fail-closed：返回不可用且一个字节都不写，绝不回落到"全采"缺省。
func TestRequestTraceUpdateFailsClosedWhenStoredSettingsUnreadable(t *testing.T) {
	repo := &failingTraceSettingRepo{&traceSettingRepoStub{values: map[string]string{}}}
	svc := NewSettingService(repo, &config.Config{})
	svc.SetRequestTraceSupportProbe(&traceSupportProbeStub{
		support: PlaintextCaptureSupport{Supported: true, Reason: PlaintextCaptureSupportReasonSupported},
	})
	for _, tc := range []struct {
		name  string
		input RequestTraceOperatorUpdateInput
	}{
		{"enable", RequestTraceOperatorUpdateInput{Enabled: true, AdminUserID: 7, Phrase: RequestTraceRiskAcknowledgementPhraseEN}},
		{"emergency disable", RequestTraceOperatorUpdateInput{Enabled: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.UpdateRequestTraceOperatorSettings(context.Background(), tc.input)
			require.ErrorIs(t, err, ErrRequestTraceSettingsUnavailable)
			require.Empty(t, repo.values["request_trace_settings"], "读失败时不得写入任何设置")
		})
	}
}
