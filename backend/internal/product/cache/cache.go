// Package cache 实现商品详情 Redis 协议，不判断调用者权限或访问业务数据库。
package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/redis/go-redis/v9"
)

// Detail 是独立缓存格式，大整数在 JSON 中编码为字符串。
type Detail struct {
	ProductID        int64      `json:"product_id,string"`
	Version          int64      `json:"aggregate_version,string"`
	SellerID         int64      `json:"seller_id,string"`
	CategoryID       int64      `json:"category_id,string"`
	PriceCent        int64      `json:"price_cent,string"`
	Title            string     `json:"title"`
	Description      string     `json:"description"`
	Status           string     `json:"status"`
	FirstPublishedAt *time.Time `json:"first_published_at"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	Images           []Image    `json:"images"`
}

// Image 仅保存对象键及稳定排序依据，不缓存签名 URL。
type Image struct {
	ID        int64     `json:"id,string"`
	ObjectKey string    `json:"object_key"`
	SortOrder int32     `json:"sort_order"`
	CreatedAt time.Time `json:"created_at"`
}

type entry struct {
	Schema    int       `json:"schema_version"`
	Kind      string    `json:"kind"`
	ProductID int64     `json:"product_id,string"`
	ExpiresAt time.Time `json:"expires_at"`
	Detail    *Detail   `json:"detail,omitempty"`
}

// Config 控制快照寿命；LoadTimeout 用于验证 epoch 能覆盖最长加载过程。
type Config struct{ TTL, Jitter, NegativeTTL, EpochTTL, LoadTimeout time.Duration }

// DefaultConfig 返回开发环境初始值，不代表生产调优结果。
func DefaultConfig() Config {
	return Config{300 * time.Second, 60 * time.Second, 30 * time.Second, 10 * time.Minute, 3 * time.Second}
}

// Store 使用外部管理的连接池，不拥有连接池生命周期。
type Store struct {
	client redis.Scripter
	reader interface {
		Get(context.Context, string) *redis.StringCmd
	}
	cfg Config
}

// New 校验寿命约束；不建立连接、不启用业务缓存。
func New(client *redis.Client, cfg Config) (*Store, error) {
	if client == nil {
		return nil, errors.New("Redis 客户端不能为空")
	}
	maxTTL := cfg.TTL + cfg.Jitter
	if cfg.TTL < time.Millisecond || cfg.Jitter < 0 || maxTTL < cfg.TTL || cfg.NegativeTTL < time.Millisecond || cfg.LoadTimeout <= 0 || cfg.EpochTTL <= maxTTL || cfg.EpochTTL-maxTTL <= cfg.LoadTimeout || cfg.EpochTTL <= cfg.NegativeTTL || cfg.EpochTTL-cfg.NegativeTTL <= cfg.LoadTimeout {
		return nil, errors.New("缓存寿命或代次 TTL 无效")
	}
	return &Store{client: client, reader: client, cfg: cfg}, nil
}

// keys 使同一商品的脚本 Key 处于相同 Redis Cluster 槽位。
func keys(id int64) (string, string, error) {
	if id <= 0 {
		return "", "", errors.New("商品 ID 必须为正数")
	}
	return fmt.Sprintf("product:detail:v1:{%d}", id), fmt.Sprintf("product:epoch:v1:{%d}", id), nil
}

// token 使用密码学随机数避免代次被淘汰重建后复用旧令牌。
func token() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

var acquire = redis.NewScript(`local v=redis.call('GET',KEYS[1]); if v then return v end; redis.call('SET',KEYS[1],ARGV[1],'PX',ARGV[2]); return ARGV[1]`)
var invalidate = redis.NewScript(`redis.call('SET',KEYS[2],ARGV[1],'PX',ARGV[2]); redis.call('DEL',KEYS[1]); return 1`)
var fill = redis.NewScript(`if redis.call('GET',KEYS[2]) ~= ARGV[1] then return 0 end; local t=redis.call('TIME'); local now=t[1]*1000+math.floor(t[2]/1000); if tonumber(ARGV[3]) <= now then return 0 end; local r=redis.call('SET',KEYS[1],ARGV[2],'PXAT',ARGV[3],'NX'); if r then return 1 end; return 0`)
var remove = redis.NewScript(`if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('DEL',KEYS[1]) end; return 0`)

// Begin 必须在数据库读取之前调用，失败时调用方应选择不回填。
func (s *Store) Begin(ctx context.Context, id int64) (string, error) {
	_, epoch, err := keys(id)
	if err != nil {
		return "", err
	}
	t, err := token()
	if err != nil {
		return "", err
	}
	return acquire.Run(ctx, s.client, []string{epoch}, t, s.cfg.EpochTTL.Milliseconds()).Text()
}

// Invalidate 原子更换代次并删除正负缓存，可安全重复或乱序执行。
func (s *Store) Invalidate(ctx context.Context, id int64) error {
	detail, epoch, err := keys(id)
	if err != nil {
		return err
	}
	t, err := token()
	if err != nil {
		return err
	}
	return invalidate.Run(ctx, s.client, []string{detail, epoch}, t, s.cfg.EpochTTL.Milliseconds()).Err()
}

// Deadline 在回源开始时生成截止时间，后续回填不能重新计算以延长寿命。
func (s *Store) Deadline(start time.Time, negative bool) (time.Time, error) {
	if negative {
		return start.Add(s.cfg.NegativeTTL).UTC(), nil
	}
	jitter := time.Duration(0)
	if s.cfg.Jitter > 0 {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(s.cfg.Jitter)))
		if err != nil {
			return time.Time{}, err
		}
		jitter = time.Duration(n.Int64())
	}
	return start.Add(s.cfg.TTL + jitter).UTC(), nil
}

// valid 验证业务必要字段和图片顺序，阻止损坏数据成为可见性判断依据。
func valid(d *Detail, id int64) bool {
	if d == nil || d.ProductID != id || d.Version <= 0 || d.SellerID <= 0 || d.CategoryID <= 0 || d.PriceCent <= 0 || d.Images == nil {
		return false
	}
	switch d.Status {
	case "DRAFT", "ON_SALE", "RESERVED", "SOLD", "OFF_SHELF":
	default:
		return false
	}
	for i, img := range d.Images {
		if img.ID <= 0 || img.ObjectKey == "" || img.SortOrder < 0 {
			return false
		}
		if i > 0 {
			p := d.Images[i-1]
			if p.SortOrder > img.SortOrder || (p.SortOrder == img.SortOrder && p.ID >= img.ID) {
				return false
			}
		}
	}
	return true
}

// Put 按代次和 NX 条件回填；nil 详情仅供主库明确不存在时使用，false 表示未写入。
func (s *Store) Put(ctx context.Context, id int64, epoch string, d *Detail, deadline time.Time) (bool, error) {
	key, ek, err := keys(id)
	if err != nil {
		return false, err
	}
	if len(epoch) != 32 {
		return false, errors.New("代次令牌无效")
	}
	if _, err := hex.DecodeString(epoch); err != nil {
		return false, errors.New("代次令牌无效")
	}
	e := entry{Schema: 1, Kind: "notfound", ProductID: id, ExpiresAt: deadline.UTC()}
	maxTTL := s.cfg.NegativeTTL
	if d != nil {
		if !valid(d, id) {
			return false, errors.New("商品缓存详情无效")
		}
		e.Kind = "detail"
		e.Detail = d
		maxTTL = s.cfg.TTL + s.cfg.Jitter
	}
	if deadline.After(time.Now().Add(maxTTL)) {
		return false, errors.New("缓存截止时间超出寿命上限")
	}
	if !deadline.After(time.Now()) {
		return false, nil
	}
	data, err := json.Marshal(e)
	if err != nil {
		return false, err
	}
	n, err := fill.Run(ctx, s.client, []string{key, ek}, epoch, string(data), deadline.UnixMilli()).Int()
	return n == 1, err
}

// Get 返回独立解码的详情及命中标记；命中且详情为 nil 表示物理不存在。
func (s *Store) Get(ctx context.Context, id int64) (*Detail, bool, error) {
	key, _, err := keys(id)
	if err != nil {
		return nil, false, err
	}
	raw, err := s.reader.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var e entry
	good := json.Unmarshal([]byte(raw), &e) == nil && e.Schema == 1 && e.ProductID == id && e.ExpiresAt.After(time.Now())
	good = good && ((e.Kind == "notfound" && e.Detail == nil) || (e.Kind == "detail" && valid(e.Detail, id)))
	if !good {
		// 比较原值后清理，避免误删并发替换的新快照；失败不妨碍回源。
		_ = remove.Run(ctx, s.client, []string{key}, raw).Err()
		return nil, false, nil
	}
	return e.Detail, true, nil
}
