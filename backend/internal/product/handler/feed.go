package handler

import (
	"context"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/product/service"
)

// WithFeed 注入公开商品游标业务。
func (s *Server) WithFeed(query *service.FeedQuery) *Server { s.feed = query; return s }

// ListFeedProducts 返回仅在售的商品，游标和卖家过滤交由业务校验。
func (s *Server) ListFeedProducts(ctx context.Context, r *productv1.ListFeedProductsRequest) (*productv1.ListFeedProductsResponse, error) {
	if s.feed == nil {
		return s.UnimplementedProductServiceServer.ListFeedProducts(ctx, r)
	}
	rows, next, err := s.feed.List(ctx, r.GetCursor(), int(r.GetPageSize()), r.GetSellerIds())
	if err != nil {
		return nil, err
	}
	out := &productv1.ListFeedProductsResponse{NextCursor: next}
	for i := range rows {
		out.Products = append(out.Products, productProto(&rows[i]))
	}
	return out, nil
}
