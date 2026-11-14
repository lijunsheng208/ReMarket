// Package redis 提供单节点 Redis 连接配置，不包含业务缓存或自动启动重连任务。
package redis

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Config 定义单节点连接参数；密码不能写入日志。
type Config struct {
	Address, Username, Password                                         string
	DB, PoolSize, MinIdleConns                                          int
	DialTimeout, ReadTimeout, WriteTimeout, PoolTimeout, ConnectTimeout time.Duration
}

// LoadConfig 读取环境变量；缺失地址时兼容 Compose 的 REDIS_PORT，不自动加载 .env。
func LoadConfig() (Config, error) {
	c := Config{Address: os.Getenv("REDIS_ADDR"), Username: os.Getenv("REDIS_USERNAME"), Password: os.Getenv("REDIS_PASSWORD"), PoolSize: 10, DialTimeout: time.Second, ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond, PoolTimeout: 200 * time.Millisecond, ConnectTimeout: 2 * time.Second}
	if c.Address == "" {
		port := os.Getenv("REDIS_PORT")
		if port == "" {
			port = "6379"
		}
		c.Address = net.JoinHostPort("127.0.0.1", port)
	}
	for key, target := range map[string]*int{"REDIS_DB": &c.DB, "REDIS_POOL_SIZE": &c.PoolSize, "REDIS_MIN_IDLE_CONNS": &c.MinIdleConns} {
		if raw := os.Getenv(key); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				return Config{}, fmt.Errorf("%s 必须为整数", key)
			}
			*target = value
		}
	}
	for key, target := range map[string]*time.Duration{"REDIS_DIAL_TIMEOUT": &c.DialTimeout, "REDIS_READ_TIMEOUT": &c.ReadTimeout, "REDIS_WRITE_TIMEOUT": &c.WriteTimeout, "REDIS_POOL_TIMEOUT": &c.PoolTimeout, "REDIS_CONNECT_TIMEOUT": &c.ConnectTimeout} {
		if raw := os.Getenv(key); raw != "" {
			value, err := time.ParseDuration(raw)
			if err != nil {
				return Config{}, fmt.Errorf("%s 必须为时长", key)
			}
			*target = value
		}
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate 拒绝会禁用超时、放开连接上限或产生无效地址的配置，不回显输入值。
func (c Config) Validate() error {
	host, port, err := net.SplitHostPort(c.Address)
	if err != nil || strings.TrimSpace(host) == "" || strings.TrimSpace(host) != host {
		return fmt.Errorf("REDIS_ADDR 必须为 host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("Redis 端口必须在 1～65535 内")
	}
	if c.DB < 0 || c.PoolSize <= 0 || c.MinIdleConns < 0 || c.MinIdleConns > c.PoolSize {
		return fmt.Errorf("Redis DB 或连接池参数无效")
	}
	if c.DialTimeout <= 0 || c.ReadTimeout <= 0 || c.WriteTimeout <= 0 || c.PoolTimeout <= 0 || c.ConnectTimeout <= 0 {
		return fmt.Errorf("Redis 超时必须全部为正时长")
	}
	return nil
}

// Open 创建并探测连接；失败关闭连接池，成功由调用方 Close，不延长上游 Deadline。
func Open(ctx context.Context, c Config) (*goredis.Client, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client := goredis.NewClient(&goredis.Options{
		Addr: c.Address, Username: c.Username, Password: c.Password, DB: c.DB,
		PoolSize: c.PoolSize, MaxActiveConns: c.PoolSize, MinIdleConns: c.MinIdleConns,
		DialTimeout: c.DialTimeout, ReadTimeout: c.ReadTimeout, WriteTimeout: c.WriteTimeout, PoolTimeout: c.PoolTimeout,
		ContextTimeoutEnabled: true, MaxRetries: -1, DialerRetries: 1,
	})
	// 缓存故障应快速交给业务降级；禁用隐式命令重试，恢复策略由调用方决定。
	probe, cancel := context.WithTimeout(ctx, c.ConnectTimeout)
	defer cancel()
	if err := client.Ping(probe).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("Redis 连接探测失败: %w", err)
	}
	return client, nil
}
