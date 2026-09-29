package service

// 值明细旁路的**历史留存契约**（ADR 0006；票据 10 退役）。
//
// 新采集、运维开关与揭示入口已在票据 10 一并移除：新版本不再写入任何新的值明细行，
// 也不再暴露读取旧明文的接口。这里只保留仍然必需的部分——把已存在的旧密文按原七天
// 期限在在线主库上物理清除，并上报清理积压（见 request_audit_value_detail_cleanup_service.go）。
//
// 保留期语义与旧版一致，退役不改变它：
//   - 第 7 天起应用层拒绝读取（该读取入口已随退役移除）；
//   - 在线主库上的物理清除是周期批量作业，每轮有上限，不承诺「到期即删」；
//   - 已关联使用记录的行归使用记录所有，随 usage 删除，不因这个计时器提前消失。
//
// 迁移 251–257 的字节不因本次退役回写；旧表不因升级自动 DROP 或清空。

import (
	"context"
	"errors"
	"time"
)

// ErrRequestAuditValueDetailUnavailable 表示存储或密钥此刻不可用（可重试）。
//
// 退役后它只由清理作业返回：读取侧的「没有行／未留存／已到期」三个分支随读取入口
// 一并移除，因此这里不再保留那三个哨兵。
var ErrRequestAuditValueDetailUnavailable = errors.New("request audit value detail unavailable")

// RequestAuditValueDetailRepository 是值明细存储的**留存契约**：只保留到期物理清除。
//
// 采集方法与读取方法随票据 10 一并移除，因此任何调用方都无法再通过这个接口写入
// 或读出值——旧密文只可能被清除，不会被重新读出或覆盖。
type RequestAuditValueDetailRepository interface {
	ClearExpiredRequestAuditValueDetails(ctx context.Context, now time.Time, limit int) (int64, error)
}
