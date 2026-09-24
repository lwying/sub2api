package service

// ProvideErrorDiagnosticService 提供错误诊断的领域入口（票据 01／02）。
//
// 该服务只在真实上游尝试的接缝被调用：写入失败必须由调用方忽略，
// 不参与调度、计费，也不改变既有请求审计的普通／强制语义。
// cipher 可以由部署方留空，此时正文一律不留存，绝不回退为明文。
func ProvideErrorDiagnosticService(repo ErrorDiagnosticRepository, settings *SettingService, cipher ErrorDiagnosticBodyCipher) *ErrorDiagnosticService {
	return NewErrorDiagnosticService(repo, settings, cipher)
}

// ProvideErrorDiagnosticCleanupService 提供按保留期清理在线主库诊断数据的后台服务。
func ProvideErrorDiagnosticCleanupService(repo ErrorDiagnosticRepository, diagnostics *ErrorDiagnosticService) *ErrorDiagnosticCleanupService {
	return NewErrorDiagnosticCleanupService(repo, diagnostics.Metrics())
}
