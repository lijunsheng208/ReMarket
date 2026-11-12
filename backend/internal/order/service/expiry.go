package service

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"remarket/backend/internal/order/events"
	"remarket/backend/internal/order/model"
	"time"
)

type ExpiryRepository interface {
	WithExpired(context.Context, func(*model.Order, time.Time) (*model.OrderOperation, *model.OutboxEvent, error)) (bool, error)
}
type ExpiryService struct{ orders ExpiryRepository }

// NewExpiry 注入过期决定持久化；商品释放由统一恢复执行器异步完成。
func NewExpiry(orders ExpiryRepository) *ExpiryService { return &ExpiryService{orders: orders} }

// ExpireOne 在订单锁内按数据库时间决定过期，不能直接终结 PENDING 或覆盖已有成交/取消决定。
func (s *ExpiryService) ExpireOne(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return s.orders.WithExpired(ctx, func(order *model.Order, now time.Time) (*model.OrderOperation, *model.OutboxEvent, error) {
		if order.Status != model.StatusReserved || now.Before(order.ExpiresAt) {
			return nil, nil, nil
		}
		if order.BuyerConfirmed && order.SellerConfirmed {
			return nil, nil, errors.New("双方确认订单缺少成交决定")
		}
		if order.AggregateVersion == math.MaxInt64 {
			return nil, nil, errors.New("订单版本已达上限")
		}
		order.Status = model.StatusExpiring
		order.AggregateVersion++
		order.UpdatedAt = now
		operationID, err := events.NewID()
		if err != nil {
			return nil, nil, err
		}
		event, err := events.NewStateChanged(*order, now)
		if err != nil {
			return nil, nil, err
		}
		return &model.OrderOperation{OperationID: operationID, OrderID: order.ID, OperationType: model.OperationRelease, Status: model.OperationPending, NextRetryAt: now, CreatedAt: now, UpdatedAt: now}, event, nil
	})
}

// Run 启动即扫描，每秒最多领取一百个到期订单；独立于远程恢复，避免慢 RPC 阻塞过期仲裁。
func (s *ExpiryService) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		for i := 0; i < 100 && ctx.Err() == nil; i++ {
			changed, err := s.ExpireOne(ctx)
			if err != nil {
				if ctx.Err() == nil {
					slog.ErrorContext(ctx, "订单过期扫描失败", "code", "EXPIRY_STORAGE_OR_STATE_ERROR")
				}
				break
			}
			if !changed {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
