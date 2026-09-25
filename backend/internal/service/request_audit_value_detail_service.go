package service

// 值明细旁路的读取与运维接缝。
//
// 读取分成两步，且**默认视图不含值**：
//   - GetRequestAuditValueDetail 返回信封（状态、原因、标量、计数、到期时刻），类型里没有值字段；
//   - RevealRequestAuditValueDetail 是显式的揭示动作，只有它返回解密后的值。
//
// 运维开关（门控与书面风险确认）也走这个接缝，使采集、读取与运维三处用的是**同一份**
// 门控判定，界面结论与采集结论不会各自漂移。

import (
	"context"
	"time"
)

// RequestAuditValueDetailReveal 是**显式揭示动作**的响应载荷。
//
// 它只有两个字段：这条揭示属于哪条使用记录，以及揭示出来的值。
// 信封字段（状态、原因、到期时刻）刻意不在这里——调用方应当先读默认视图，
// 揭示只回答「值是什么」。
type RequestAuditValueDetailReveal struct {
	UsageLogID int64                         `json:"usage_log_id"`
	Values     RequestAuditValueDetailValues `json:"values"`
}

// RequestAuditValueDetailService 是值明细的读取、揭示与运维接缝。
type RequestAuditValueDetailService struct {
	repo     RequestAuditValueDetailRepository
	settings *SettingService
	now      func() time.Time
}

// NewRequestAuditValueDetailService 构造读取与运维接缝；settings 可为 nil（门控一律按全关）。
func NewRequestAuditValueDetailService(repo RequestAuditValueDetailRepository, settings *SettingService) *RequestAuditValueDetailService {
	return &RequestAuditValueDetailService{repo: repo, settings: settings, now: time.Now}
}

// Capture 返回共享同一仓储与门控的采集接缝，供 gateway 在审计行落库后调用。
//
// 与读取路径用同一份门控结论：读得到、采得到、运维界面显示的，是同一个判定。
func (s *RequestAuditValueDetailService) Capture() *RequestAuditValueDetailCapture {
	if s == nil {
		return nil
	}
	return NewRequestAuditValueDetailCapture(s.repo, s.settings)
}

func (s *RequestAuditValueDetailService) clockNow() time.Time {
	if s == nil || s.now == nil {
		return time.Now()
	}
	return s.now()
}

// GetRequestAuditValueDetail 读取默认披露的信封，**永不返回值**。
//
// 没有行时返回 ErrRequestAuditValueDetailNotFound；存储或设置不可用时返回
// ErrRequestAuditValueDetailUnavailable。到期与已清除不影响信封：它们本身就是
// 「曾经留过、现在不可读」这一事实的对外表达。
func (s *RequestAuditValueDetailService) GetRequestAuditValueDetail(ctx context.Context, usageLogID int64) (RequestAuditValueDetailEnvelope, error) {
	if s == nil || s.repo == nil {
		return RequestAuditValueDetailEnvelope{}, ErrRequestAuditValueDetailUnavailable
	}
	detail, err := s.repo.GetRequestAuditValueDetail(ctx, usageLogID)
	if err != nil {
		return RequestAuditValueDetailEnvelope{}, err
	}
	if detail.UsageLogID <= 0 {
		return RequestAuditValueDetailEnvelope{}, ErrRequestAuditValueDetailNotFound
	}
	now := s.clockNow()
	envelope := RequestAuditValueDetailEnvelope{
		UsageLogID:        detail.UsageLogID,
		State:             DescribeRequestAuditValueDetailState(detail, now),
		Reason:            requestAuditValueDetailDiscloseReason(detail.Reason),
		Route:             detail.Fields.Route,
		Protocol:          detail.Fields.Protocol,
		ClientStatus:      detail.Fields.ClientStatus,
		AttemptCount:      detail.AttemptCount,
		EntryCount:        detail.EntryCount,
		PayloadBytes:      detail.PayloadBytes,
		CreatedAt:         detail.CreatedAt,
		CapabilityEnabled: s.capabilityEnabled(ctx),
	}
	if !detail.Fields.StartedAt.IsZero() {
		startedAt := detail.Fields.StartedAt
		envelope.StartedAt = &startedAt
	}
	if !detail.Fields.CompletedAt.IsZero() {
		completedAt := detail.Fields.CompletedAt
		envelope.CompletedAt = &completedAt
	}
	// 到期时刻只在「确实留存过」时才有意义：从未留存的行走
	// not_observed／skipped，给出一个到期时刻会让界面暗示曾经有值可看。
	if detail.Reason == RequestAuditValueDetailRetained && !detail.ExpiresAt.IsZero() {
		expiresAt := detail.ExpiresAt
		envelope.ExpiresAt = &expiresAt
	}
	return envelope, nil
}

// RevealRequestAuditValueDetail 是显式揭示动作：只有它返回解密后的值。
//
// 三种「没有值可给」必须分开：
//   - ErrRequestAuditValueDetailNotFound：没有这条使用记录的值明细行；
//   - ErrRequestAuditValueDetailGone：曾经留存过，但已过 7 天窗口或已被物理清除；
//   - ErrRequestAuditValueDetailNotRetained：有行，但从未留存过值（未采集／被跳过）。
func (s *RequestAuditValueDetailService) RevealRequestAuditValueDetail(ctx context.Context, usageLogID int64) (RequestAuditValueDetailValues, error) {
	if s == nil || s.repo == nil {
		return RequestAuditValueDetailValues{}, ErrRequestAuditValueDetailUnavailable
	}
	detail, err := s.repo.GetRequestAuditValueDetail(ctx, usageLogID)
	if err != nil {
		return RequestAuditValueDetailValues{}, err
	}
	if detail.UsageLogID <= 0 {
		return RequestAuditValueDetailValues{}, ErrRequestAuditValueDetailNotFound
	}
	now := s.clockNow()
	if !detail.Readable(now) {
		if detail.Reason == RequestAuditValueDetailRetained {
			// 留过值，但此刻已到期或已被清理：这是稳定的「不再可揭示」。
			return RequestAuditValueDetailValues{}, ErrRequestAuditValueDetailGone
		}
		return RequestAuditValueDetailValues{}, ErrRequestAuditValueDetailNotRetained
	}
	values, err := s.repo.ReadRequestAuditValueDetailValues(ctx, usageLogID, now)
	if err != nil {
		// 读取期的到期／密文已清除（Gone）与存储不可用（Unavailable）要分开，
		// 绝不把「缺密钥」与「存储故障」混为一谈。
		return RequestAuditValueDetailValues{}, err
	}
	return values, nil
}

// GetRequestAuditValueDetailOperatorStatus 读取运维开关状态（存量值与校验结论）。
func (s *RequestAuditValueDetailService) GetRequestAuditValueDetailOperatorStatus(ctx context.Context) (RequestAuditValueDetailOperatorStatus, error) {
	if s == nil || s.settings == nil {
		return RequestAuditValueDetailOperatorStatus{}, ErrRequestAuditValueDetailSettingsUnavailable
	}
	return s.settings.GetRequestAuditValueDetailOperatorStatus(ctx)
}

// UpdateRequestAuditValueDetailOperatorSettings 应用一次运维开关更新（开启必须逐字风险确认）。
func (s *RequestAuditValueDetailService) UpdateRequestAuditValueDetailOperatorSettings(ctx context.Context, input RequestAuditValueDetailOperatorUpdateInput) (RequestAuditValueDetailOperatorStatus, error) {
	if s == nil || s.settings == nil {
		return RequestAuditValueDetailOperatorStatus{}, ErrRequestAuditValueDetailSettingsUnavailable
	}
	return s.settings.UpdateRequestAuditValueDetailOperatorSettings(ctx, input)
}

// capabilityEnabled 只用于界面解释，读取失败按「关闭」处理：
// 一个解释性字段不得让整个信封读取失败。
func (s *RequestAuditValueDetailService) capabilityEnabled(ctx context.Context) bool {
	if s == nil || s.settings == nil {
		return false
	}
	return s.settings.RequestAuditValueDetailGate(ctx).CaptureAllowed
}

// requestAuditValueDetailDiscloseReason 只回声封闭原因码集合；未知值按 not_observed 处理，
// 不回显任意存储字符串。
func requestAuditValueDetailDiscloseReason(reason string) string {
	switch reason {
	case RequestAuditValueDetailRetained,
		RequestAuditValueDetailSkippedOutOfScope,
		RequestAuditValueDetailSkippedRetentionDisabled,
		RequestAuditValueDetailSkippedEncryptionUnavailable,
		RequestAuditValueDetailSkippedInvalidValues,
		RequestAuditValueDetailSkippedTooManyAttempts:
		return reason
	default:
		return RequestAuditValueDetailNotObserved
	}
}
