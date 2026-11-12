package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"remarket/backend/internal/product/model"
	"remarket/backend/pkg/apperror"
)

// GetReservation 按订单查询实际预占，结束或到期记录仍返回，查询不到不伪造成功。
func (r *ProductRepository) GetReservation(ctx context.Context, orderID int64) (*model.ProductReservation, error) {
	var result model.ProductReservation
	err := r.db.WithContext(ctx).Where("order_id = ?", orderID).Take(&result).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, &apperror.Error{Kind: apperror.NotFound, Message: "预占不存在"}
	}
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// WithReservation 按商品后预占的统一锁序，原子提交商品、预占及事件；nil 事件代表幂等无变更。
func (r *ProductRepository) WithReservation(ctx context.Context, orderID, productID int64, change func(*model.Product, *model.ProductReservation) (*model.OutboxEvent, error)) (*model.ProductReservation, error) {
	var reservation model.ProductReservation
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var product model.Product
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", productID).Take(&product).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return &apperror.Error{Kind: apperror.NotFound, Message: "商品不存在"}
			}
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_id = ?", orderID).Take(&reservation).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return &apperror.Error{Kind: apperror.NotFound, Message: "预占不存在"}
			}
			return err
		}
		if reservation.ProductID != productID {
			return &apperror.Error{Kind: apperror.FailedPrecondition, Message: "预占与商品不匹配"}
		}
		version, oldStatus := product.AggregateVersion, reservation.Status
		event, err := change(&product, &reservation)
		if err != nil {
			return err
		}
		if event == nil {
			return nil
		}
		updated := tx.Model(&model.Product{}).Where("id = ? AND status = ? AND aggregate_version = ?", productID, model.StatusReserved, version).Updates(map[string]any{"status": product.Status, "aggregate_version": product.AggregateVersion, "updated_at": product.UpdatedAt})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return errors.New("商品预占状态更新失败")
		}
		updated = tx.Model(&model.ProductReservation{}).Where("id = ? AND status = ?", reservation.ID, oldStatus).Updates(map[string]any{"status": reservation.Status, "confirmed_at": reservation.ConfirmedAt, "released_at": reservation.ReleasedAt, "updated_at": reservation.UpdatedAt})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return errors.New("预占状态更新失败")
		}
		// Outbox 插入失败时回滚商品及预占，不能残留部分完成状态。
		return tx.Create(event).Error
	})
	if err != nil {
		return nil, err
	}
	return &reservation, nil
}
