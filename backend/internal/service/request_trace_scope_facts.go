package service

import (
	"context"
	"strings"
)

// 请求 Trace 采集范围的"请求时事实"载体。
//
// 采集范围在请求结束时按实际观察到的事实复核，因此执行过程中必须把
// 客户端请求模型与实际选中过的上游账号平台记在请求上下文里。

type requestTraceScopeFactsContextKey struct{}

// requestTraceScopeFacts 是一次逻辑请求上累积的范围事实。
type requestTraceScopeFacts struct {
	requestedModel string
	platforms      []string
}

// WithRequestTraceRequestedModel 记录客户端请求的模型名（解析请求体之后调用）。
func WithRequestTraceRequestedModel(ctx context.Context, model string) context.Context {
	if ctx == nil {
		return ctx
	}
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return ctx
	}
	facts, _ := ctx.Value(requestTraceScopeFactsContextKey{}).(*requestTraceScopeFacts)
	if facts == nil {
		facts = &requestTraceScopeFacts{}
		ctx = context.WithValue(ctx, requestTraceScopeFactsContextKey{}, facts)
	}
	facts.requestedModel = trimmed
	return ctx
}

// WithRequestTraceSelectedPlatform 记录一次实际选中的上游账号平台。
// 采集范围按"首次可确定的平台"判定，因此只保留第一次；后来的尝试可能切到别的平台，
// 但那条逻辑请求的采集结论不再改变（页面仍按任一平台命中）。
func WithRequestTraceSelectedPlatform(ctx context.Context, platform string) context.Context {
	if ctx == nil {
		return ctx
	}
	trimmed := strings.TrimSpace(platform)
	if trimmed == "" {
		return ctx
	}
	facts, _ := ctx.Value(requestTraceScopeFactsContextKey{}).(*requestTraceScopeFacts)
	if facts == nil {
		facts = &requestTraceScopeFacts{}
		ctx = context.WithValue(ctx, requestTraceScopeFactsContextKey{}, facts)
	}
	// 只认第一次：后来换账号选到的平台不参与采集范围判定。
	if len(facts.platforms) == 0 {
		facts.platforms = append(facts.platforms, trimmed)
	}
	return ctx
}

// RequestTraceScopeFactsFromContext 读出本次请求累积的范围事实。
func RequestTraceScopeFactsFromContext(ctx context.Context) (string, []string) {
	if ctx == nil {
		return "", nil
	}
	facts, _ := ctx.Value(requestTraceScopeFactsContextKey{}).(*requestTraceScopeFacts)
	if facts == nil {
		return "", nil
	}
	return facts.requestedModel, append([]string(nil), facts.platforms...)
}
