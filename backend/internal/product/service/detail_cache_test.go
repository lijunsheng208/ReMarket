package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pc "remarket/backend/internal/product/cache"
	"remarket/backend/internal/product/model"
	"remarket/backend/internal/product/repository"
	"remarket/backend/pkg/auth"
)

type fakeSnapshot struct {
	QueryRepository
	calls atomic.Int32
	load  func(context.Context) (*model.Product, []model.ProductImage, error)
}

// GetDetailSnapshot 统计实际回源次数并支持屏障控制。
func (r *fakeSnapshot) GetDetailSnapshot(ctx context.Context, _ int64) (*model.Product, []model.ProductImage, error) {
	r.calls.Add(1)
	return r.load(ctx)
}

type fakeCache struct {
	mu        sync.Mutex
	d         *pc.Detail
	hit, fail bool
	gets      atomic.Int32
	puts      atomic.Int32
}

// Get 模拟缓存命中或 Redis 故障。
func (c *fakeCache) Get(context.Context, int64) (*pc.Detail, bool, error) {
	c.gets.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return nil, false, errors.New("offline")
	}
	return c.d, c.hit, nil
}

// Begin 模拟读取代次。
func (c *fakeCache) Begin(context.Context, int64) (string, error) {
	if c.fail {
		return "", errors.New("offline")
	}
	return "epoch", nil
}

// Deadline 使用加载开始时刻计算截止时间。
func (c *fakeCache) Deadline(start time.Time, _ bool) (time.Time, error) {
	return start.Add(time.Minute), nil
}

// Put 记录原始快照，授权结果不能进入此接口。
func (c *fakeCache) Put(_ context.Context, _ int64, _ string, d *pc.Detail, _ time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts.Add(1)
	c.d = d
	c.hit = true
	return true, nil
}

// testConfig 为并发测试提供有限预算。
func testConfig() DetailLoadConfig {
	return DetailLoadConfig{MaxConcurrent: 1, MaxWaiting: 10, LoadTimeout: time.Second, WaitTimeout: 20 * time.Millisecond, CacheTimeout: 20 * time.Millisecond}
}

// testProduct 提供可缓存商品。
func testProduct() *model.Product {
	return &model.Product{ID: 1, SellerID: 2, CategoryID: 1, PriceCent: 100, AggregateVersion: 1, Status: model.StatusOnSale}
}

// newTestQuery 构造测试查询且确保服务生命周期受测试关闭控制。
func newTestQuery(t *testing.T, r *fakeSnapshot, c *fakeCache) *QueryService {
	t.Helper()
	life, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	s, err := NewCachedQuery(r, nil, c, life, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestCacheHitAndNegative 验证正负命中零回源及返回对象隔离。
func TestCacheHitAndNegative(t *testing.T) {
	r := &fakeSnapshot{}
	c := &fakeCache{d: toCache(testProduct(), nil), hit: true}
	s := newTestQuery(t, r, c)
	d, err := s.GetDetail(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	d.Product.Title = "changed"
	again, err := s.GetDetail(context.Background(), 1)
	if err != nil || again.Product.Title != "" || r.calls.Load() != 0 {
		t.Fatal("命中回源或共享对象污染")
	}
	c.d = nil
	if _, err := s.GetDetail(context.Background(), 1); !errors.Is(err, repository.ErrProductNotFound) {
		t.Fatal("负缓存语义错误")
	}
	if r.calls.Load() != 0 {
		t.Fatal("负缓存发生回源")
	}
}

// TestMissAndFailure 验证回填、负缓存与 Redis 故障回源；未知数据库错误不写负缓存。
func TestMissAndFailure(t *testing.T) {
	for _, mode := range []string{"success", "missing", "database-error", "redis-error"} {
		t.Run(mode, func(t *testing.T) {
			c := &fakeCache{fail: mode == "redis-error"}
			r := &fakeSnapshot{load: func(context.Context) (*model.Product, []model.ProductImage, error) {
				if mode == "missing" {
					return nil, nil, repository.ErrProductNotFound
				}
				if mode == "database-error" {
					return nil, nil, errors.New("db failure")
				}
				return testProduct(), nil, nil
			}}
			s := newTestQuery(t, r, c)
			_, err := s.GetDetail(context.Background(), 1)
			if mode == "success" || mode == "redis-error" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("错误被吞掉")
			}
			expected := int32(0)
			if mode == "success" || mode == "missing" {
				expected = 1
			}
			if c.puts.Load() != expected {
				t.Fatal("不当回填")
			}
		})
	}
}

// TestMergedLoadAndCancellation 用屏障验证独立等待者取消不会取消首个请求的回源。
func TestMergedLoadAndCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	r := &fakeSnapshot{load: func(ctx context.Context) (*model.Product, []model.ProductImage, error) {
		close(entered)
		select {
		case <-release:
			return testProduct(), nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}}
	c := &fakeCache{}
	s := newTestQuery(t, r, c)
	done := make(chan error, 1)
	go func() { _, err := s.GetDetail(context.Background(), 1); done <- err }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.GetDetail(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatal("等待者取消未传播")
	}
	// 后续请求无论加入共享加载还是在回填后到达，都不应新增回源。
	joined := make(chan error, 1)
	go func() { _, err := s.GetDetail(context.Background(), 1); joined <- err }()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
	if r.calls.Load() != 1 {
		t.Fatalf("回源次数=%d", r.calls.Load())
	}
}

// TestCapacity 验证 Redis 故障时不同商品仍受同一回源容量门限保护。
func TestCapacity(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	r := &fakeSnapshot{load: func(ctx context.Context) (*model.Product, []model.ProductImage, error) {
		close(entered)
		select {
		case <-release:
			return testProduct(), nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}}
	s := newTestQuery(t, r, &fakeCache{fail: true})
	done := make(chan error, 1)
	go func() { _, err := s.GetDetail(context.Background(), 1); done <- err }()
	<-entered
	if _, err := s.GetDetail(context.Background(), 2); err == nil {
		t.Fatal("未执行容量限制")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r.calls.Load() != 1 {
		t.Fatal("容量限制仍访问数据库")
	}
}

// TestVisibility 验证缓存中的私有状态仍按逐请求身份判断，不把隐藏结果写成负缓存。
func TestVisibility(t *testing.T) {
	p := testProduct()
	p.Status = model.StatusDraft
	c := &fakeCache{hit: true, d: toCache(p, nil)}
	s := newTestQuery(t, &fakeSnapshot{}, c)
	if _, err := s.GetDetail(context.Background(), 1); !errors.Is(err, repository.ErrProductNotFound) {
		t.Fatal("匿名越权")
	}
	manager, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("x", 32), Issuer: "test", Audience: "test", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := manager.IssueAccessToken(2, "USER")
	if err != nil {
		t.Fatal(err)
	}
	seller, err := auth.Authenticate(context.Background(), raw, manager)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDetail(seller, 1); err != nil {
		t.Fatal("卖家不能查看缓存草稿")
	}
	raw, _, err = manager.IssueAccessToken(3, "ADMIN")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(context.Background(), raw, manager)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &changingAdmin{}
	s.admins = verifier
	if _, err := s.GetDetail(admin, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDetail(admin, 1); !errors.Is(err, repository.ErrProductNotFound) {
		t.Fatal("管理员核实结果被缓存")
	}
	if c.puts.Load() != 0 {
		t.Fatal("权限结果污染了缓存")
	}

}

type changingAdmin struct{ calls int }

// IsCurrentAdmin 模拟管理员首次有效、随后已降权。
func (a *changingAdmin) IsCurrentAdmin(context.Context) (bool, error) {
	a.calls++
	return a.calls == 1, nil
}

// TestLifecycleAndWaitCapacity 验证服务关闭和总等待容量拒绝，不触发额外回源。
func TestLifecycleAndWaitCapacity(t *testing.T) {
	life, stop := context.WithCancel(context.Background())
	entered := make(chan struct{})
	r := &fakeSnapshot{load: func(ctx context.Context) (*model.Product, []model.ProductImage, error) {
		close(entered)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}}
	cfg := testConfig()
	cfg.MaxWaiting = 1
	s, err := NewCachedQuery(r, nil, &fakeCache{fail: true}, life, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	done := make(chan error, 1)
	go func() { _, err := s.GetDetail(context.Background(), 1); done <- err }()
	<-entered
	if _, err := s.GetDetail(context.Background(), 2); err == nil {
		t.Fatal("总等待容量未限制")
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("生命周期取消未传播: %v", err)
	}
	if _, err := s.GetDetail(context.Background(), 1); !errors.Is(err, context.Canceled) {
		t.Fatal("退出后仍接受读取")
	}
}
