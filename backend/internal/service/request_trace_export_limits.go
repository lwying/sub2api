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
