package ratelimit

import (
	"context"
	"fmt"
	"os"
	rx "remarket/backend/pkg/redis"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDistributedBucket 验证两个独立客户端并发共享五个令牌、过期设置、补充及取消。
func TestDistributedBucket(t *testing.T) {
	if os.Getenv("REDIS_INTEGRATION") != "1" {
		t.Skip("设置 REDIS_INTEGRATION=1 运行真实 Redis 限流")
	}
	cfg, err := rx.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := rx.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := rx.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	key := fmt.Sprintf("ratelimit:test:%d", time.Now().UnixNano())
	defer a.Del(ctx, key)
	rule := Rule{5, 1, time.Hour}
	buckets := []*RedisBucket{New(a), New(b)}
	var allowed atomic.Int32
	var failed atomic.Int32
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			d, err := buckets[i%2].Allow(ctx, key, rule)
			if err != nil {
				failed.Add(1)
			} else if d.Allowed {
				allowed.Add(1)
			} else if d.RetryAfter <= 0 {
				failed.Add(1)
			}
		}(i)
	}
	close(gate)
	wg.Wait()
	if allowed.Load() != 5 || failed.Load() != 0 {
		t.Fatalf("超发或失败: allowed=%d failures=%d", allowed.Load(), failed.Load())
	}
	ttl, err := a.PTTL(ctx, key).Result()
	if err != nil || ttl <= 0 || ttl > 5*time.Hour {
		t.Fatal("桶 TTL 无效")
	}
	// 推进 Redis 桶基准一小时，无需 sleep 即可验证连续补充。
	if err := a.HIncrBy(ctx, key, "at", -int64(time.Hour/time.Millisecond)).Err(); err != nil {
		t.Fatal(err)
	}
	d, err := buckets[0].Allow(ctx, key, rule)
	if err != nil || !d.Allowed {
		t.Fatal("补充失败")
	}
	stopped, stop := context.WithCancel(ctx)
	stop()
	if _, err := buckets[0].Allow(stopped, key, rule); err == nil {
		t.Fatal("取消未传播")
	}
}

// TestRuleValidation 验证危险数字和时长无法进入 Lua。
func TestRuleValidation(t *testing.T) {
	for _, r := range []Rule{{0, 1, time.Second}, {1, 0, time.Second}, {1, 1, 0}, {1000001, 1, time.Second}, {1000000, 1, 24 * time.Hour}} {
		if r.Validate() == nil {
			t.Fatal("无效规则被接受")
		}
	}
}
