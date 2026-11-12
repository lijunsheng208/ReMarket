package repository

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"remarket/backend/internal/order/model"
	"remarket/backend/pkg/apperror"
)

// FindByKey 查询买家幂等键，未找到返回 nil，不能借此跨用户读取订单。
func (r *Repository) FindByKey(ctx context.Context, buyerID int64, key string) (*model.Order, error) {
	var order model.Order
	err := r.db.WithContext(ctx).Where("buyer_id = ? AND idempotency_key = ?", buyerID, key).Take(&order).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &order, nil
}

// CreatePending 将订单、RESERVE 操作与创建事件原子保存，唯一键竞争失败后返回赢家。
// build 在生成订单 ID 后调用，失败时整个事务回滚；回调不能执行外部 RPC。
func (r *Repository) CreatePending(ctx context.Context, order *model.Order, build func(*model.Order) (*model.OrderOperation, *model.OutboxEvent, error)) (*model.Order, error) {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(order).Error; err != nil {
			return err
		}
		operation, event, err := build(order)
		if err != nil {
			return err
		}
		if err := tx.Create(operation).Error; err != nil {
			return err
		}
		return tx.Create(event).Error
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		existing, readErr := r.FindByKey(ctx, order.BuyerID, order.IdempotencyKey)
		if readErr != nil {
			return nil, readErr
		}
		if existing != nil {
			if existing.RequestHash != order.RequestHash {
				return nil, &apperror.Error{Kind: apperror.Conflict, Message: "幂等键已用于不同请求"}
			}
			return existing, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return order, nil
}
