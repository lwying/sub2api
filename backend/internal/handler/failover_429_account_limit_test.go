package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestLogicalRequestStopsAfterTwoDistinct429Accounts(t *testing.T) {
	limit := NewRequest429AccountLimit(2)
	first := NewFailoverState(10, false)
	first.SetRequest429AccountLimit(limit)
	err429 := &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests}
	err500 := &service.UpstreamFailoverError{StatusCode: http.StatusInternalServerError}
	unscheduler := &mockTempUnscheduler{}

	require.Equal(t, FailoverContinue, first.HandleFailoverError(context.Background(), unscheduler, 10, service.PlatformAnthropic, 0, err500))
	require.Equal(t, FailoverContinue, first.HandleFailoverError(context.Background(), unscheduler, 11, service.PlatformAnthropic, 0, err429))
	// Simulates the outer handler entering a new selection pass for the same logical request.
	second := NewFailoverState(10, false)
	second.SetRequest429AccountLimit(limit)
	require.Equal(t, FailoverExhausted, second.HandleFailoverError(context.Background(), unscheduler, 12, service.PlatformAnthropic, 0, err429))
	require.Equal(t, 2, limit.Count())

	nextRequest := NewFailoverState(10, false)
	nextRequest.SetRequest429AccountLimit(NewRequest429AccountLimit(2))
	require.Equal(t, FailoverContinue, nextRequest.HandleFailoverError(context.Background(), unscheduler, 13, service.PlatformAnthropic, 0, err429))
}

// TestSameAccountMultiple429CountsOnce 覆盖验收标准 3 的前半段：同一账号内部
// 多次 429（含同账号重试用尽前后的多次失败）在请求内只占一个 429 账号额度。
func TestSameAccountMultiple429CountsOnce(t *testing.T) {
	limit := NewRequest429AccountLimit(2)
	fs := NewFailoverState(10, false)
	fs.SetRequest429AccountLimit(limit)
	unscheduler := &mockTempUnscheduler{}
	retryable429 := &service.UpstreamFailoverError{
		StatusCode:             http.StatusTooManyRequests,
		RetryableOnSameAccount: true,
	}
	ctx := context.Background()

	// 同账号重试仍允许：先重试，不占 429 账号额度。
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, unscheduler, 20, service.PlatformAnthropic, 1, retryable429))
	require.Equal(t, 0, limit.Count())

	// 同账号重试用尽、准备换号：此时才计入账号 20。
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, unscheduler, 20, service.PlatformAnthropic, 1, retryable429))
	require.Equal(t, 1, limit.Count())

	// 同一账号再次 429 仍只算一个（选号被重入/排除列表被清理也不例外）。
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, unscheduler, 20, service.PlatformAnthropic, 0, retryable429))
	require.Equal(t, 1, limit.Count())

	// 第二个不同账号 429 才触顶。
	require.Equal(t, FailoverExhausted, fs.HandleFailoverError(ctx, unscheduler, 21, service.PlatformAnthropic, 0, retryable429))
	require.Equal(t, 2, limit.Count())
}

// TestCanceledClientDoesNotConsume429Budget 覆盖验收标准 4：请求取消不应被视为
// 账号耗尽，也不应消耗 429 账号额度（否则会误报触顶）。
func TestCanceledClientDoesNotConsume429Budget(t *testing.T) {
	limit := NewRequest429AccountLimit(2)
	fs := NewFailoverState(10, false)
	fs.SetRequest429AccountLimit(limit)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	action := fs.HandleFailoverError(ctx, &mockTempUnscheduler{}, 30, service.PlatformAnthropic, 0, &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests})
	require.Equal(t, FailoverCanceled, action)
	require.Equal(t, 0, limit.Count())
}

// TestNon429FailureDoesNotConsume429Budget 覆盖验收标准 3：先发生非 429 失败，
// 非 429 自身不占额度，随后两个不同账号的 429 才触顶。
func TestNon429FailureDoesNotConsume429Budget(t *testing.T) {
	limit := NewRequest429AccountLimit(2)
	fs := NewFailoverState(10, false)
	fs.SetRequest429AccountLimit(limit)
	unscheduler := &mockTempUnscheduler{}
	ctx := context.Background()
	err500 := &service.UpstreamFailoverError{StatusCode: http.StatusInternalServerError}
	err429 := &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests}

	require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, unscheduler, 40, service.PlatformAnthropic, 0, err500))
	require.Equal(t, 0, limit.Count())
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, unscheduler, 41, service.PlatformAnthropic, 0, err429))
	require.Equal(t, 1, limit.Count())
	require.Equal(t, FailoverExhausted, fs.HandleFailoverError(ctx, unscheduler, 42, service.PlatformAnthropic, 0, err429))
	require.Equal(t, 2, limit.Count())
}

// TestNoNextAccount429NeitherSwitchesNorConsumesBudget 覆盖验收标准 4：当上游明确
// 禁止再换号（如流已产生语义输出）时，429 既不触发额外账号切换，也不占用额度——
// 该额度只用于「因 429 耗尽同账号重试、准备切换」的账号。
func TestNoNextAccount429NeitherSwitchesNorConsumesBudget(t *testing.T) {
	limit := NewRequest429AccountLimit(2)
	fs := NewFailoverState(10, false)
	fs.SetRequest429AccountLimit(limit)
	stopErr := &service.UpstreamFailoverError{
		StatusCode:        http.StatusTooManyRequests,
		NextAccountAction: service.NextAccountStop,
	}

	action := fs.HandleFailoverError(context.Background(), &mockTempUnscheduler{}, 60, service.PlatformAnthropic, 0, stopErr)
	require.Equal(t, FailoverExhausted, action)
	require.Equal(t, 0, limit.Count())
	require.Equal(t, 0, fs.SwitchCount)
}
