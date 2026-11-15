// Package handler 将 Search RPC 转为业务请求，不持有检索规则。
package handler

import (
	"context"
	"google.golang.org/protobuf/types/known/timestamppb"
	searchv1 "remarket/backend/gen/search/v1"
	"remarket/backend/internal/search/repository"
	"remarket/backend/internal/search/service"
	"remarket/backend/pkg/apperror"
)

// Handler 提供公开只读搜索，不从身份头构造用户权限。
type Handler struct {
	searchv1.UnimplementedSearchServiceServer
	service *service.Service
}

// New 注入完整查询业务。
func New(s *service.Service) *Handler { return &Handler{service: s} }

// SearchProducts 转换协议枚举与结果，业务错误交给公共拦截器映射。
func (h *Handler) SearchProducts(ctx context.Context, r *searchv1.SearchProductsRequest) (*searchv1.SearchProductsResponse, error) {
	if r == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "搜索请求无效"}
	}
	sorts := map[searchv1.SearchSort]string{0: "", 1: "relevance", 2: "newest", 3: "price_asc", 4: "price_desc"}
	sort, ok := sorts[r.Sort]
	if !ok {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "排序无效"}
	}
	result, err := h.service.Search(ctx, service.Request{Query: repository.Query{Text: r.Query, CategoryID: r.CategoryId, MinPrice: r.MinPriceCent, MaxPrice: r.MaxPriceCent, Sort: sort, Size: int(r.PageSize)}, Cursor: r.Cursor})
	if err != nil {
		return nil, err
	}
	response := &searchv1.SearchProductsResponse{NextCursor: result.Cursor}
	for _, d := range result.Products {
		response.Products = append(response.Products, &searchv1.SearchProduct{Id: d.ProductID, SellerId: d.SellerID, CategoryId: d.CategoryID, Title: d.Title, Description: d.Description, PriceCent: d.PriceCent, AggregateVersion: d.AggregateVersion, FirstPublishedAt: timestamppb.New(d.FirstPublishedAt)})
	}
	return response, nil
}
