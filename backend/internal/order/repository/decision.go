package repository

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"remarket/backend/internal/order/model"
	"remarket/backend/pkg/apperror"
	"time"
)

// WithDecision 锁定参与者订单并取得锁后的数据库时间，原子保存业务决定、操作及事件。
func (r *Repository) WithDecision(ctx context.Context, id, userID int64, change func(*model.Order, time.Time) (*model.OrderOperation, *model.OutboxEvent, error)) (*model.Order, error) {
	var order model.Order
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND (buyer_id = ? OR seller_id = ?)", id, userID, userID).Take(&order).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return &apperror.Error{Kind: apperror.NotFound, Message: "订单不存在"}
			}
			return err
		}
		var now time.Time
		if err := tx.Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&now); err != nil {
			return err
		}
		version := order.AggregateVersion
		operation, event, err := change(&order, now)
		if err != nil {
			return err
		}
		if event == nil {
			return nil
		}
		update := tx.Model(&model.Order{}).Where("id = ? AND aggregate_version = ?", order.ID, version).Updates(map[string]any{"status": order.Status, "buyer_confirmed": order.BuyerConfirmed, "seller_confirmed": order.SellerConfirmed, "aggregate_version": order.AggregateVersion, "updated_at": order.UpdatedAt})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return errors.New("订单决定版本更新失败")
		}
		if operation != nil {
			if err := tx.Create(operation).Error; err != nil {
				return err
			}
		}
		return tx.Create(event).Error
	})
	if err != nil {
		return nil, err
	}
	return &order, nil
}
