// Package handler 将订单 gRPC 请求适配为业务调用，不处理数据库或跨服务编排。
package handler

import (
	"context"
	"errors"
	"google.golang.org/protobuf/types/known/timestamppb"
	orderv1 "remarket/backend/gen/order/v1"
	"remarket/backend/internal/order/model"
	"remarket/backend/pkg/apperror"
)

type Query interface {
	Get(context.Context, int64) (*model.Order, error)
	List(context.Context, string, int32, int32) ([]model.Order, int64, error)
}
type Server struct {
	orderv1.UnimplementedOrderServiceServer
	decisions Decisions
	creator   Creator
	query     Query
}

// New 注入查询业务；未注入的写操作明确返回 Unimplemented。
func New(query Query) *Server { return &Server{query: query} }

// GetOrder 获取当前用户参与的订单，拒绝空请求。
func (s *Server) GetOrder(ctx context.Context, req *orderv1.GetOrderRequest) (*orderv1.OrderResponse, error) {
	if req == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "查询请求不能为空"}
	}
	result, err := s.query.Get(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	order, err := orderProto(result)
	if err != nil {
		return nil, err
	}
	return &orderv1.OrderResponse{Order: order}, nil
}

// ListOrders 显式转换角色枚举，未知值交由业务校验，结果不暴露幂等键与请求摘要。
func (s *Server) ListOrders(ctx context.Context, req *orderv1.ListOrdersRequest) (*orderv1.ListOrdersResponse, error) {
	if req == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "列表请求不能为空"}
	}
	role := ""
	if req.Role == orderv1.OrderRole_ORDER_ROLE_BUYER {
		role = "buyer"
	}
	if req.Role == orderv1.OrderRole_ORDER_ROLE_SELLER {
		role = "seller"
	}
	orders, total, err := s.query.List(ctx, role, req.Page, req.PageSize)
	if err != nil {
		return nil, err
	}
	result := &orderv1.ListOrdersResponse{Orders: make([]*orderv1.Order, 0, len(orders)), Total: total}
	for i := range orders {
		order, err := orderProto(&orders[i])
		if err != nil {
			return nil, err
		}
		result.Orders = append(result.Orders, order)
	}
	return result, nil
}

// orderProto 映射业务状态和可空关闭时间，未知持久化状态返回内部错误而非伪造成功。
func orderProto(order *model.Order) (*orderv1.Order, error) {
	states := map[model.OrderStatus]orderv1.OrderStatus{
		model.StatusPending:    orderv1.OrderStatus_ORDER_STATUS_PENDING,
		model.StatusReserved:   orderv1.OrderStatus_ORDER_STATUS_RESERVED,
		model.StatusConfirming: orderv1.OrderStatus_ORDER_STATUS_CONFIRMING,
		model.StatusCancelling: orderv1.OrderStatus_ORDER_STATUS_CANCELLING,
		model.StatusExpiring:   orderv1.OrderStatus_ORDER_STATUS_EXPIRING,
		model.StatusCompleted:  orderv1.OrderStatus_ORDER_STATUS_COMPLETED,
		model.StatusCancelled:  orderv1.OrderStatus_ORDER_STATUS_CANCELLED,
		model.StatusExpired:    orderv1.OrderStatus_ORDER_STATUS_EXPIRED,
		model.StatusFailed:     orderv1.OrderStatus_ORDER_STATUS_FAILED,
	}
	state, ok := states[order.Status]
	if !ok {
		return nil, errors.New("订单状态无效")
	}
	result := &orderv1.Order{Id: order.ID, BuyerId: order.BuyerID, SellerId: order.SellerID, ProductId: order.ProductID, PriceCent: order.PriceCent, Status: state, BuyerConfirmed: order.BuyerConfirmed, SellerConfirmed: order.SellerConfirmed, AggregateVersion: order.AggregateVersion, ExpiresAt: timestamppb.New(order.ExpiresAt), CreatedAt: timestamppb.New(order.CreatedAt), UpdatedAt: timestamppb.New(order.UpdatedAt)}
	if order.ClosedAt != nil {
		result.ClosedAt = timestamppb.New(*order.ClosedAt)
	}
	return result, nil
}
