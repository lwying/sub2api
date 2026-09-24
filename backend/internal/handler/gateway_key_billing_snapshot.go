package handler

import "github.com/Wei-Shaw/sub2api/internal/service"

func (h *GatewayHandler) SetKeyBillingSnapshotService(snapshot *service.KeyBillingSnapshotService) {
	if h != nil {
		h.keyBillingSnapshot = snapshot
	}
}
