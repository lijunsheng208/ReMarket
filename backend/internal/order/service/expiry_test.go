package service

import (
	"context"
	"remarket/backend/internal/order/model"
	"testing"
	"time"
)

type expiryBoundaryRepository struct {
	order     model.Order
	now       time.Time
	operation *model.OrderOperation
	event     *model.OutboxEvent
}

// WithExpired 模拟持锁后的数据库时间，仅用于精确截止时刻的业务边界验证。
func (r *expiryBoundaryRepository) WithExpired(ctx context.Context, change func(*model.Order, time.Time) (*model.OrderOperation, *model.OutboxEvent, error)) (bool, error) {
	operation, event, err := change(&r.order, r.now)
	if err != nil {
		return false, err
	}
	r.operation, r.event = operation, event
	return event != nil, nil
}

// TestExpiryBoundary 验证截止前一微秒不关闭、恰到截止时可过期，已有决定不被覆盖。
func TestExpiryBoundary(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		status   model.OrderStatus
		deadline time.Time
		want     bool
	}{
		{"before", model.StatusReserved, now.Add(time.Microsecond), false},
		{"equal", model.StatusReserved, now, true},
		{"after", model.StatusReserved, now.Add(-time.Microsecond), true},
		{"pending", model.StatusPending, now, false},
		{"confirming", model.StatusConfirming, now, false},
		{"cancelling", model.StatusCancelling, now, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &expiryBoundaryRepository{now: now, order: model.Order{ID: 1, BuyerID: 1, SellerID: 2, ProductID: 3, PriceCent: 100, Status: tc.status, AggregateVersion: 2, BuyerConfirmed: true, ExpiresAt: tc.deadline}}
			changed, err := NewExpiry(repo).ExpireOne(context.Background())
			if err != nil || changed != tc.want {
				t.Fatal("截止判断错误", changed, err)
			}
			if tc.want && (repo.order.Status != model.StatusExpiring || repo.order.AggregateVersion != 3 || !repo.order.BuyerConfirmed || repo.operation.OperationType != model.OperationRelease || repo.event.EventType != "order.expiring.v1") {
				t.Fatal("过期决定内容不正确")
			}
		})
	}
}
