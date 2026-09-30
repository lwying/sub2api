//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 导出的可用性由「管理员开启 Trace + 确认导出风险 + 显式单实例声明」决定，
// 不再额外要求一个配置文件开关。
//
// 这条用例刻意走**默认配置**：`cfg` 不带任何 request_trace_export 键，设置仓库里
// 只有界面能写的那两项。它复现的是管理员按设计操作（在界面上开启并确认）之后
// 仍然拿不到导出的真实路径——此前因为服务构造要求 `opts.Enabled`，
// 而该值来自默认 false 的 `request_trace_export.enabled`，导出永远被拒。
func TestRequestTraceExportWorksWithDefaultConfigAndInterfaceConsent(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{RunMode: config.RunModeSimple}

	// 默认配置：导出相关键全部未设置。
	require.False(t, cfg.RequestTraceExport.Enabled)
	require.False(t, cfg.RequestTraceExport.SingleInstanceDeclared)

	settingsRepo := &traceSettingRepoStub{values: map[string]string{}}
	settings := NewSettingService(settingsRepo, cfg)

	// 管理员在界面上确认导出风险（这是导出唯一的授权动作）。
	_, err := settings.AcknowledgeRequestTraceExportRisk(ctx, RequestTraceExportRiskUpdateInput{
		Language: "en", Phrase: RequestTraceExportRiskAcknowledgementPhraseEN, AdminUserID: 12,
	})
	require.NoError(t, err)
	require.True(t, settings.RequestTraceExportAcknowledged(ctx))

	traceID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store := &traceExportStoreStub{}
	source := &traceExportSourceStub{
		ids:     []string{traceID},
		details: map[string]RequestTraceExportApprovedDetail{traceID: {TraceID: traceID}},
	}

	// 装配方式与生产一致：导出能力只看界面授权 + 单实例声明，不看配置文件开关。
	traceExport := NewRequestTraceExportService(store, source, RequestTraceExportOptions{
		SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one",
	})
	traceExport.SetAcknowledgementChecker(func() bool { return settings.RequestTraceExportAcknowledged(ctx) })

	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "admin-session"}
	task, err := traceExport.CreateTask(ctx, actor, RequestTraceExportFilter{})
	require.NoError(t, err, "界面已确认风险且部署已声明单实例时，默认配置下也必须能创建导出任务")

	finished, err := traceExport.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, RequestTraceExportCompleted, finished.Status)
	require.Equal(t, int64(1), finished.RowsExported)

	// 交付物必须真的可下载：清单说明这次导出，分片承载详情。
	manifestFile, _, err := traceExport.OpenDownload(ctx, actor, task.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = manifestFile.Close() })
	shardFile, _, _, err := traceExport.OpenShardDownload(ctx, actor, task.ID, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = shardFile.Close() })
}

// 未确认风险时仍然必须被拒——上面的放宽不能顺手把这条授权边界也放开。
func TestRequestTraceExportStillRefusesWithoutInterfaceConsent(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{RunMode: config.RunModeSimple}
	settingsRepo := &traceSettingRepoStub{values: map[string]string{}}
	settings := NewSettingService(settingsRepo, cfg)

	traceExport := NewRequestTraceExportService(&traceExportStoreStub{}, &traceExportSourceStub{}, RequestTraceExportOptions{
		SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one",
	})
	traceExport.SetAcknowledgementChecker(func() bool { return settings.RequestTraceExportAcknowledged(ctx) })

	_, err := traceExport.CreateTask(ctx, RequestTraceExportActor{AdminUserID: 12, SessionID: "s"}, RequestTraceExportFilter{})
	require.ErrorIs(t, err, ErrRequestTraceExportRiskAcknowledgementRequired)
}

// 下载时的三种"还没有文件可下载"必须区分开：排队中/进行中是可重试的未就绪；
// 已失败是终态（重试无用，报不可用）；已完成但窗口关闭仍是过期。
func TestRequestTraceExportDownloadDistinguishesPendingFailedAndExpired(t *testing.T) {
	ctx := context.Background()
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "s"}
	cases := []struct {
		name   string
		status RequestTraceExportStatus
		want   error
	}{
		{name: "pending is retryable", status: RequestTraceExportPending, want: ErrRequestTraceExportNotReady},
		{name: "running is retryable", status: RequestTraceExportRunning, want: ErrRequestTraceExportNotReady},
		{name: "failed is terminal", status: RequestTraceExportFailed, want: ErrRequestTraceExportUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			store := &traceExportStoreStub{tasks: map[string]RequestTraceExportTask{
				id: {
					ID: id, Status: tc.status, AdminUserID: actor.AdminUserID,
					SessionDigest: sessionDigest(actor.SessionID), InstanceID: "instance-one", CreatedAt: time.Now(),
				},
			}}
			svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{
				SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one",
			})
			svc.SetAcknowledgementSatisfiedForTest(true)

			_, _, err := svc.OpenDownload(ctx, actor, id)
			require.ErrorIs(t, err, tc.want)
			_, _, _, err = svc.OpenShardDownload(ctx, actor, id, 1)
			require.ErrorIs(t, err, tc.want)
		})
	}
}

// 没有单实例声明时同样必须被拒：这是部署前提，不是配置文件开关。
func TestRequestTraceExportStillRequiresSingleInstanceDeclaration(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{RunMode: config.RunModeSimple}
	settingsRepo := &traceSettingRepoStub{values: map[string]string{}}
	settings := NewSettingService(settingsRepo, cfg)
	_, err := settings.AcknowledgeRequestTraceExportRisk(ctx, RequestTraceExportRiskUpdateInput{
		Language: "en", Phrase: RequestTraceExportRiskAcknowledgementPhraseEN, AdminUserID: 12,
	})
	require.NoError(t, err)

	traceExport := NewRequestTraceExportService(&traceExportStoreStub{}, &traceExportSourceStub{}, RequestTraceExportOptions{
		TempDir: t.TempDir(), InstanceID: "instance-one",
	})
	traceExport.SetAcknowledgementChecker(func() bool { return settings.RequestTraceExportAcknowledged(ctx) })

	_, err = traceExport.CreateTask(ctx, RequestTraceExportActor{AdminUserID: 12, SessionID: "s"}, RequestTraceExportFilter{})
	require.ErrorIs(t, err, ErrRequestTraceExportDisabled)
}
