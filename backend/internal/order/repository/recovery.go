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

// ClaimOperation 先无锁选候选，再按订单→操作加锁并重查资格；多实例不重复持有有效租约。
func (r *Repository) ClaimOperation(ctx context.Context) (*service.RecoveryJob, error) {
	var candidates []model.OrderOperation
	eligible := "((status IN ? AND next_retry_at <= UTC_TIMESTAMP(6)) OR (status = ? AND locked_until <= UTC_TIMESTAMP(6)))"
	if err := r.db.WithContext(ctx).Where("((operation_type = 'RESERVE' AND order_id IN (SELECT id FROM orders WHERE status = 'PENDING')) OR (operation_type = 'RELEASE' AND order_id IN (SELECT id FROM orders WHERE status IN ('CANCELLING','EXPIRING'))) OR (operation_type = 'CONFIRM' AND order_id IN (SELECT id FROM orders WHERE status = 'CONFIRMING')))").Where(eligible, []model.OperationStatus{model.OperationPending, model.OperationRetry}, model.OperationProcessing).Order("id").Limit(20).Find(&candidates).Error; err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		var job *service.RecoveryJob
		err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var order model.Order
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("id = ? AND status IN ?", candidate.OrderID, operationStates(candidate.OperationType)).Take(&order).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil
				}
				return err
			}
			var operation model.OrderOperation
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", candidate.ID).Where(eligible, []model.OperationStatus{model.OperationPending, model.OperationRetry}, model.OperationProcessing).Take(&operation).Error; err != nil {
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

// FinishOperation 只有当前未过期租约可以落地结果；订单、操作和状态事件在一个事务提交。
func (r *Repository) FinishOperation(ctx context.Context, job *service.RecoveryJob, result service.RecoveryResult) error {
	if result.Status == model.OperationFailed && job.Operation.OperationType != model.OperationReserve {
		return errors.New("终结操作不能推断失败")
	}
	if result.Status != model.OperationRetry && result.Status != model.OperationSucceeded && result.Status != model.OperationFailed {
		return errors.New("非法恢复结果")
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var order model.Order
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", job.Order.ID).Take(&order).Error; err != nil {
			return err
		}
		if !operationMatches(job.Operation.OperationType, order.Status) {
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
			switch job.Operation.OperationType {
			case model.OperationReserve:
				order.Status = model.StatusReserved
			case model.OperationConfirm:
				order.Status = model.StatusCompleted
				order.ClosedAt = &now
			case model.OperationRelease:
				if order.Status == model.StatusExpiring {
					order.Status = model.StatusExpired
				} else {
					order.Status = model.StatusCancelled
				}
				order.ClosedAt = &now
			}
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

// operationStates 返回操作可以落地的不可逆订单状态，防止反向执行或旧操作覆盖新决定。
func operationStates(kind model.OperationType) []model.OrderStatus {
	switch kind {
	case model.OperationReserve:
		return []model.OrderStatus{model.StatusPending}
	case model.OperationRelease:
		return []model.OrderStatus{model.StatusCancelling, model.StatusExpiring}
	case model.OperationConfirm:
		return []model.OrderStatus{model.StatusConfirming}
	}
	return nil
}

// operationMatches 检查操作类型与订单决定是否一致。
func operationMatches(kind model.OperationType, state model.OrderStatus) bool {
	for _, allowed := range operationStates(kind) {
		if allowed == state {
			return true
		}
	}
	return false
}
