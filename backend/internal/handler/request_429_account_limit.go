package handler

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Request429AccountLimit tracks distinct failed accounts for one logical request.
type Request429AccountLimit struct {
	maxAccounts int
	failedIDs   map[int64]struct{}
}

func NewRequest429AccountLimit(maxAccounts int) *Request429AccountLimit {
	if maxAccounts < 1 {
		maxAccounts = 2
	}
	return &Request429AccountLimit{maxAccounts: maxAccounts, failedIDs: make(map[int64]struct{})}
}

func (l *Request429AccountLimit) Count() int {
	if l == nil {
		return 0
	}
	return len(l.failedIDs)
}

func (l *Request429AccountLimit) Record(accountID int64, failoverErr *service.UpstreamFailoverError) bool {
	if l == nil || failoverErr == nil || failoverErr.StatusCode != http.StatusTooManyRequests {
		return false
	}
	l.failedIDs[accountID] = struct{}{}
	return len(l.failedIDs) >= l.maxAccounts
}
