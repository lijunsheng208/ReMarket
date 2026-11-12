package client

import (
	"context"
	"errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/order/model"
	"remarket/backend/internal/order/service"
	"strings"
	"time"
)

type Reservations struct {
	products productv1.ProductServiceClient
	token    string
}

// NewReservations 校验独立服务凭据，禁止空值、弱值和 JWT 密钥复用，不回显凭据。
func NewReservations(products productv1.ProductServiceClient, token, jwtSecret string) (*Reservations, error) {
	if len(token) < 32 || strings.ContainsAny(token, " \t\r\n") || token == jwtSecret {
		return nil, errors.New("订单预占执行器需要独立且至少 32 字节的服务凭据")
	}
	return &Reservations{products: products, token: token}, nil
}

// callContext 从任务上下文派生单次两秒期限，显式覆盖用户凭据以防内部 RPC 使用用户 Token。
func (r *Reservations) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set("authorization", "Bearer "+r.token)
	return context.WithTimeout(metadata.NewOutgoingContext(ctx, md), 2*time.Second)
}

// Lookup 查询实际预占结果，NotFound 只表示当前未找到，不证明远程执行失败。
func (r *Reservations) Lookup(ctx context.Context, id int64) (*service.ReservationState, error) {
	call, cancel := r.callContext(ctx)
	defer cancel()
	result, err := r.products.GetReservation(call, &productv1.GetReservationRequest{OrderId: id})
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return reservationState(result)
}

// Reserve 仅复用持久化参数，超时保留未知结果，确定业务拒绝转换为领域标记。
func (r *Reservations) Reserve(ctx context.Context, order model.Order) (*service.ReservationState, error) {
	if order.ExpectedProductVersion == nil {
		return nil, errors.New("缺少预占版本")
	}
	call, cancel := r.callContext(ctx)
	defer cancel()
	result, err := r.products.ReserveProduct(call, &productv1.ReserveProductRequest{OrderId: order.ID, ProductId: order.ProductID, BuyerId: order.BuyerID, ExpectedSellerId: order.SellerID, ExpectedPriceCent: order.PriceCent, ExpectedProductVersion: *order.ExpectedProductVersion, ExpiresAt: timestamppb.New(order.ExpiresAt)})
	if status.Code(err) == codes.FailedPrecondition || status.Code(err) == codes.NotFound {
		return nil, service.ErrReserveRejected
	}
	if err != nil {
		return nil, err
	}
	return reservationState(result)
}

// reservationState 校验下游结构并转换为领域值，不向上层传递 Proto 或内部错误内容。
func reservationState(result *productv1.ReservationResponse) (*service.ReservationState, error) {
	if result == nil || result.Reservation == nil || result.Reservation.ExpiresAt == nil || result.Reservation.ExpiresAt.CheckValid() != nil {
		return nil, errors.New("预占响应结构无效")
	}
	r := result.Reservation
	states := map[productv1.ReservationStatus]string{productv1.ReservationStatus_RESERVATION_STATUS_ACTIVE: "ACTIVE", productv1.ReservationStatus_RESERVATION_STATUS_RELEASED: "RELEASED", productv1.ReservationStatus_RESERVATION_STATUS_CONFIRMED: "CONFIRMED"}
	state, ok := states[r.Status]
	if !ok {
		return nil, errors.New("预占响应状态无效")
	}
	return &service.ReservationState{OrderID: r.OrderId, ProductID: r.ProductId, BuyerID: r.BuyerId, Status: state, ExpiresAt: r.ExpiresAt.AsTime()}, nil
}

// Release 执行已持久化关闭决定，错误保持未知结果，由执行器查询和重试。
func (r *Reservations) Release(ctx context.Context, order model.Order) (*service.ReservationState, error) {
	call, cancel := r.callContext(ctx)
	defer cancel()
	result, err := r.products.ReleaseReservation(call, &productv1.ReleaseReservationRequest{OrderId: order.ID, ProductId: order.ProductID})
	if err != nil {
		return nil, err
	}
	return reservationState(result)
}

// Confirm 执行已持久化双方确认决定，不因当前时间过期反向释放。
func (r *Reservations) Confirm(ctx context.Context, order model.Order) (*service.ReservationState, error) {
	call, cancel := r.callContext(ctx)
	defer cancel()
	result, err := r.products.ConfirmReservation(call, &productv1.ConfirmReservationRequest{OrderId: order.ID, ProductId: order.ProductID})
	if err != nil {
		return nil, err
	}
	return reservationState(result)
}
