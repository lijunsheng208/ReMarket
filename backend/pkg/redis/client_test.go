package redis

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// cleanEnv 隔离开发机 Redis 环境变量，避免测试依赖本机密码和端口。
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"REDIS_ADDR", "REDIS_PORT", "REDIS_USERNAME", "REDIS_PASSWORD", "REDIS_DB", "REDIS_POOL_SIZE", "REDIS_MIN_IDLE_CONNS", "REDIS_DIAL_TIMEOUT", "REDIS_READ_TIMEOUT", "REDIS_WRITE_TIMEOUT", "REDIS_POOL_TIMEOUT", "REDIS_CONNECT_TIMEOUT"} {
		t.Setenv(key, "")
	}
}

// TestLoadConfig 验证自定义端口兼容及危险配置被拒绝。
func TestLoadConfig(t *testing.T) {
	cleanEnv(t)
	t.Setenv("REDIS_PORT", "16379")
	c, err := LoadConfig()
	if err != nil || c.Address != "127.0.0.1:16379" {
		t.Fatalf("端口回退失败: %v", err)
	}
	for _, tc := range []struct{ key, value string }{
		{"REDIS_ADDR", "redis://secret@localhost"}, {"REDIS_PORT", "70000"}, {"REDIS_DB", "-1"},
		{"REDIS_POOL_SIZE", "0"}, {"REDIS_POOL_SIZE", "bad"}, {"REDIS_MIN_IDLE_CONNS", "11"},
		{"REDIS_READ_TIMEOUT", "-1s"}, {"REDIS_WRITE_TIMEOUT", "0s"}, {"REDIS_DIAL_TIMEOUT", "bad"},
		{"REDIS_POOL_TIMEOUT", "0s"}, {"REDIS_CONNECT_TIMEOUT", "0s"},
	} {
		// 每个子测试独立恢复环境，防止一个错误掩盖另一个配置错误。
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("应拒绝无效配置")
			}
		})
	}
	t.Setenv("REDIS_ADDR", "[::1]:6379")
	if _, err := LoadConfig(); err != nil {
		t.Fatal(err)
	}
}

// TestOpenCanceled 验证启动探测在已取消或到期时不创建客户端。
func TestOpenCanceled(t *testing.T) {
	cleanEnv(t)
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if client, err := Open(ctx, c); client != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("取消未传播: %v", err)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if client, err := Open(ctx, c); client != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时未传播: %v", err)
	}
}

// TestRedisIntegration 显式启用时验证真实 Redis 连接、上下文及关闭，不创建业务 Key。
func TestRedisIntegration(t *testing.T) {
	if os.Getenv("REDIS_INTEGRATION") != "1" {
		t.Skip("设置 REDIS_INTEGRATION=1 后运行真实 Redis 验证")
	}
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	opts := client.Options()
	if !opts.ContextTimeoutEnabled || opts.MaxRetries != 0 || opts.MaxActiveConns != c.PoolSize {
		t.Fatal("超时、重试或容量配置未生效")
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if err := client.Ping(canceled).Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("命令未遵守取消: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(ctx).Err(); !errors.Is(err, goredis.ErrClosed) {
		t.Fatalf("关闭后仍可用: %v", err)
	}
}
