package repository

import (
	"context"
	"database/sql"
	"errors"
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

// GetDetailSnapshot 在同一只读快照加载商品和图片，不承担可见性判断。
func (r *ProductRepository) GetDetailSnapshot(ctx context.Context, id int64) (*model.Product, []model.ProductImage, error) {
	var product model.Product
	images := make([]model.ProductImage, 0)
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id = ?", id).Take(&product).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrProductNotFound
			}
			return err
		}
		return tx.Where("product_id = ?", id).Order("sort_order ASC").Order("id ASC").Find(&images).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	return &product, images, nil
}

// CountBySeller 单条聚合查询统计卖家所有商品，不应用分页或分类筛选，空集合返回零。
func (r *ProductRepository) CountBySeller(ctx context.Context, sellerID int64) (*model.ProductStatistics, error) {
	var counts model.ProductStatistics
	err := r.db.WithContext(ctx).Model(&model.Product{}).Select("COUNT(*) AS total, COALESCE(SUM(status = 'ON_SALE'),0) AS on_sale, COALESCE(SUM(status = 'SOLD'),0) AS sold, COALESCE(SUM(status = 'OFF_SHELF'),0) AS off_shelf").Where("seller_id = ?", sellerID).Scan(&counts).Error
	return &counts, err
}
