package timeline

import (
	"context"
	"fmt"
	"google.golang.org/protobuf/types/known/timestamppb"
	"os"
	productv1 "remarket/backend/gen/product/v1"
	"testing"
	"time"
)

// TestRedisTimeline 验证真实 Redis 幂等、乱序、BIGINT 精度、推拉合并、窗口失效、丢失检测及长度限制。
func TestRedisTimeline(t *testing.T) {
	if os.Getenv("FEED_INTEGRATION") != "1" {
		t.Skip("需要 FEED_INTEGRATION=1")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Prefix = fmt.Sprintf("feed:test:%d:", time.Now().UnixNano())
	cfg.Limit = 100
	s, client, err := OpenLazy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	// 清理范围仅限本次唯一前缀，不清空 Redis 数据库。
	defer func() {
		keys, e := client.Keys(context.Background(), cfg.Prefix+"*").Result()
		if e != nil {
			t.Error(e)
		} else if len(keys) > 0 {
			if e := client.Del(context.Background(), keys...).Err(); e != nil {
				t.Error(e)
			}
		}
	}()
	now := time.Now().UTC().Truncate(time.Microsecond)
	p := &productv1.Product{Id: 9007199254740993, SellerId: 22, CreatedAt: timestamppb.New(now)}
	for i := 0; i < 2; i++ {
		if err := s.Apply(ctx, p, 2, true, []int64{11, 12}, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := client.ZCard(ctx, s.key("user", 11)).Val(); n != 1 {
		t.Fatal("重复关注消息产生重复记录")
	}
	if err := s.Apply(ctx, p, 9007199254740994, false, []int64{11, 12}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, p, 9007199254740993, true, []int64{11, 12}, false); err != nil {
		t.Fatal(err)
	}
	if n := client.ZCard(ctx, s.key("user", 11)).Val(); n != 0 {
		t.Fatal("乱序大整数版本复活商品")
	}
	p.Id = 1
	if err := s.Apply(ctx, p, 2, true, []int64{11, 12}, false); err != nil {
		t.Fatal(err)
	}
	hot := *p
	hot.Id = 2
	hot.SellerId = 23
	if err := s.Apply(ctx, &hot, 2, true, []int64{11, 12}, true); err != nil {
		t.Fatal(err)
	}
	if n := client.ZCard(ctx, s.key("user", 11)).Val(); n != 1 {
		t.Fatal("热门卖家错误写扩散")
	}
	if err := s.Seed(ctx, 11, "relations", []*productv1.Product{&hot, p}); err != nil {
		t.Fatal(err)
	}
	ids, _, err := s.Candidates(ctx, 11, []int64{22, 23}, "relations")
	if err != nil || len(ids) != 2 {
		t.Fatalf("合并去重失败 %v %v", ids, err)
	}
	if _, _, err := s.Candidates(ctx, 11, []int64{22, 23}, "refollow"); err == nil {
		t.Fatal("新关注指纹未使旧窗口失效")
	}
	if err := client.Del(ctx, s.key("user", 11)).Err(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Candidates(ctx, 11, []int64{22, 23}, "relations"); err == nil {
		t.Fatal("未检测收件箱丢失")
	}
	for id := int64(1); id <= 120; id++ {
		p.Id = id
		if err := s.Apply(ctx, p, 9007199254740995, true, nil, true); err != nil {
			t.Fatal(err)
		}
	}
	if n := client.ZCard(ctx, s.key("author", 22)).Val(); n != 100 {
		t.Fatalf("长度限制失败 %d", n)
	}
}
