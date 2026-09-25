package service

// 值明细旁路的依赖装配（ADR 0006）。
//
// 读取接缝与采集接缝共用同一个仓储与同一份门控判定，因此运维界面显示的、
// 采集时强制的、读取时依赖的，是同一个结论。门控读取器直接传 *SettingService，
// 与错误诊断的装配方式一致：这里不需要额外 Bind 接口。

// ProvideRequestAuditValueDetailService 构造值明细的读取、揭示与运维接缝。
func ProvideRequestAuditValueDetailService(repo RequestAuditValueDetailRepository, settings *SettingService) *RequestAuditValueDetailService {
	return NewRequestAuditValueDetailService(repo, settings)
}

// ProvideRequestAuditValueDetailCapture 构造值明细的采集接缝。
//
// gateway 在请求审计行落库之后调用它；Enabled 供绑定阶段决定是否在请求上下文上打
// httpattempt.WithClaudeHeaderValueCapture 标记，因此默认关闭的部署不会复制任何明文值。
func ProvideRequestAuditValueDetailCapture(repo RequestAuditValueDetailRepository, settings *SettingService) *RequestAuditValueDetailCapture {
	return NewRequestAuditValueDetailCapture(repo, settings)
}
