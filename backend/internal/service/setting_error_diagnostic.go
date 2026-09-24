package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrorDiagnosticRiskAcknowledgementReader 报告是否存在覆盖当前语句版本的有效书面确认。
//
// 只返回布尔结论：操作员身份、来源 IP 等 PII 不进入采集侧。
// 由 *SettingService 实现。
type ErrorDiagnosticRiskAcknowledgementReader interface {
	ErrorDiagnosticRiskAcknowledgementCurrent(ctx context.Context) (bool, error)
}

var _ ErrorDiagnosticRiskAcknowledgementReader = (*SettingService)(nil)

// ApplyErrorDiagnosticRiskAcknowledgement 把书面确认并入采集门控结论。
//
// 这是采集门控的唯一判定点：只有「存量布尔值为真」并且「存在覆盖当前语句版本的有效
// 书面确认记录」时才报告可采集。因此直接改库、其他管理路径或历史脏数据把布尔值写成真，
// 都不足以打开采集——没有书面确认就没有采集。
//
// fail closed，且不在热路径上抛错：
//   - 布尔值本来就为假时不读取确认键（关闭态不依赖确认记录，也不产生额外读）；
//   - 读取器缺失、读不到或读出错，一律按全关返回，且不把错误交给采集热路径。
//     确认键损坏时采集应当停，而不是让采集侧去记一条它无法处理的错误；
//     该故障由运维状态接口（单独读取确认键）暴露。
func ApplyErrorDiagnosticRiskAcknowledgement(ctx context.Context, settings ErrorDiagnosticSettings, reader ErrorDiagnosticRiskAcknowledgementReader) ErrorDiagnosticSettings {
	if !settings.CaptureAllowed() {
		return settings
	}
	if reader == nil {
		// 没有确认读取能力就不可能有「有效书面确认」，因此一律按全关。
		return ErrorDiagnosticSettings{}
	}
	current, err := reader.ErrorDiagnosticRiskAcknowledgementCurrent(ctx)
	if err != nil || !current {
		return ErrorDiagnosticSettings{}
	}
	return settings
}

// ReadStoredErrorDiagnosticSettings 读取**存量**门控配置，不做书面确认校验。
//
// 仅供运维状态展示「存了什么」：与 GetErrorDiagnosticSettings 的有效结论对照，
// 使越权改写或脏数据呈现为「存量开着、但不允许采集」，而不是一个无法解释的开启。
// 采集侧一律使用 GetErrorDiagnosticSettings，不得使用本方法。
func (s *SettingService) ReadStoredErrorDiagnosticSettings(ctx context.Context) (ErrorDiagnosticSettings, error) {
	defaults := ErrorDiagnosticSettings{}
	if s == nil || s.settingRepo == nil {
		return defaults, errors.New("error diagnostic settings are unavailable")
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyErrorDiagnostic)
	if errors.Is(err, ErrSettingNotFound) {
		return defaults, nil
	}
	if err != nil {
		return defaults, fmt.Errorf("get error diagnostic settings: %w", err)
	}
	if raw == "" {
		return defaults, nil
	}
	var settings ErrorDiagnosticSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return defaults, fmt.Errorf("decode error diagnostic settings: %w", err)
	}
	return settings, nil
}

// GetErrorDiagnosticSettings 读取**有效**门控配置：存量布尔值与书面确认合并后的结论，
// 并在正文留存这一层再并入「稳定密钥是否可用」。
//
// 生产默认全关：设置缺失、为空、解析失败或读取失败都返回「全关」，
// 只有显式写入 enabled=true 且 risk_acknowledged=true、并且存在覆盖当前语句版本的
// 有效书面确认时才允许采集。操作员的风险确认是启用门槛的一部分，不能由代码代替，
// 也不能只由存量布尔值代表。
//
// 正文留存比采集更窄：存量开着而稳定密钥缺席时（配置被移除、随机密钥重启后失效），
// 有效结论里的 BodyRetentionEnabled 一律为 false，采集（元数据）不受影响。
// 否则接缝会 tee 出一份注定被判为 skipped_encryption_unavailable 的明文，白白把
// 最多 1 MiB 的请求正文复制进内存。
//
// 收窄只发生在这一层布尔值上：那段**存量意图**随之留在 BodyRetentionRequested 里，
// 使接缝仍能表达「要求过留存、但部署此刻做不到」，从而在元数据上留下稳定原因码
// skipped_encryption_unavailable，而不是退化成 not_observed。因此本方法既不弱化门控
// （有效结论的采集与正文留存判定一字未改），也不增加任何按请求的额外读取。
//
// 采集侧（含诊断服务与传输接缝）必须使用本方法；需要查看存量值的运维界面使用
// ReadStoredErrorDiagnosticSettings。
func (s *SettingService) GetErrorDiagnosticSettings(ctx context.Context) (ErrorDiagnosticSettings, error) {
	stored, err := s.ReadStoredErrorDiagnosticSettings(ctx)
	if err != nil {
		return ErrorDiagnosticSettings{}, err
	}
	effective := ApplyErrorDiagnosticRiskAcknowledgement(ctx, stored, s)
	return s.applyErrorDiagnosticBodyRetentionKeyAvailability(effective), nil
}
