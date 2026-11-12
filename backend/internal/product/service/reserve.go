package service

import (
	"context"
	"math"
	"remarket/backend/internal/product/events"
	"remarket/backend/internal/product/model"
	"remarket/backend/pkg/apperror"
	"time"
)

type ReserveInput struct {
	OrderID, ProductID, BuyerID                                 int64
	ExpectedSellerID, ExpectedPriceCent, ExpectedProductVersion int64
	ExpiresAt                                                   time.Time
}

// Reserve 校验首次请求快照并原子预占；历史记录优先比较完整参数，不因到期或终态误拒绝重试。
func (s *ReservationService) Reserve(ctx context.Context, input ReserveInput) (*model.ProductReservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input.OrderID <= 0 || input.ProductID <= 0 || input.BuyerID <= 0 || input.ExpectedSellerID <= 0 || input.ExpectedPriceCent <= 0 || input.ExpectedProductVersion <= 0 || input.ExpiresAt.IsZero() || input.ExpiresAt.Year() < 1000 || input.ExpiresAt.Year() > 9999 || input.ExpiresAt.Nanosecond()%1000 != 0 {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "预占参数或时间精度无效"}
	}
	return s.reservations.WithReserve(ctx, input.ProductID, input.OrderID, func(product *model.Product, existing *model.ProductReservation, now time.Time) (*model.ProductReservation, *model.OutboxEvent, error) {
		if existing != nil {
			if existing.ExpectedSellerID == nil || existing.ExpectedPriceCent == nil || existing.ExpectedProductVersion == nil {
				return nil, nil, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "历史预占缺少请求快照"}
			}
			if existing.ProductID != input.ProductID || existing.BuyerID != input.BuyerID || *existing.ExpectedSellerID != input.ExpectedSellerID || *existing.ExpectedPriceCent != input.ExpectedPriceCent || *existing.ExpectedProductVersion != input.ExpectedProductVersion || !existing.ExpiresAt.Equal(input.ExpiresAt) {
				return nil, nil, &apperror.Error{Kind: apperror.Conflict, Message: "同订单预占参数不一致"}
			}
			return existing, nil, nil
		}
		if product.Status != model.StatusOnSale || product.SellerID == input.BuyerID || product.SellerID != input.ExpectedSellerID || product.PriceCent != input.ExpectedPriceCent || product.AggregateVersion != input.ExpectedProductVersion || !input.ExpiresAt.After(now) || product.AggregateVersion == math.MaxInt64 {
			return nil, nil, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "商品状态、快照或预占期限不允许购买"}
		}
		reservation := &model.ProductReservation{ProductID: input.ProductID, OrderID: input.OrderID, BuyerID: input.BuyerID, ExpectedSellerID: &input.ExpectedSellerID, ExpectedPriceCent: &input.ExpectedPriceCent, ExpectedProductVersion: &input.ExpectedProductVersion, Status: model.ReservationActive, ExpiresAt: input.ExpiresAt.UTC(), CreatedAt: now, UpdatedAt: now}
		product.Status = model.StatusReserved
		product.AggregateVersion++
		product.UpdatedAt = now
		event, err := events.NewReservedOutbox(*product, now)
		return reservation, event, err
	})
}
