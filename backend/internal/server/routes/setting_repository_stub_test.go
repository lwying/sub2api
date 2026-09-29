package routes

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// errorDiagnosticSettingsRouteStubRepo 只提供服务构造所需的空仓储。
//
// 它原先定义在已随票据 10 退役删除的 error_diagnostic_settings_routes_test.go 中，
// 但 `internal/server/routes/request_trace_settings_routes_test.go`（Trace 侧，不在本次
// 授权范围内）仍在使用，因此搬到这里单独保存。该测试改用自有替身后本文件应一并删除。
type errorDiagnosticSettingsRouteStubRepo struct{}

func (errorDiagnosticSettingsRouteStubRepo) Get(_ context.Context, _ string) (*service.Setting, error) {
	return nil, service.ErrSettingNotFound
}

func (errorDiagnosticSettingsRouteStubRepo) GetValue(_ context.Context, _ string) (string, error) {
	return "", service.ErrSettingNotFound
}

func (errorDiagnosticSettingsRouteStubRepo) Set(_ context.Context, _, _ string) error { return nil }

func (errorDiagnosticSettingsRouteStubRepo) GetMultiple(_ context.Context, _ []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (errorDiagnosticSettingsRouteStubRepo) SetMultiple(_ context.Context, _ map[string]string) error {
	return nil
}

func (errorDiagnosticSettingsRouteStubRepo) GetAll(_ context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (errorDiagnosticSettingsRouteStubRepo) Delete(_ context.Context, _ string) error { return nil }
