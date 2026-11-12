package service

import (
	"context"
	"math"
	"remarket/backend/internal/order/events"
	"remarket/backend/internal/order/model"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/auth"
	"time"
)

type DecisionRepository interface {
	WithDecision(context.Context, int64, int64, func(*model.Order, time.Time) (*model.OrderOperation, *model.OutboxEvent, error)) (*model.Order, error)
}
type DecisionService struct{ orders DecisionRepository }

// NewDecision 注入订单事务持久化，外部 RPC 由已持久化操作异步执行。
func NewDecision(orders DecisionRepository) *DecisionService { return &DecisionService{orders: orders} }

// Cancel 允许未到期 RESERVED 的任一参与人取消，持久化 CANCELLING 后不可反向确认。
func (s *DecisionService) Cancel(ctx context.Context, id int64) (*model.Order, error) {
	return s.decide(ctx, id, false)
}

// Confirm 记录当前参与人确认，双方均确认后持久化 CONFIRMING 决定。
func (s *DecisionService) Confirm(ctx context.Context, id int64) (*model.Order, error) {
	return s.decide(ctx, id, true)
}

// decide 在同一订单锁内检查权限、期限及当前状态；重复请求不增加版本或写入重复事件。
func (s *DecisionService) decide(ctx context.Context, id int64, confirm bool) (*model.Order, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok {
		return nil, &apperror.Error{Kind: apperror.Unauthenticated, Message: "请先登录"}
	}
	if id <= 0 {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "订单 ID 必须为正整数"}
	}
	return s.orders.WithDecision(ctx, id, identity.UserID, func(order *model.Order, now time.Time) (*model.OrderOperation, *model.OutboxEvent, error) {
		if !confirm && (order.Status == model.StatusCancelling || order.Status == model.StatusCancelled) {
			return nil, nil, nil
		}
		if confirm && (order.Status == model.StatusConfirming || order.Status == model.StatusCompleted) {
			return nil, nil, nil
		}
		if order.Status != model.StatusReserved {
			return nil, nil, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "当前订单状态不允许此操作"}
		}
		if confirm && ((identity.UserID == order.BuyerID && order.BuyerConfirmed) || (identity.UserID == order.SellerID && order.SellerConfirmed)) {
			return nil, nil, nil
		}
		if !now.Before(order.ExpiresAt) {
			return nil, nil, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "订单已到期"}
		}
		if order.AggregateVersion == math.MaxInt64 {
			return nil, nil, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "订单版本已达上限"}
		}
		operationType := model.OperationRelease
		eventType := "order.cancelling.v1"
		order.Status = model.StatusCancelling
		if confirm {
			order.Status = model.StatusReserved
			if identity.UserID == order.BuyerID {
				order.BuyerConfirmed = true
			} else {
				order.SellerConfirmed = true
			}
			eventType = "order.confirmation_recorded.v1"
			if order.BuyerConfirmed && order.SellerConfirmed {
				order.Status = model.StatusConfirming
				operationType = model.OperationConfirm
				eventType = "order.confirming.v1"
			}
		}
		order.AggregateVersion++
		order.UpdatedAt = now
		event, err := events.NewStateChanged(*order, now)
		if err != nil {
			return nil, nil, err
		}
		event.EventType = eventType
		if order.Status == model.StatusReserved {
			return nil, event, nil
		}
		operationID, err := events.NewID()
		if err != nil {
			return nil, nil, err
		}
		return &model.OrderOperation{OperationID: operationID, OrderID: order.ID, OperationType: operationType, Status: model.OperationPending, NextRetryAt: now, CreatedAt: now, UpdatedAt: now}, event, nil
	})
}
