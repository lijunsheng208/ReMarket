// Package timeline 管理可丢弃的 Feed Redis 索引，商品状态最终以 Product MySQL 为准。
package timeline

import (
	"context"
	"encoding/json"
	"fmt"
	goredis "github.com/redis/go-redis/v9"
	productv1 "remarket/backend/gen/product/v1"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Store struct {
	client *goredis.Client
	prefix string
	Limit  int64
	TTL    time.Duration
}
type Window struct {
	Fingerprint string `json:"fingerprint"`
	Floor       string `json:"floor"`
}

// New 创建隔离命名空间的单节点 Redis 时间线，限制每条时间线长度与存活时间。
func New(client *goredis.Client, prefix string, limit int64, ttl time.Duration) (*Store, error) {
	if client == nil || prefix == "" || limit < 100 || limit > 10000 || ttl < time.Minute || ttl > 24*time.Hour {
		return nil, fmt.Errorf("Feed Redis 配置无效")
	}
	return &Store{client: client, prefix: prefix, Limit: limit, TTL: ttl}, nil
}

// key 只组合内部确定的键类型和十进制 ID。
func (s *Store) key(kind string, id int64) string { return fmt.Sprintf("%s%s:%d", s.prefix, kind, id) }

// Member 将时间和 BIGINT 编成等长字典序，避免 ZSet 浮点分数损失 ID 或微秒精度。
func Member(p *productv1.Product) string {
	return fmt.Sprintf("%020d|%020d", p.GetCreatedAt().AsTime().UnixMicro(), p.Id)
}

// Boundary 返回可安全服务缓存窗口的最后一个完整排序键。
func Boundary(products []*productv1.Product) string {
	if len(products) == 0 {
		return ""
	}
	return Member(products[len(products)-1])
}

const applyScript = `
local old=redis.call('GET',KEYS[1])
local v=ARGV[1]
if old and (#old>#v or (#old==#v and old>v)) then return 0 end
redis.call('SET',KEYS[1],v,'EX',604800)
redis.call('SET',KEYS[3],ARGV[6],'EX',ARGV[5])
if ARGV[2]=='1' then redis.call('ZADD',KEYS[2],0,ARGV[3]) else redis.call('ZREM',KEYS[2],ARGV[3]) end
redis.call('ZREMRANGEBYRANK',KEYS[2],0,-tonumber(ARGV[4])-1)
redis.call('EXPIRE',KEYS[2],ARGV[5])
return 1`

// Apply 逐批流水线更新作者或收件箱；版本比较使用十进制字符串，重复版本允许安全重试未完成批次。
func (s *Store) Apply(ctx context.Context, product *productv1.Product, version int64, visible bool, followers []int64, hot bool) error {
	if product.Id <= 0 || product.SellerId <= 0 || version <= 0 || product.CreatedAt == nil {
		return fmt.Errorf("Feed 索引输入无效")
	}
	destinations := []string{s.key("author", product.SellerId)}
	if !hot {
		for _, id := range followers {
			destinations = append(destinations, s.key("user", id))
		}
	}
	for offset := 0; offset < len(destinations); offset += 200 {
		end := offset + 200
		if end > len(destinations) {
			end = len(destinations)
		}
		_, err := s.client.Pipelined(ctx, func(pipe goredis.Pipeliner) error {
			for _, key := range destinations[offset:end] {
				on := "0"
				if visible {
					on = "1"
				}
				pipe.Eval(ctx, applyScript, []string{s.key("version", product.Id), key, s.key("mode", product.SellerId)}, strconv.FormatInt(version, 10), on, Member(product), s.Limit, int64(s.TTL/time.Second), strconv.FormatBool(hot))
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Candidates 合并普通卖家收件箱和热门作者流；缺失窗口或作者状态时要求 MySQL 重建。
func (s *Store) Candidates(ctx context.Context, user int64, sellers []int64, fingerprint string) ([]int64, string, error) {
	raw, err := s.client.Get(ctx, s.key("window", user)).Result()
	if err != nil {
		return nil, "", err
	}
	var w Window
	if json.Unmarshal([]byte(raw), &w) != nil || w.Fingerprint != fingerprint || w.Floor == "" {
		return nil, "", fmt.Errorf("Feed 窗口未就绪")
	}
	keys := []string{s.key("user", user)}
	pipe := s.client.Pipeline()
	modes := make([]*goredis.StringCmd, len(sellers))
	for i, id := range sellers {
		modes[i] = pipe.Get(ctx, s.key("mode", id))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, "", err
	}
	for i, m := range modes {
		if m.Val() == "true" {
			keys = append(keys, s.key("author", sellers[i]))
		}
	}
	// 限制单次热门源合并规模，超出后走权威查询，避免极端关注集合放大 Redis 返回量。
	if len(keys) > 33 {
		return nil, "", fmt.Errorf("Feed 热门源过多，需回退")
	}
	pipe = s.client.Pipeline()
	ranges := make([]*goredis.StringSliceCmd, len(keys))
	for i, key := range keys {
		ranges[i] = pipe.ZRevRange(ctx, key, 0, s.Limit-1)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, "", err
	}
	// 收件箱被裁剪后提升安全下界，避免以完整旧窗口掩盖已经丢弃的记录。
	if len(ranges[0].Val()) == 0 {
		return nil, "", fmt.Errorf("Feed 收件箱缺失")
	}
	floor := w.Floor
	inbox := ranges[0].Val()
	if int64(len(inbox)) >= s.Limit && inbox[len(inbox)-1] > floor {
		floor = inbox[len(inbox)-1]
	}
	unique := map[string]bool{}
	for i, r := range ranges {
		if i > 0 && len(r.Val()) == 0 {
			return nil, "", fmt.Errorf("Feed 作者流缺失")
		}
		for _, member := range r.Val() {
			if member >= floor {
				unique[member] = true
			}
		}
	}
	members := make([]string, 0, len(unique))
	for member := range unique {
		members = append(members, member)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(members)))
	if len(members) > 2000 {
		members = members[:2000]
		if members[len(members)-1] > floor {
			floor = members[len(members)-1]
		}
	}
	ids := make([]int64, 0, len(members))
	for _, m := range members {
		parts := strings.Split(m, "|")
		if len(parts) != 2 {
			return nil, "", fmt.Errorf("Feed 成员无效")
		}
		id, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil || id <= 0 {
			return nil, "", fmt.Errorf("Feed 成员无效")
		}
		ids = append(ids, id)
	}
	return ids, floor, nil
}

// Seed 仅在首页回退成功后合并权威窗口；不覆盖消费者并发写入，标记短期完整下界。
func (s *Store) Seed(ctx context.Context, user int64, fingerprint string, products []*productv1.Product) error {
	if len(products) == 0 {
		return nil
	}
	members := make([]goredis.Z, 0, len(products))
	for _, p := range products {
		members = append(members, goredis.Z{Score: 0, Member: Member(p)})
	}
	raw, _ := json.Marshal(Window{Fingerprint: fingerprint, Floor: Boundary(products)})
	_, err := s.client.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		pipe.ZAdd(ctx, s.key("user", user), members...)
		pipe.ZRemRangeByRank(ctx, s.key("user", user), 0, -s.Limit-1)
		pipe.Expire(ctx, s.key("user", user), s.TTL)
		pipe.Set(ctx, s.key("window", user), raw, 30*time.Second)
		return nil
	})
	return err
}

// EnsureModes 为没有新事件的卖家补普通模式；NX 不覆盖消费者已判定的热门模式。
func (s *Store) EnsureModes(ctx context.Context, sellers []int64) error {
	_, err := s.client.Pipelined(ctx, func(pipe goredis.Pipeliner) error {
		for _, id := range sellers {
			pipe.SetNX(ctx, s.key("mode", id), "false", s.TTL)
		}
		return nil
	})
	return err
}
