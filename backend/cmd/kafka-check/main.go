package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"remarket/backend/pkg/kafka"
)

// main 检查 Kafka 协议连接，不写入业务消息；失败时以非零状态退出。
func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

// run 在受信号及超时约束的上下文内探测 Broker，并释放客户端连接。
func run() error {
	cfg, err := kafka.LoadConfig()
	if err != nil {
		return err
	}
	client, err := kafka.NewClient(cfg)
	if err != nil {
		return err
	}
	defer client.Close()
	parent, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(parent, cfg.ConnectTimeout)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		return err
	}
	log.Print("Kafka 连接验证通过")
	return nil
}
