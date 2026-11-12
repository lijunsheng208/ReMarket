package repository

import (
	"context"
	"database/sql"
	"gorm.io/gorm"
	"remarket/backend/internal/product/model"
)

// ListOnSale 使用同一只读快照统计和分页，确保当前响应的总数与过滤条件一致。
func (r *ProductRepository) ListOnSale(ctx context.Context, categoryID int64, offset, limit int) ([]model.Product, int64, error) {
	products := make([]model.Product, 0)
	var total int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&model.Product{}).Where("status = ?", model.StatusOnSale)
		if categoryID > 0 {
			query = query.Where("category_id = ?", categoryID)
		}
		if err := query.Count(&total).Error; err != nil {
			return err
		}
		return query.Order("first_published_at DESC").Order("id DESC").Offset(offset).Limit(limit).Find(&products).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	return products, total, nil
}

// ListBySeller 在同一快照中查询当前卖家的全部状态商品，不接受客户端授权判断。
func (r *ProductRepository) ListBySeller(ctx context.Context, sellerID, categoryID int64, offset, limit int) ([]model.Product, int64, error) {
	products := make([]model.Product, 0)
	var total int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&model.Product{}).Where("seller_id = ?", sellerID)
		if categoryID > 0 {
			query = query.Where("category_id = ?", categoryID)
		}
		if err := query.Count(&total).Error; err != nil {
			return err
		}
		return query.Order("created_at DESC").Order("id DESC").Offset(offset).Limit(limit).Find(&products).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	return products, total, nil
}

// ListImages 按商品内排序返回图片对象键，不包含签名 URL。
func (r *ProductRepository) ListImages(ctx context.Context, id int64) ([]model.ProductImage, error) {
	images := make([]model.ProductImage, 0)
	err := r.db.WithContext(ctx).Where("product_id = ?", id).Order("sort_order ASC").Order("id ASC").Find(&images).Error
	return images, err
}
