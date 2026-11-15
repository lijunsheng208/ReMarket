package node

import (
	"errors"
	"os"
	"strconv"
	"time"
)

// LoadRedisConfig 读取显式开关，命名空间按环境隔离，租约续期间隔不超过有效期三分之一。
func LoadRedisConfig() (bool, RedisConfig, error) {
	cfg := RedisConfig{Prefix: "chat:", LeaseTTL: 30 * time.Second, RenewInterval: 10 * time.Second}
	enabled := false
	if raw := os.Getenv("CHAT_REDIS_ENABLED"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return false, cfg, errors.New("聊天 Redis 开关无效")
		}
		enabled = value
	}
	if raw := os.Getenv("CHAT_REDIS_PREFIX"); raw != "" {
		cfg.Prefix = raw
	}
	for key, target := range map[string]*time.Duration{"CHAT_ROUTE_TTL": &cfg.LeaseTTL, "CHAT_ROUTE_RENEW_INTERVAL": &cfg.RenewInterval} {
		if raw := os.Getenv(key); raw != "" {
			value, err := time.ParseDuration(raw)
			if err != nil {
				return false, cfg, errors.New("聊天 Redis 租约时长无效")
			}
			*target = value
		}
	}
	if len(cfg.Prefix) > 100 || cfg.LeaseTTL < time.Second || cfg.LeaseTTL > 5*time.Minute || cfg.RenewInterval <= 0 || cfg.RenewInterval > cfg.LeaseTTL/3 {
		return false, cfg, errors.New("聊天 Redis 租约配置无效")
	}
	return enabled, cfg, nil
}
