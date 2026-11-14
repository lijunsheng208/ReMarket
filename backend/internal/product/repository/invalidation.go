package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Invalidation 是 Product 自有失效任务，不作为跨服务事件。
type Invalidation struct {
	ID, ProductID, AggregateVersion, Attempts int64
	Status                                    string
	NextAttemptAt, CreatedAt                  time.Time
	LockedUntil, CompletedAt                  *time.Time
	LockToken, LastError                      *string
}

// TableName 指定失效任务表名。
func (Invalidation) TableName() string { return "product_cache_invalidations" }

// enqueueInvalidation 必须使用商品变更同一事务，失败由调用方回滚。
func enqueueInvalidation(tx *gorm.DB, id, version int64) error {
	return tx.Exec("INSERT INTO product_cache_invalidations (product_id, aggregate_version) VALUES (?, ?)", id, version).Error
}

// ClaimInvalidation 短事务领取一条任务；productID 为零时扫描全部，过期租约允许接管。
func (r *ProductRepository) ClaimInvalidation(ctx context.Context, productID int64, lease time.Duration) (*Invalidation, error) {
	if productID < 0 || lease < time.Millisecond {
		return nil, errors.New("失效任务领取参数无效")
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(b[:])
	var result Invalidation
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("(status = 'PENDING' AND next_attempt_at <= UTC_TIMESTAMP(6)) OR (status = 'PROCESSING' AND locked_until <= UTC_TIMESTAMP(6))")
		if productID > 0 {
			query = query.Where("product_id = ?", productID)
		}
		if err := query.Order("id ASC").Take(&result).Error; err != nil {
			return err
		}
		update := tx.Model(&Invalidation{}).Where("id = ?", result.ID).Updates(map[string]any{"status": "PROCESSING", "attempts": gorm.Expr("attempts + 1"), "lock_token": token, "locked_until": gorm.Expr("TIMESTAMPADD(MICROSECOND, ?, UTC_TIMESTAMP(6))", lease.Microseconds())})
		if update.Error != nil {
			return update.Error
		}
		result.LockToken = &token
		result.Attempts++
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// FinishInvalidation 仅当前未过期租约能推进状态，Redis 确认成功后才能完成任务。
func (r *ProductRepository) FinishInvalidation(ctx context.Context, task *Invalidation, success bool, backoff time.Duration) error {
	if task == nil || task.LockToken == nil || backoff < 0 {
		return errors.New("失效任务完成参数无效")
	}
	values := map[string]any{"lock_token": nil, "locked_until": nil}
	if success {
		values["status"] = "DONE"
		values["completed_at"] = gorm.Expr("UTC_TIMESTAMP(6)")
		values["last_error"] = nil
	} else {
		values["status"] = "PENDING"
		values["next_attempt_at"] = gorm.Expr("TIMESTAMPADD(MICROSECOND, ?, UTC_TIMESTAMP(6))", backoff.Microseconds())
		values["last_error"] = "redis_invalidation_failed"
	}
	update := r.db.WithContext(ctx).Model(&Invalidation{}).Where("id = ? AND status = 'PROCESSING' AND lock_token = ? AND locked_until > UTC_TIMESTAMP(6)", task.ID, *task.LockToken).Updates(values)
	if update.Error != nil {
		return update.Error
	}
	if update.RowsAffected != 1 {
		return errors.New("失效任务租约已丢失")
	}
	return nil
}

// PurgeInvalidations 分批清理保留期外完成任务，不删除待处理或租约任务。
func (r *ProductRepository) PurgeInvalidations(ctx context.Context, before time.Time, limit int) error {
	if limit <= 0 || limit > 1000 {
		return errors.New("失效任务清理批量无效")
	}
	return r.db.WithContext(ctx).Exec("DELETE FROM product_cache_invalidations WHERE status = 'DONE' AND completed_at < ? ORDER BY id LIMIT ?", before, limit).Error
}
