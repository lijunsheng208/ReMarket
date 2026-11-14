package service

import (
	"context"
	"errors"
	"remarket/backend/internal/product/repository"
	"testing"
	"time"
)

type taskRepo struct {
	sequence *[]string
	task     *repository.Invalidation
	success  bool
	backoff  time.Duration
}

// ClaimInvalidation 模拟持久化领取，不依赖 Redis。
func (r *taskRepo) ClaimInvalidation(context.Context, int64, time.Duration) (*repository.Invalidation, error) {
	*r.sequence = append(*r.sequence, "claim")
	return r.task, nil
}

// FinishInvalidation 记录状态变更顺序和退避预算。
func (r *taskRepo) FinishInvalidation(_ context.Context, _ *repository.Invalidation, success bool, backoff time.Duration) error {
	*r.sequence = append(*r.sequence, "finish")
	r.success = success
	r.backoff = backoff
	return nil
}

// PurgeInvalidations 模拟有界清理。
func (r *taskRepo) PurgeInvalidations(context.Context, time.Time, int) error { return nil }

type invalidator struct {
	sequence *[]string
	err      error
}

// Invalidate 模拟 Redis ACK 或失败。
func (c *invalidator) Invalidate(context.Context, int64) error {
	*c.sequence = append(*c.sequence, "redis")
	return c.err
}

// TestInvalidationACK 验证必须先获得 Redis 结果再推进任务，失败保留重试且退避封顶。
func TestInvalidationACK(t *testing.T) {
	for _, failure := range []bool{false, true} {
		seq := []string{}
		repo := &taskRepo{sequence: &seq, task: &repository.Invalidation{ProductID: 1, Attempts: 100}}
		cache := &invalidator{sequence: &seq}
		if failure {
			cache.err = errors.New("unavailable")
		}
		runner := NewInvalidationRunner(repo, cache, nil)
		if !runner.Process(context.Background(), 1) {
			t.Fatal("未处理任务")
		}
		if len(seq) != 3 || seq[0] != "claim" || seq[1] != "redis" || seq[2] != "finish" {
			t.Fatal("ACK 顺序错误")
		}
		if repo.success == failure || repo.backoff != time.Minute {
			t.Fatal("重试状态错误")
		}
	}
}

type commitHook struct{ calls int }

// AfterCommit 统计提交后的同步失效调用。
func (h *commitHook) AfterCommit(context.Context, int64) { h.calls++ }

// TestNotifyCommit 验证业务失败或请求取消不延长上游进行失效。
func TestNotifyCommit(t *testing.T) {
	h := &commitHook{}
	notifyCommit(context.Background(), h, 1, errors.New("rollback"))
	ctx, stop := context.WithCancel(context.Background())
	stop()
	notifyCommit(ctx, h, 1, nil)
	if h.calls != 0 {
		t.Fatal("失败或取消后仍执行")
	}
	notifyCommit(context.Background(), h, 1, nil)
	if h.calls != 1 {
		t.Fatal("成功后未执行")
	}
}
