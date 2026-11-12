package repository

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"remarket/backend/internal/order/model"
	"time"
)

// WithExpired 按到期索引领取一个 RESERVED 订单并锁定，原子保存过期决定、释放操作和事件。
// SKIP LOCKED 允许多实例扫描并跳过正在确认或取消的订单，不持锁调用 Product。
func (r *Repository) WithExpired(ctx context.Context, change func(*model.Order, time.Time) (*model.OrderOperation, *model.OutboxEvent, error)) (bool, error) {
	changed := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var order model.Order
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("status = ? AND expires_at <= UTC_TIMESTAMP(6)", model.StatusReserved).Order("expires_at, id").Take(&order).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		// 等待/取得行锁后重新取数据库时间，确认和过期共用相同截止边界。
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
		update := tx.Model(&model.Order{}).Where("id = ? AND status = ? AND aggregate_version = ?", order.ID, model.StatusReserved, version).Updates(map[string]any{"status": order.Status, "aggregate_version": order.AggregateVersion, "updated_at": order.UpdatedAt})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return errors.New("订单过期决定更新失败")
		}
		if err := tx.Create(operation).Error; err != nil {
			return err
		}
		if err := tx.Create(event).Error; err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}
