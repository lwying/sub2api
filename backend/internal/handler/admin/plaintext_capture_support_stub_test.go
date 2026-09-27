package admin

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// supportedPlaintextCaptureProbe 是 service.PlaintextCaptureSupportProbe 的替身。
//
// 运维开关相关的 handler 用例验证的是「逐字确认 / 二次验证 / 机器凭证不能开启」这些门槛，
// 不是部署前提（ADR 0007；票据 10）。部署前提缺失时三类新明文采集会先被关掉（fail closed），
// 用例也就测不到自己关心的那道门槛了，所以这里显式把前提置为成立。
//
// 部署前提本身（分区、所有权外键缺失、探针故障、探针没接）的失败关闭与开启拒绝由
// service 层的 plaintext_capture_support_test.go 与 repository 层的目录查询集成测试覆盖。
type supportedPlaintextCaptureProbe struct{}

var _ service.PlaintextCaptureSupportProbe = supportedPlaintextCaptureProbe{}

func (supportedPlaintextCaptureProbe) ProbePlaintextCaptureSupport(context.Context) (service.PlaintextCaptureSupport, error) {
	return service.PlaintextCaptureSupport{
		Supported: true,
		Reason:    service.PlaintextCaptureSupportReasonSupported,
	}, nil
}
