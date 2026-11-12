package repository

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"math"
	"remarket/backend/internal/order/events"
	"remarket/backend/internal/order/model"
	"remarket/backend/internal/order/service"
	"time"
)

// ClaimReserve 先无锁选候选，再按订单→操作加锁并重查资格；多实例不重复持有有效租约。
func (r *Repository) ClaimReserve(ctx context.Context) (*service.RecoveryJob, error) {
	var candidates []model.OrderOperation
	eligible := "operation_type = ? AND ((status IN ? AND next_retry_at <= UTC_TIMESTAMP(6)) OR (status = ? AND locked_until <= UTC_TIMESTAMP(6)))"
	if err := r.db.WithContext(ctx).Where("order_id IN (SELECT id FROM orders WHERE status = ?)", model.StatusPending).Where(eligible, model.OperationReserve, []model.OperationStatus{model.OperationPending, model.OperationRetry}, model.OperationProcessing).Order("id").Limit(20).Find(&candidates).Error; err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		var job *service.RecoveryJob
		err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var order model.Order
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("id = ? AND status = ?", candidate.OrderID, model.StatusPending).Take(&order).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil
				}
				return err
			}
			var operation model.OrderOperation
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", candidate.ID).Where(eligible, model.OperationReserve, []model.OperationStatus{model.OperationPending, model.OperationRetry}, model.OperationProcessing).Take(&operation).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil
				}
				return err
			}
			token, err := events.NewID()
			if err != nil {
				return err
			}
			var now time.Time
			if err := tx.Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&now); err != nil {
				return err
			}
			until := now.Add(time.Minute)
			if err := tx.Model(&model.OrderOperation{}).Where("id = ?", operation.ID).Updates(map[string]any{"status": model.OperationProcessing, "lock_token": token, "locked_until": until, "updated_at": now}).Error; err != nil {
				return err
			}
			operation.Status = model.OperationProcessing
			operation.LockToken = &token
			operation.LockedUntil = &until
			job = &service.RecoveryJob{Order: order, Operation: operation}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if job != nil {
			return job, nil
		}
	}
	return nil, nil
}

// FinishReserve 只有当前未过期租约可以落地结果；订单、操作和状态事件在一个事务提交。
func (r *Repository) FinishReserve(ctx context.Context, job *service.RecoveryJob, result service.RecoveryResult) error {
	if result.Status != model.OperationRetry && result.Status != model.OperationSucceeded && result.Status != model.OperationFailed {
		return errors.New("非法恢复结果")
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var order model.Order
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", job.Order.ID).Take(&order).Error; err != nil {
			return err
		}
		if order.Status != model.StatusPending {
			return nil
		}
		var operation model.OrderOperation
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND status = ? AND lock_token = ? AND locked_until > UTC_TIMESTAMP(6)", job.Operation.ID, model.OperationProcessing, job.Operation.LockToken).Take(&operation).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var now time.Time
		if err := tx.Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&now); err != nil {
			return err
		}
		updates := map[string]any{"status": result.Status, "lock_token": nil, "locked_until": nil, "updated_at": now, "last_error": nil}
		if result.Status == model.OperationRetry {
			retries := operation.RetryCount
			if retries < math.MaxInt32 {
				retries++
			}
			updates["retry_count"] = retries
			updates["next_retry_at"] = now.Add(result.RetryDelay)
			updates["last_error"] = result.ErrorCode
		} else {
			if order.AggregateVersion == math.MaxInt64 {
				return errors.New("订单版本已达上限")
			}
			order.Status = model.StatusReserved
			if result.Status == model.OperationFailed {
				order.Status = model.StatusFailed
				order.ClosedAt = &now
				updates["last_error"] = result.ErrorCode
			}
			order.AggregateVersion++
			order.UpdatedAt = now
			if err := tx.Model(&model.Order{}).Where("id = ?", order.ID).Updates(map[string]any{"status": order.Status, "aggregate_version": order.AggregateVersion, "closed_at": order.ClosedAt, "updated_at": now}).Error; err != nil {
				return err
			}
			event, err := events.NewStateChanged(order, now)
			if err != nil {
				return err
			}
			if err := tx.Create(event).Error; err != nil {
				return err
			}
			updates["finished_at"] = now
		}
		return tx.Model(&model.OrderOperation{}).Where("id = ?", operation.ID).Updates(updates).Error
	})
}
