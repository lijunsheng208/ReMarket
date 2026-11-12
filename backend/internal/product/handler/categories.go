package handler

import (
	"context"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/product/model"
)

type CategoryLister interface {
	ListCategories(context.Context) ([]model.Category, error)
}

// WithCategories 在组装阶段注入分类读取能力，不提前扩展其他业务。
func (s *Server) WithCategories(categories CategoryLister) *Server {
	s.categories = categories
	return s
}

// ListCategories 返回启用且按展示顺序排列的分类。
func (s *Server) ListCategories(ctx context.Context, req *productv1.ListCategoriesRequest) (*productv1.ListCategoriesResponse, error) {
	if s.categories == nil {
		return s.UnimplementedProductServiceServer.ListCategories(ctx, req)
	}
	categories, err := s.categories.ListCategories(ctx)
	if err != nil {
		return nil, err
	}
	result := &productv1.ListCategoriesResponse{Categories: make([]*productv1.Category, 0, len(categories))}
	for _, c := range categories {
		result.Categories = append(result.Categories, &productv1.Category{Id: c.ID, Code: c.Code, Name: c.Name, SortOrder: c.SortOrder})
	}
	return result, nil
}
