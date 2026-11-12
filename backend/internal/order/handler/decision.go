package handler

import (
	"context"
	orderv1 "remarket/backend/gen/order/v1"
	"remarket/backend/internal/order/model"
	"remarket/backend/pkg/apperror"
)

type Decisions interface {
	Cancel(context.Context, int64) (*model.Order, error)
	Confirm(context.Context, int64) (*model.Order, error)
}

// WithDecisions 注入取消和确认能力，RPC 返回持久化决定而非伪造成交或释放已完成。
func (s *Server) WithDecisions(decisions Decisions) *Server { s.decisions = decisions; return s }

// CancelOrder 取消订单，由业务层校验参与者和期限，实际释放由恢复执行器完成。
func (s *Server) CancelOrder(ctx context.Context, req *orderv1.CancelOrderRequest) (*orderv1.OrderResponse, error) {
	if s.decisions == nil {
		return s.UnimplementedOrderServiceServer.CancelOrder(ctx, req)
	}
	if req == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "取消请求不能为空"}
	}
	order, err := s.decisions.Cancel(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	result, err := orderProto(order)
	if err != nil {
		return nil, err
	}
	return &orderv1.OrderResponse{Order: result}, nil
}

// ConfirmOrder 记录当前参与者确认，第二方确认后由执行器完成商品成交。
func (s *Server) ConfirmOrder(ctx context.Context, req *orderv1.ConfirmOrderRequest) (*orderv1.OrderResponse, error) {
	if s.decisions == nil {
		return s.UnimplementedOrderServiceServer.ConfirmOrder(ctx, req)
	}
	if req == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "确认请求不能为空"}
	}
	order, err := s.decisions.Confirm(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	result, err := orderProto(order)
	if err != nil {
		return nil, err
	}
	return &orderv1.OrderResponse{Order: result}, nil
}
