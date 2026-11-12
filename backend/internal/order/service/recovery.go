package service

import (
	"context"
	"errors"
	"log/slog"
	"remarket/backend/internal/order/model"
	"time"
)

var ErrReserveRejected = errors.New("商品明确拒绝预占")

type RecoveryJob struct {
	Order     model.Order
	Operation model.OrderOperation
}
type RecoveryResult struct {
	Status     model.OperationStatus
	ErrorCode  string
	RetryDelay time.Duration
}
type RecoveryRepository interface {
	ClaimReserve(context.Context) (*RecoveryJob, error)
	FinishReserve(context.Context, *RecoveryJob, RecoveryResult) error
}
type ReservationState struct {
	OrderID, ProductID, BuyerID int64
	Status                      string
	ExpiresAt                   time.Time
}
type ReservationClient interface {
	Lookup(context.Context, int64) (*ReservationState, error)
	Reserve(context.Context, model.Order) (*ReservationState, error)
}
type RecoveryService struct {
	orders   RecoveryRepository
	products ReservationClient
}

// NewRecovery 注入持久化任务和 Product 访问，不依赖 gRPC 状态或数据库具体实现。
func NewRecovery(orders RecoveryRepository, products ReservationClient) *RecoveryService {
	return &RecoveryService{orders: orders, products: products}
}

// RecoverOne 领取单个 RESERVE 操作，先查真实结果，结果未知时只安排重试，不推断失败。
func (s *RecoveryService) RecoverOne(ctx context.Context) (bool, error) {
	job, err := s.orders.ClaimReserve(ctx)
	if err != nil || job == nil {
		return false, err
	}
	result := RecoveryResult{Status: model.OperationRetry, ErrorCode: "REMOTE_UNKNOWN", RetryDelay: retryDelay(job.Operation.RetryCount)}
	if job.Order.ExpectedProductVersion == nil {
		result.ErrorCode = "MISSING_SNAPSHOT"
		slog.ErrorContext(ctx, "订单恢复需要人工检查", "order_id", job.Order.ID, "code", result.ErrorCode)
		return true, s.orders.FinishReserve(ctx, job, result)
	}
	state, err := s.products.Lookup(ctx, job.Order.ID)
	if err == nil && state == nil {
		state, err = s.products.Reserve(ctx, job.Order)
		// 超时可能已提交，使用新的子调用期限查询；父 Context 已取消时留待租约恢复。
		if err != nil {
			rejected := errors.Is(err, ErrReserveRejected)
			observed, queryErr := s.products.Lookup(ctx, job.Order.ID)
			if queryErr == nil && observed != nil {
				state, err = observed, nil
			} else if rejected && queryErr == nil {
				result.Status = model.OperationFailed
				result.ErrorCode = "RESERVE_REJECTED"
				return true, s.orders.FinishReserve(ctx, job, result)
			}
		}
	}
	if err == nil && state != nil {
		if state.OrderID == job.Order.ID && state.ProductID == job.Order.ProductID && state.BuyerID == job.Order.BuyerID && state.ExpiresAt.Equal(job.Order.ExpiresAt) && state.Status == "ACTIVE" {
			result.Status = model.OperationSucceeded
			result.ErrorCode = ""
		} else {
			result.ErrorCode = "REMOTE_STATE_CONFLICT"
			slog.ErrorContext(ctx, "订单恢复需要人工检查", "order_id", job.Order.ID, "code", result.ErrorCode)
		}
	}
	return true, s.orders.FinishReserve(ctx, job, result)
}

// retryDelay 对连续失败指数退避并封顶一分钟，不因达到重试次数上限伪造远程失败。
func retryDelay(attempt int32) time.Duration {
	if attempt > 5 {
		return time.Minute
	}
	if attempt < 0 {
		attempt = 0
	}
	return time.Second * time.Duration(1<<uint(attempt))
}

// Run 启动即扫描持久化操作，每轮最多处理二十条；取消后退出，由租约承接未完成任务。
func (s *RecoveryService) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		for i := 0; i < 20 && ctx.Err() == nil; i++ {
			worked, err := s.RecoverOne(ctx)
			if err != nil {
				if ctx.Err() == nil {
					slog.ErrorContext(ctx, "订单恢复执行失败", "code", "RECOVERY_STORAGE_ERROR")
				}
				break
			}
			if !worked {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
