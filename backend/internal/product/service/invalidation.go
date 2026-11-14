package service

import (
	"context"
	"time"

	"remarket/backend/internal/product/repository"
)

// InvalidationRepository 只操作 Product 自有任务表。
type InvalidationRepository interface {
	ClaimInvalidation(context.Context, int64, time.Duration) (*repository.Invalidation, error)
	FinishInvalidation(context.Context, *repository.Invalidation, bool, time.Duration) error
	PurgeInvalidations(context.Context, time.Time, int) error
}

// CacheInvalidator 表示已实现原子代次切换的缓存适配器。
type CacheInvalidator interface {
	Invalidate(context.Context, int64) error
}

// InvalidationRunner 协调数据库租约与 Redis，外部通过 Run 管理生命周期。
type InvalidationRunner struct {
	repo    InvalidationRepository
	cache   CacheInvalidator
	observe func(string)
}

// NewInvalidationRunner 注入持久化和原子失效器；observe 必须并发安全、不阻塞。
func NewInvalidationRunner(repo InvalidationRepository, cache CacheInvalidator, observe func(string)) *InvalidationRunner {
	return &InvalidationRunner{repo: repo, cache: cache, observe: observe}
}

// note 仅报告固定错误类别，不泄漏 Redis 地址、密码或 SQL。
func (r *InvalidationRunner) note(event string) {
	if r.observe != nil {
		r.observe(event)
	}
}

// Process 在短预算内领取并执行一条任务，失败保留恢复能力，不回滚已提交业务。
func (r *InvalidationRunner) Process(ctx context.Context, id int64) bool {
	work, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	task, err := r.repo.ClaimInvalidation(work, id, 5*time.Second)
	if err != nil {
		r.note("claim_failed")
		return false
	}
	if task == nil {
		return false
	}
	err = r.cache.Invalidate(work, task.ProductID)
	backoff := time.Second
	for n := int64(1); n < task.Attempts && backoff < time.Minute; n++ {
		backoff *= 2
	}
	if backoff > time.Minute {
		backoff = time.Minute
	}
	if err != nil {
		r.note("invalidate_failed")
	}
	if task.Attempts >= 10 {
		r.note("retry_threshold_exceeded")
	}
	if finishErr := r.repo.FinishInvalidation(work, task, err == nil, backoff); finishErr != nil {
		r.note("finish_failed")
	}
	return true
}

// AfterCommit 尽力失效指定商品；同步尝试预算有限，剩余任务由 Run 恢复。
func (r *InvalidationRunner) AfterCommit(ctx context.Context, id int64) { r.Process(ctx, id) }

// Run 启动即扫描，定时按有界批量恢复；取消后停止，调用方等待退出再关闭连接池。
func (r *InvalidationRunner) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	cleanup := time.NewTicker(time.Hour)
	defer cleanup.Stop()
	for {
		for i := 0; i < 20 && ctx.Err() == nil; i++ {
			if !r.Process(ctx, 0) {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-cleanup.C:
			bounded, cancel := context.WithTimeout(ctx, time.Second)
			if err := r.repo.PurgeInvalidations(bounded, time.Now().UTC().Add(-7*24*time.Hour), 1000); err != nil {
				r.note("cleanup_failed")
			}
			cancel()
		}
	}
}

// PostCommit 定义业务提交后尽力失效入口，不把 Redis 失败转换为业务失败。
type PostCommit interface{ AfterCommit(context.Context, int64) }

// notifyCommit 仅在事务成功返回后调用，不延长已取消的上游生命周期。
func notifyCommit(ctx context.Context, hook PostCommit, id int64, err error) {
	if err == nil && hook != nil && ctx.Err() == nil {
		hook.AfterCommit(ctx, id)
	}
}
