package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// 明文导出任务的资源上限。
//
// 它必须由管理员显式设为**有限**的正数：没有上限就意味着一句筛选可能把磁盘写满。
// 上限在任务创建时取一次快照并随任务保存，因此之后改配置不会改变已排队任务的行为。
const (
	SettingKeyRequestTraceExportLimits = "request_trace_export_limits"

	// 上限的允许区间。下限保证一次导出至少能写出一点东西，上限避免配置成天文数字。
	RequestTraceExportMinRows       = int64(100)
	RequestTraceExportMaxRows       = int64(5_000_000)
	RequestTraceExportMinBytes      = int64(1 << 20)
	RequestTraceExportMaxBytes      = int64(64 << 30)
	RequestTraceExportMinRuntime    = 30 * time.Second
	RequestTraceExportMaxRuntimeCap = 6 * time.Hour
	RequestTraceExportMinShardRow   = int64(10)
)

var ErrRequestTraceExportLimitsInvalid = errors.New("request trace export limits are invalid")

// RequestTraceExportLimits 是整任务与单分片的资源上限。
type RequestTraceExportLimits struct {
	MaxRows       int64 `json:"max_rows"`
	MaxBytes      int64 `json:"max_bytes"`
	MaxRuntimeSec int64 `json:"max_runtime_seconds"`
	MaxShardRows  int64 `json:"max_shard_rows"`
	MaxShardBytes int64 `json:"max_shard_bytes"`
	// Configured 为 false 表示管理员没有显式配置过，当前值来自缺省。
	Configured bool `json:"configured"`
}

// DefaultRequestTraceExportLimits 返回保守的缺省上限。
func DefaultRequestTraceExportLimits() RequestTraceExportLimits {
	return RequestTraceExportLimits{
		MaxRows:       requestTraceExportDefaultRows,
		MaxBytes:      requestTraceExportDefaultBytes,
		MaxRuntimeSec: int64(requestTraceExportDefaultRuntime / time.Second),
		MaxShardRows:  requestTraceExportDefaultShardRows,
		MaxShardBytes: requestTraceExportDefaultShardBytes,
	}
}

// NormalizeRequestTraceExportLimits 把缺省与越界值收敛到允许区间。
// 缺省值不标记 Configured，越界值被夹到区间边界而不是被拒绝：
// 让一次错误输入不至于把任务变成"永不动"的状态。
func NormalizeRequestTraceExportLimits(limits RequestTraceExportLimits) RequestTraceExportLimits {
	if limits.MaxRows < RequestTraceExportMinRows {
		limits.MaxRows = RequestTraceExportMinRows
	}
	if limits.MaxRows > RequestTraceExportMaxRows {
		limits.MaxRows = RequestTraceExportMaxRows
	}
	if limits.MaxBytes < RequestTraceExportMinBytes {
		limits.MaxBytes = RequestTraceExportMinBytes
	}
	if limits.MaxBytes > RequestTraceExportMaxBytes {
		limits.MaxBytes = RequestTraceExportMaxBytes
	}
	if limits.MaxRuntimeSec < int64(RequestTraceExportMinRuntime/time.Second) {
		limits.MaxRuntimeSec = int64(RequestTraceExportMinRuntime / time.Second)
	}
	if limits.MaxRuntimeSec > int64(RequestTraceExportMaxRuntimeCap/time.Second) {
		limits.MaxRuntimeSec = int64(RequestTraceExportMaxRuntimeCap / time.Second)
	}
	if limits.MaxShardRows < RequestTraceExportMinShardRow {
		limits.MaxShardRows = RequestTraceExportMinShardRow
	}
	if limits.MaxShardRows > limits.MaxRows {
		limits.MaxShardRows = limits.MaxRows
	}
	if limits.MaxShardBytes < RequestTraceExportMinBytes {
		limits.MaxShardBytes = RequestTraceExportMinBytes
	}
	if limits.MaxShardBytes > limits.MaxBytes {
		limits.MaxShardBytes = limits.MaxBytes
	}
	return limits
}

// Runtime 把秒数换算成时长。
func (l RequestTraceExportLimits) Runtime() time.Duration {
	return time.Duration(l.MaxRuntimeSec) * time.Second
}

// RequestTraceExportLimitsFromOptions 把构造时的缺省值表达成一份上限，供没有
// 注入配置来源的部署使用。
func RequestTraceExportLimitsFromOptions(opts RequestTraceExportOptions) RequestTraceExportLimits {
	return RequestTraceExportLimits{
		MaxRows:       opts.MaxRows,
		MaxBytes:      opts.MaxBytes,
		MaxRuntimeSec: int64(opts.MaxRuntime / time.Second),
		MaxShardRows:  opts.MaxShardRows,
		MaxShardBytes: opts.MaxShardBytes,
	}
}

// requestTraceExportBounds 是一次任务实际使用的资源预算。
//
// 它按任务取值，而不是挂在服务上共用一个可变字段：一个任务执行期间另一个任务
// （或另一个管理员会话）不该改变它的边界。取值来源见 boundsForTask。
type requestTraceExportBounds struct {
	MaxRows       int64
	MaxBytes      int64
	MaxRuntime    time.Duration
	MaxShardRows  int64
	MaxShardBytes int64
}

// boundsFromLimits 归一化一份上限并换算成执行预算。
//
// 归一化同时是护栏：无论快照来自落库值还是当前配置，落到执行路径上的上限都被
// 夹在允许区间内，永远是有限正数。单分片上限不得超过整任务上限，否则分片永远
// 写不满，上限形同虚设。
func boundsFromLimits(limits RequestTraceExportLimits) requestTraceExportBounds {
	limits = NormalizeRequestTraceExportLimits(limits)
	bounds := requestTraceExportBounds{
		MaxRows:       limits.MaxRows,
		MaxBytes:      limits.MaxBytes,
		MaxRuntime:    limits.Runtime(),
		MaxShardRows:  limits.MaxShardRows,
		MaxShardBytes: limits.MaxShardBytes,
	}
	if bounds.MaxShardRows > bounds.MaxRows {
		bounds.MaxShardRows = bounds.MaxRows
	}
	if bounds.MaxShardBytes > bounds.MaxBytes {
		bounds.MaxShardBytes = bounds.MaxBytes
	}
	return bounds
}

// boundsFromOptions 把构造时的缺省值当作执行预算。
//
// 这条路径不归一化：缺省值属于部署自己给定的配置，构造时已经补齐并保证分片
// 上限不超过整任务上限，服务不该在这里悄悄改写它（例如把一个小到足以立刻
// 触发截断的测试预算抬到下限之上）。
func boundsFromOptions(opts RequestTraceExportOptions) requestTraceExportBounds {
	return requestTraceExportBounds{
		MaxRows:       opts.MaxRows,
		MaxBytes:      opts.MaxBytes,
		MaxRuntime:    opts.MaxRuntime,
		MaxShardRows:  opts.MaxShardRows,
		MaxShardBytes: opts.MaxShardBytes,
	}
}

// readRequestTraceExportLimits 读取已存的上限；缺省、损坏或读取失败都按缺省处理，
// 因为这里给的是"能写多少"，不是"能不能写"——权限边界由确认与部署条件决定。
func (s *SettingService) readRequestTraceExportLimits(ctx context.Context) RequestTraceExportLimits {
	defaults := DefaultRequestTraceExportLimits()
	if s == nil || s.settingRepo == nil {
		return defaults
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyRequestTraceExportLimits)
	if errors.Is(err, ErrSettingNotFound) {
		return defaults
	}
	if err != nil {
		return defaults
	}
	var stored RequestTraceExportLimits
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return defaults
	}
	return NormalizeRequestTraceExportLimits(stored)
}

// GetRequestTraceExportLimits 返回当前生效的任务上限，供管理端回显。
func (s *SettingService) GetRequestTraceExportLimits() RequestTraceExportLimits {
	return s.readRequestTraceExportLimits(context.Background())
}

// UpdateRequestTraceExportLimits 保存管理员配置的上限。
// 保存前归一化并标记为已配置：任务创建时会取这份快照。
func (s *SettingService) UpdateRequestTraceExportLimits(ctx context.Context, limits RequestTraceExportLimits) (RequestTraceExportLimits, error) {
	if s == nil || s.settingRepo == nil {
		return RequestTraceExportLimits{}, ErrRequestTraceSettingsUnavailable
	}
	normalized := NormalizeRequestTraceExportLimits(limits)
	normalized.Configured = true
	payload, err := json.Marshal(normalized)
	if err != nil {
		return RequestTraceExportLimits{}, fmt.Errorf("encode request trace export limits: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyRequestTraceExportLimits, string(payload)); err != nil {
		return RequestTraceExportLimits{}, fmt.Errorf("save request trace export limits: %w", err)
	}
	return normalized, nil
}
