// Package handler 适配 Feed RPC 与业务查询。
package handler

import (
	"context"
	feedv1 "remarket/backend/gen/feed/v1"
	"remarket/backend/internal/feed/service"
)

type Server struct {
	feedv1.UnimplementedFeedServiceServer
	query *service.Service
}

// New 注入 Feed 查询业务。
func New(query *service.Service) *Server { return &Server{query: query} }

// Home 返回公开首页商品流。
func (s *Server) Home(ctx context.Context, r *feedv1.ListFeedRequest) (*feedv1.ListFeedResponse, error) {
	return s.query.List(ctx, r, false)
}

// Following 返回当前已验证用户关注卖家的商品流。
func (s *Server) Following(ctx context.Context, r *feedv1.ListFeedRequest) (*feedv1.ListFeedResponse, error) {
	return s.query.List(ctx, r, true)
}
