package handler

import (
	"context"
	"google.golang.org/protobuf/types/known/timestamppb"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/product/model"
	"remarket/backend/internal/product/service"
	"remarket/backend/pkg/apperror"
)

type ReservationManager interface {
	Reserve(context.Context, service.ReserveInput) (*model.ProductReservation, error)
	Get(context.Context, int64) (*model.ProductReservation, error)
	Release(context.Context, int64, int64) (*model.ProductReservation, error)
	Confirm(context.Context, int64, int64) (*model.ProductReservation, error)
}

// WithReservations 注入商品预占、查询及终结能力。
func (s *Server) WithReservations(manager ReservationManager) *Server {
	s.reservations = manager
	return s
}

// GetReservation 将内部查询交给预占 Service，身份由专用服务鉴权拦截器验证。
func (s *Server) GetReservation(ctx context.Context, req *productv1.GetReservationRequest) (*productv1.ReservationResponse, error) {
	if s.reservations == nil {
		return s.UnimplementedProductServiceServer.GetReservation(ctx, req)
	}
	if req == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "查询请求不能为空"}
	}
	result, err := s.reservations.Get(ctx, req.OrderId)
	if err != nil {
		return nil, err
	}
	return reservationResponse(result), nil
}

// ReleaseReservation 转发订单和商品标识，不提供按商品批量释放入口。
func (s *Server) ReleaseReservation(ctx context.Context, req *productv1.ReleaseReservationRequest) (*productv1.ReservationResponse, error) {
	if s.reservations == nil {
		return s.UnimplementedProductServiceServer.ReleaseReservation(ctx, req)
	}
	if req == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "释放请求不能为空"}
	}
	result, err := s.reservations.Release(ctx, req.OrderId, req.ProductId)
	if err != nil {
		return nil, err
	}
	return reservationResponse(result), nil
}

// ConfirmReservation 执行 Order 的成交决定，返回实际预占状态。
func (s *Server) ConfirmReservation(ctx context.Context, req *productv1.ConfirmReservationRequest) (*productv1.ReservationResponse, error) {
	if s.reservations == nil {
		return s.UnimplementedProductServiceServer.ConfirmReservation(ctx, req)
	}
	if req == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "确认请求不能为空"}
	}
	result, err := s.reservations.Confirm(ctx, req.OrderId, req.ProductId)
	if err != nil {
		return nil, err
	}
	return reservationResponse(result), nil
}

// reservationResponse 显式映射状态和可空终态时间，保留数据库微秒精度。
func reservationResponse(r *model.ProductReservation) *productv1.ReservationResponse {
	status := map[model.ReservationStatus]productv1.ReservationStatus{model.ReservationActive: productv1.ReservationStatus_RESERVATION_STATUS_ACTIVE, model.ReservationReleased: productv1.ReservationStatus_RESERVATION_STATUS_RELEASED, model.ReservationConfirmed: productv1.ReservationStatus_RESERVATION_STATUS_CONFIRMED}[r.Status]
	result := &productv1.Reservation{Id: r.ID, OrderId: r.OrderID, ProductId: r.ProductID, BuyerId: r.BuyerID, Status: status, ExpiresAt: timestamppb.New(r.ExpiresAt), CreatedAt: timestamppb.New(r.CreatedAt), UpdatedAt: timestamppb.New(r.UpdatedAt)}
	if r.ConfirmedAt != nil {
		result.ConfirmedAt = timestamppb.New(*r.ConfirmedAt)
	}
	if r.ReleasedAt != nil {
		result.ReleasedAt = timestamppb.New(*r.ReleasedAt)
	}
	return &productv1.ReservationResponse{Reservation: result}
}

// ReserveProduct 校验 Timestamp 后传递完整快照，调用身份由独立服务鉴权验证。
func (s *Server) ReserveProduct(ctx context.Context, req *productv1.ReserveProductRequest) (*productv1.ReservationResponse, error) {
	if s.reservations == nil {
		return s.UnimplementedProductServiceServer.ReserveProduct(ctx, req)
	}
	if req == nil || req.ExpiresAt == nil || req.ExpiresAt.CheckValid() != nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "预占请求或截止时间无效"}
	}
	result, err := s.reservations.Reserve(ctx, service.ReserveInput{OrderID: req.OrderId, ProductID: req.ProductId, BuyerID: req.BuyerId, ExpectedSellerID: req.ExpectedSellerId, ExpectedPriceCent: req.ExpectedPriceCent, ExpectedProductVersion: req.ExpectedProductVersion, ExpiresAt: req.ExpiresAt.AsTime()})
	if err != nil {
		return nil, err
	}
	return reservationResponse(result), nil
}
