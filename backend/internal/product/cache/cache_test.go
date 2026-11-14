package cache

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	redisx "remarket/backend/pkg/redis"
)

// sample 提供超过 JavaScript 安全整数范围的商品快照。
func sample(id int64) *Detail {
	return &Detail{ProductID: id, Version: 1, SellerID: 9007199254740993, CategoryID: 1, PriceCent: 123, Status: "ON_SALE", Images: []Image{}}
}

// TestFormatAndTTL 验证精度、空数组、配置约束与抖动范围。
func TestFormatAndTTL(t *testing.T) {
	d := sample(9007199254740995)
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"seller_id":"9007199254740993"`) || !strings.Contains(string(raw), `"images":[]`) {
		t.Fatalf("格式错误: %s", raw)
	}
	var copy Detail
	if err := json.Unmarshal(raw, &copy); err != nil || copy.ProductID != d.ProductID {
		t.Fatalf("精度丢失: %v", err)
	}
	cfg := DefaultConfig()
	s := &Store{cfg: cfg}
	start := time.Now()
	for i := 0; i < 100; i++ {
		end, err := s.Deadline(start, false)
		if err != nil || end.Before(start.Add(cfg.TTL)) || !end.Before(start.Add(cfg.TTL+cfg.Jitter)) {
			t.Fatal("抖动越界")
		}
	}
	end, _ := s.Deadline(start, true)
	if !end.Equal(start.Add(cfg.NegativeTTL)) {
		t.Fatal("负缓存寿命错误")
	}
	if valid(&Detail{}, 1) {
		t.Fatal("损坏详情被接受")
	}
}

// TestRedisProtocol 使用专用随机商品 Key 验证真实 Lua；不清空数据库或操作业务商品。
func TestRedisProtocol(t *testing.T) {
	if os.Getenv("REDIS_INTEGRATION") != "1" {
		t.Skip("设置 REDIS_INTEGRATION=1 验证真实 Redis Lua")
	}
	cfg, err := redisx.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := redisx.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// 测试使用高位随机 ID，退出仅删除本次两个 Key，保留其他数据。
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000_000_000_000_000))
	if err != nil {
		t.Fatal(err)
	}
	id := int64(8_000_000_000_000_000_000) + n.Int64()
	key, ek, _ := keys(id)
	exists, err := client.Exists(ctx, key, ek).Result()
	if err != nil || exists != 0 {
		t.Fatalf("测试 Key 已存在或不可用: %v", err)
	}
	// 测试清理使用独立短预算，避免主测试到期后遗留测试 Key。
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		if err := client.Del(cleanup, key, ek).Err(); err != nil {
			t.Errorf("清理测试 Key 失败: %v", err)
		}
	}()
	s, err := New(client, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	bad := DefaultConfig()
	bad.EpochTTL = bad.TTL
	if _, err := New(client, bad); err == nil {
		t.Fatal("无效代次寿命未拒绝")
	}
	old, err := s.Begin(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := s.Begin(ctx, id)
	if again != old {
		t.Fatal("读取代次不稳定")
	}
	deadline, _ := s.Deadline(time.Now(), false)
	if err := s.Invalidate(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Put(ctx, id, old, sample(id), deadline); err != nil || ok {
		t.Fatalf("旧读回填未拒绝: %v", err)
	}
	current, _ := s.Begin(ctx, id)
	if ok, err := s.Put(ctx, id, current, sample(id), deadline); err != nil || !ok {
		t.Fatalf("回填失败: %v", err)
	}
	ttl, _ := client.PTTL(ctx, key).Result()
	if ttl <= 0 || ttl > 360*time.Second {
		t.Fatal("TTL 越界")
	}
	updated := sample(id)
	updated.Title = "不得覆盖"
	if ok, err := s.Put(ctx, id, current, updated, deadline); err != nil || ok {
		t.Fatal("NX 未生效")
	}
	d, hit, err := s.Get(ctx, id)
	if err != nil || !hit || d.Title != "" || d.SellerID != 9007199254740993 {
		t.Fatalf("详情命中错误: %v", err)
	}
	d.Title = "修改调用者副本"
	d, _, _ = s.Get(ctx, id)
	if d.Title != "" {
		t.Fatal("共享对象被污染")
	}
	// 模拟 epoch 淘汰和重建：旧令牌不能通过缺失状态或 ABA。
	if err := client.Del(ctx, ek, key).Err(); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Put(ctx, id, current, sample(id), deadline); err != nil || ok {
		t.Fatal("缺失代次被放行")
	}
	fresh, _ := s.Begin(ctx, id)
	if fresh == current {
		t.Fatal("重建复用了旧代次")
	}
	if ok, err := s.Put(ctx, id, current, sample(id), deadline); err != nil || ok {
		t.Fatal("ABA 防护失败")
	}
	neg, _ := s.Deadline(time.Now(), true)
	if ok, err := s.Put(ctx, id, fresh, nil, neg); err != nil || !ok {
		t.Fatalf("空值写入失败: %v", err)
	}
	if d, hit, err := s.Get(ctx, id); err != nil || !hit || d != nil {
		t.Fatal("空值命中错误")
	}
	ttl, _ = client.PTTL(ctx, key).Result()
	if ttl <= 0 || ttl > 30*time.Second {
		t.Fatal("空值 TTL 越界")
	}
	if err := s.Invalidate(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := s.Get(ctx, id); err != nil || hit {
		t.Fatal("失效未删除")
	}
	fresh, _ = s.Begin(ctx, id)
	if ok, err := s.Put(ctx, id, fresh, nil, time.Now().Add(-time.Second)); err != nil || ok {
		t.Fatal("过期回填被接受")
	}
	if err := client.Set(ctx, key, "broken", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := s.Get(ctx, id); err != nil || hit {
		t.Fatal("损坏值未视为 miss")
	}
	if n, _ := client.Exists(ctx, key).Result(); n != 0 {
		t.Fatal("损坏值未清理")
	}
	// 确认条件删除不能删除其他请求已替换的新值。
	if err := client.Set(ctx, key, "replacement", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := remove.Run(ctx, client, []string{key}, "broken").Err(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := client.Get(ctx, key).Result(); raw != "replacement" {
		t.Fatal("误删新值")
	}
	// 两个独立 Store 并发回填相同代次，屏障保证竞争且只允许一个成功。
	if err := s.Invalidate(ctx, id); err != nil {
		t.Fatal(err)
	}
	other, err := New(client, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	fresh, err = s.Begin(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	deadline, _ = s.Deadline(time.Now(), false)
	gate := make(chan struct{})
	results := make(chan bool, 2)
	failures := make(chan error, 2)
	for _, store := range []*Store{s, other} {
		// 并发协程只上报结果，断言在测试主协程执行。
		go func(st *Store) {
			<-gate
			ok, err := st.Put(ctx, id, fresh, sample(id), deadline)
			results <- ok
			failures <- err
		}(store)
	}
	close(gate)
	wins := 0
	for i := 0; i < 2; i++ {
		if <-results {
			wins++
		}
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("并发回填成功次数=%d", wins)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := s.Begin(canceled, id); err == nil {
		t.Fatal("取消未传播")
	}
}
