package handler

import (
	"context"
	orderv1 "remarket/backend/gen/order/v1"
	"remarket/backend/internal/order/model"
	"remarket/backend/pkg/apperror"
)

type Creator interface {
	Create(context.Context, int64, string) (*model.Order, error)
}

// WithCreator 注入创建意图业务，返回 PENDING 不表示商品已预占。
func (s *Server) WithCreator(creator Creator) *Server { s.creator = creator; return s }

// CreateOrder 持久化幂等创建请求，金额、卖家和买家均不取自客户端声明。
func (s *Server) CreateOrder(ctx context.Context, req *orderv1.CreateOrderRequest) (*orderv1.OrderResponse, error) {
	if s.creator == nil {
		return s.UnimplementedOrderServiceServer.CreateOrder(ctx, req)
	}
	if req == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "创建请求不能为空"}
	}
	result, err := s.creator.Create(ctx, req.ProductId, req.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	order, err := orderProto(result)
	if err != nil {
		return nil, err
	}
	return &orderv1.OrderResponse{Order: order}, nil
}
