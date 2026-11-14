package service

import (
	"context"
	"errors"
	"math"
	"remarket/backend/internal/product/events"
	"remarket/backend/internal/product/model"
	"remarket/backend/pkg/apperror"
	"time"
)

type ReservationRepository interface {
	WithReserve(context.Context, int64, int64, func(*model.Product, *model.ProductReservation, time.Time) (*model.ProductReservation, *model.OutboxEvent, error)) (*model.ProductReservation, error)
	GetReservation(context.Context, int64) (*model.ProductReservation, error)
	WithReservation(context.Context, int64, int64, func(*model.Product, *model.ProductReservation) (*model.OutboxEvent, error)) (*model.ProductReservation, error)
}
type ReservationService struct {
	reservations ReservationRepository
	invalidation PostCommit
}

// NewReservations 注入预占持久化；服务调用身份由 RPC 边界独立验证，不建立虚假用户身份。
func NewReservations(reservations ReservationRepository) *ReservationService {
	return &ReservationService{reservations: reservations}
}

// Get 返回订单的实际预占状态，不根据时间隐藏记录。
func (s *ReservationService) Get(ctx context.Context, orderID int64) (*model.ProductReservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if orderID <= 0 {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "订单 ID 必须为正整数"}
	}
	return s.reservations.GetReservation(ctx, orderID)
}

// Release 执行 Order 已持久化的释放决定；重复释放保留原结果，不能释放成交预占。
func (s *ReservationService) Release(ctx context.Context, orderID, productID int64) (*model.ProductReservation, error) {
	return s.finish(ctx, orderID, productID, false)
}

// Confirm 执行截止前由 Order 持久化的成交决定，不因 RPC 到达较晚自动释放预占。
func (s *ReservationService) Confirm(ctx context.Context, orderID, productID int64) (*model.ProductReservation, error) {
	return s.finish(ctx, orderID, productID, true)
}

// finish 在持有商品和预占行锁的回调内判断状态，确认与释放只能有一个终态胜出。
func (s *ReservationService) finish(ctx context.Context, orderID, productID int64, confirm bool) (*model.ProductReservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if orderID <= 0 || productID <= 0 {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "订单和商品 ID 必须为正整数"}
	}
	result, err := s.reservations.WithReservation(ctx, orderID, productID, func(product *model.Product, reservation *model.ProductReservation) (*model.OutboxEvent, error) {
		target := model.ReservationReleased
		if confirm {
			target = model.ReservationConfirmed
		}
		if reservation.Status == target {
			// 已释放商品可以被新订单预占，不应以当前商品状态否定历史释放的幂等结果。
			return nil, nil
		}
		if reservation.Status != model.ReservationActive {
			return nil, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "预占已进入其他终态"}
		}
		if product.Status != model.StatusReserved {
			return nil, errors.New("商品与预占状态不一致")
		}
		if product.AggregateVersion == math.MaxInt64 {
			return nil, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "商品版本已达上限"}
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		reservation.Status = target
		reservation.UpdatedAt = now
		product.AggregateVersion++
		product.UpdatedAt = now
		if confirm {
			product.Status = model.StatusSold
			reservation.ConfirmedAt = &now
			return events.NewSoldOutbox(*product, now)
		}
		product.Status = model.StatusOnSale
		reservation.ReleasedAt = &now
		return events.NewReleasedOutbox(*product, now)
	})
	notifyCommit(ctx, s.invalidation, productID, err)
	return result, err
}

// WithInvalidation 返回带提交后失效器的独立服务副本，应在启动组装阶段调用。
func (s *ReservationService) WithInvalidation(hook PostCommit) *ReservationService {
	copy := *s
	copy.invalidation = hook
	return &copy
}
