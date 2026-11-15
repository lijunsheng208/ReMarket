package repository

import (
	"context"
	"remarket/backend/internal/product/model"
	"time"
)

// ListFeed 按不可变创建时间和主键降序查询公开商品，卖家过滤只访问本服务数据。
func (r *ProductRepository) ListFeed(ctx context.Context, before time.Time, id int64, size int, sellers []int64) ([]model.Product, error) {
	rows := make([]model.Product, 0)
	q := r.db.WithContext(ctx).Where("status = ?", model.StatusOnSale)
	if id > 0 {
		q = q.Where("created_at < ? OR (created_at = ? AND id < ?)", before, before, id)
	}
	if len(sellers) > 0 {
		q = q.Where("seller_id IN ?", sellers)
	}
	err := q.Order("created_at DESC").Order("id DESC").Limit(size).Find(&rows).Error
	return rows, err
}
