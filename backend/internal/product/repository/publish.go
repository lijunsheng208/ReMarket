package repository

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"remarket/backend/internal/product/model"
)

// WithPublication 锁定商品和分类，由 Service 回调决定状态变化，再原子保存商品与事件。
// 回调返回 nil 事件表示无变化，不更新商品；任意错误均回滚整个事务。
func (r *ProductRepository) WithPublication(ctx context.Context, id int64, change func(*model.Product, *model.Category) (*model.OutboxEvent, error)) (*model.Product, error) {
	var result model.Product
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&result).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrProductNotFound
			}
			return err
		}
		var category model.Category
		// 共享锁避免发布事务中的启用检查与分类停用并发穿透。
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("id = ?", result.CategoryID).Take(&category).Error; err != nil {
			return err
		}
		version := result.AggregateVersion
		event, err := change(&result, &category)
		if err != nil {
			return err
		}
		if event == nil {
			return nil
		}
		// 显式写状态字段，避免 Save 意外插入或覆盖商品其他资料。
		update := tx.Model(&model.Product{}).Where("id = ? AND aggregate_version = ?", id, version).Updates(map[string]any{"status": result.Status, "aggregate_version": result.AggregateVersion, "first_published_at": result.FirstPublishedAt, "updated_at": result.UpdatedAt})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return errors.New("商品版本更新失败")
		}
		// 必须使用同一个 tx，事件插入失败时已执行的商品更新也会回滚。
		return tx.Create(event).Error
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}
