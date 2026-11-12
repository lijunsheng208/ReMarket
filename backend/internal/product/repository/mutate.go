package repository

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"remarket/backend/internal/product/model"
)

// WithMutation 锁定商品及目标分类，按 Service 返回的 changed 决定是否保存，可无事件地编辑草稿。
func (r *ProductRepository) WithMutation(ctx context.Context, id, targetCategory int64, change func(*model.Product, *model.Category) (*model.OutboxEvent, bool, error)) (*model.Product, error) {
	var product model.Product
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&product).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrProductNotFound
			}
			return err
		}
		categoryID := product.CategoryID
		if targetCategory > 0 {
			categoryID = targetCategory
		}
		var category model.Category
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("id = ?", categoryID).Take(&category).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrCategoryReference
			}
			return err
		}
		version := product.AggregateVersion
		event, changed, err := change(&product, &category)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		// 显式更新允许编辑的列，不覆盖卖家或图片；版本条件作为额外防线。
		update := tx.Model(&model.Product{}).Where("id = ? AND aggregate_version = ?", id, version).Updates(map[string]any{"category_id": product.CategoryID, "title": product.Title, "description": product.Description, "price_cent": product.PriceCent, "status": product.Status, "aggregate_version": product.AggregateVersion, "updated_at": product.UpdatedAt})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return errors.New("商品版本更新失败")
		}
		if event != nil {
			return tx.Create(event).Error
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &product, nil
}
