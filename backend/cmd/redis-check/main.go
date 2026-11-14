package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	redisx "remarket/backend/pkg/redis"
)

// main 执行只读连接探测，失败时以非零状态退出。
func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

// run 加载配置并在信号与启动超时约束下验证 Redis，不写业务 Key。
func run() error {
	cfg, err := redisx.LoadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client, err := redisx.Open(ctx, cfg)
	if err != nil {
		return err
	}
	// 探测结束立即释放连接，不遗留进程级连接池。
	defer client.Close()
	log.Print("Redis 连接验证通过")
	return nil
}
