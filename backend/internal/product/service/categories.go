package service

import (
	"context"
	"remarket/backend/internal/product/model"
)

type CategoryRepository interface {
	ListEnabledCategories(context.Context) ([]model.Category, error)
}
type CategoryService struct{ categories CategoryRepository }

// NewCategories 注入分类仓储，保持 Handler 到 Repository 之间经过业务层。
func NewCategories(categories CategoryRepository) *CategoryService {
	return &CategoryService{categories: categories}
}

// ListCategories 查询启用分类并保留调用方取消和超时。
func (s *CategoryService) ListCategories(ctx context.Context) ([]model.Category, error) {
	return s.categories.ListEnabledCategories(ctx)
}
