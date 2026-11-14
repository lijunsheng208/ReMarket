// Package ratelimit 提供与 HTTP 无关的 Redis 原子令牌桶。
package ratelimit

import (
	"context"
	"errors"
	"github.com/redis/go-redis/v9"
	"math"
	"time"
)

// Rule 表示每 Interval 补充 Tokens 个令牌，Capacity 为可积累的突发容量。
type Rule struct {
	Capacity int64         `json:"capacity"`
	Tokens   int64         `json:"tokens"`
	Interval time.Duration `json:"-"`
}

// Validate 限制 Lua 数值和空闲过期时间，拒绝禁用补充或无限桶。
func (r Rule) Validate() error {
	if r.Capacity < 1 || r.Capacity > 1000000 || r.Tokens < 1 || r.Tokens > 1000000 || r.Interval < time.Millisecond || r.Interval > 24*time.Hour || float64(r.Capacity)*float64(r.Interval.Milliseconds())/float64(r.Tokens) > float64((7*24*time.Hour).Milliseconds()) {
		return errors.New("限流阈值无效")
	}
	return nil
}

// Decision 返回本次是否允许及至少等待多久能获得一个令牌。
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// RedisBucket 使用调用方管理的连接池，不关闭连接池。
type RedisBucket struct{ client redis.Scripter }

// New 创建分布式令牌桶，Redis 时间为所有实例的统一时间基准。
func New(client redis.Scripter) *RedisBucket { return &RedisBucket{client: client} }

var bucket = redis.NewScript(`
local capacity=tonumber(ARGV[1]); local rate=tonumber(ARGV[2])/tonumber(ARGV[3]);
local t=redis.call('TIME');local now=t[1]*1000+math.floor(t[2]/1000);
local values=redis.call('HMGET',KEYS[1],'tokens','at');
local tokens=tonumber(values[1]) or capacity;local at=tonumber(values[2]) or now;
-- 时钟回退时不重复补充；不能把未来的基准强行退回当前时间。
local logical=math.max(now,at);tokens=math.min(capacity,tokens+math.max(0,now-at)*rate);
local allowed=0;local retry=0;
if tokens>=1 then tokens=tokens-1;allowed=1 else retry=math.ceil((1-tokens)/rate+logical-now) end;
redis.call('HSET',KEYS[1],'tokens',tokens,'at',logical);
redis.call('PEXPIRE',KEYS[1],math.max(1,math.ceil(capacity/rate+logical-now)));
return {allowed,retry}
`)

// Allow 原子补充并扣除一个令牌；错误不等同于额度耗尽，降级由调用方决定。
func (r *RedisBucket) Allow(ctx context.Context, key string, rule Rule) (Decision, error) {
	if err := rule.Validate(); err != nil {
		return Decision{}, err
	}
	if key == "" {
		return Decision{}, errors.New("限流 Key 不能为空")
	}
	values, err := bucket.Run(ctx, r.client, []string{key}, rule.Capacity, rule.Tokens, rule.Interval.Milliseconds()).Int64Slice()
	if err != nil {
		return Decision{}, err
	}
	if len(values) != 2 || values[1] < 0 || values[1] > math.MaxInt64/int64(time.Millisecond) {
		return Decision{}, errors.New("限流响应无效")
	}
	return Decision{Allowed: values[0] == 1, RetryAfter: time.Duration(values[1]) * time.Millisecond}, nil
}
