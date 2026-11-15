package timeline

import (
	"fmt"
	goredis "github.com/redis/go-redis/v9"
	"os"
	rx "remarket/backend/pkg/redis"
	"strconv"
	"time"
)

type Config struct {
	Enabled          bool
	Threshold, Limit int64
	TTL              time.Duration
	Prefix           string
}

// LoadConfig 校验显式开关、热门阈值及时间线容量，错误不回显配置值。
func LoadConfig() (Config, error) {
	c := Config{Threshold: 1000, Limit: 1000, TTL: time.Hour, Prefix: "feed:"}
	var err error
	if raw := os.Getenv("FEED_REDIS_ENABLED"); raw != "" {
		c.Enabled, err = strconv.ParseBool(raw)
		if err != nil {
			return c, fmt.Errorf("Feed 开关无效")
		}
	}
	for key, target := range map[string]*int64{"FEED_HOT_SELLER_THRESHOLD": &c.Threshold, "FEED_TIMELINE_LIMIT": &c.Limit} {
		if raw := os.Getenv(key); raw != "" {
			*target, err = strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return c, fmt.Errorf("Feed 数值配置无效")
			}
		}
	}
	if c.Threshold < 1 || c.Threshold > 10000 || c.Limit < 100 || c.Limit > 10000 {
		return c, fmt.Errorf("Feed 阈值或容量无效")
	}
	return c, nil
}

// OpenLazy 创建有界 Redis 连接池但不强制启动探活，查询服务可在 Redis 故障期间启动并回退 MySQL。
func OpenLazy(c Config) (*Store, *goredis.Client, error) {
	cfg, err := rx.LoadConfig()
	if err != nil {
		return nil, nil, err
	}
	client := goredis.NewClient(&goredis.Options{Addr: cfg.Address, Username: cfg.Username, Password: cfg.Password, DB: cfg.DB, PoolSize: cfg.PoolSize, MaxActiveConns: cfg.PoolSize, MinIdleConns: cfg.MinIdleConns, DialTimeout: cfg.DialTimeout, ReadTimeout: cfg.ReadTimeout, WriteTimeout: cfg.WriteTimeout, PoolTimeout: cfg.PoolTimeout, ContextTimeoutEnabled: true, MaxRetries: -1, DialerRetries: 1})
	store, err := New(client, c.Prefix, c.Limit, c.TTL)
	if err != nil {
		client.Close()
		return nil, nil, err
	}
	return store, client, nil
}
