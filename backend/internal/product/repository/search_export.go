package repository

import (
	"context"
	"remarket/backend/internal/product/model"
)

// SearchExportUpper 查询当前商品 ID 上界；调用方必须保证所有写入已暂停。
func (r *ProductRepository) SearchExportUpper(ctx context.Context) (int64, error) {
	var upper int64
	err := r.db.WithContext(ctx).Model(&model.Product{}).Select("COALESCE(MAX(id), 0)").Scan(&upper).Error
	return upper, err
}

// ExportSearch 扫描曾发布商品所有状态，不导出未发布草稿，不访问其他服务表。
func (r *ProductRepository) ExportSearch(ctx context.Context, after, upper int64, size int) ([]model.Product, error) {
	var rows []model.Product
	err := r.db.WithContext(ctx).Where("id > ? AND id <= ? AND first_published_at IS NOT NULL", after, upper).Order("id ASC").Limit(size).Find(&rows).Error
	return rows, err
}
