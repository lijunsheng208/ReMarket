package service

import (
	"remarket/backend/internal/order/model"
	"testing"
)

// TestCanTransition 验证全部状态对，确保成交与释放决定不可互换且终态不可重开。
func TestCanTransition(t *testing.T) {
	states := []model.OrderStatus{model.StatusPending, model.StatusReserved, model.StatusConfirming, model.StatusCancelling, model.StatusExpiring, model.StatusCompleted, model.StatusCancelled, model.StatusExpired, model.StatusFailed, "UNKNOWN", ""}
	allowed := map[[2]model.OrderStatus]bool{
		{model.StatusPending, model.StatusReserved}:     true,
		{model.StatusPending, model.StatusFailed}:       true,
		{model.StatusReserved, model.StatusReserved}:    true,
		{model.StatusReserved, model.StatusConfirming}:  true,
		{model.StatusReserved, model.StatusCancelling}:  true,
		{model.StatusReserved, model.StatusExpiring}:    true,
		{model.StatusConfirming, model.StatusCompleted}: true,
		{model.StatusCancelling, model.StatusCancelled}: true,
		{model.StatusExpiring, model.StatusExpired}:     true,
	}
	for _, from := range states {
		for _, to := range states {
			if got := CanTransition(from, to); got != allowed[[2]model.OrderStatus{from, to}] {
				t.Errorf("转换 %q → %q: got %v", from, to, got)
			}
		}
	}
}
