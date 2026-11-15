// search-consumer 显式启动独立搜索消费者，不提供查询或自动建索引。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"remarket/backend/internal/search/consumer"
	"remarket/backend/internal/search/repository"
	"remarket/backend/pkg/kafka"
	"syscall"
	"time"
)

// main 仅输出稳定错误分类，不记录原始消息、连接凭据或死信包。
func main() {
	if err := run(); err != nil {
		slog.Error("Search 消费者退出", "category", "startup_or_replay_failed")
		os.Exit(1)
	}
}

// run 复用公共手动提交消费者，取消后先停止消费再释放发送器及 ES 连接。
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	endpoint := os.Getenv("ELASTICSEARCH_URL")
	if endpoint == "" {
		port := os.Getenv("ELASTICSEARCH_PORT")
		if port == "" {
			port = "9200"
		}
		endpoint = "http://127.0.0.1:" + port
	}
	target := os.Getenv("SEARCH_WRITE_INDEX")
	if target == "" {
		target = "products_v1"
	}
	repo, err := repository.New(repository.Config{Endpoint: endpoint, Index: target, Timeout: 3 * time.Second, Username: os.Getenv("ELASTICSEARCH_USERNAME"), Password: os.Getenv("ELASTICSEARCH_PASSWORD")})
	if err != nil {
		return err
	}
	defer repo.Close()
	if err := repo.CheckServer(ctx); err != nil {
		return err
	}
	if err := repo.CheckIndex(ctx); err != nil {
		return err
	}
	topics, err := kafka.LoadTopics()
	if err != nil {
		return err
	}
	// 独立 retry/DLT，避免 Search 与测试消费者互相提交重试包。
	topics.ProductRetry = "remarket.search.product.retry.v1"
	topics.ProductDLT = "remarket.search.product.dlt.v1"
	cfg := consumer.Config{Main: topics.Product, Retry: topics.ProductRetry, DLT: topics.ProductDLT, MaxRetries: 3, Backoff: time.Second}
	connection, err := kafka.LoadConfig()
	if err != nil {
		return err
	}
	sender, err := kafka.NewProducer(connection, topics, 5*time.Second)
	if err != nil {
		return err
	}
	defer sender.Close()
	handler, err := consumer.New(repo, sender, cfg)
	if err != nil {
		return err
	}
	if len(os.Args) > 1 {
		if len(os.Args) != 3 || os.Args[1] != "replay" {
			return fmt.Errorf("用法: search-consumer [replay packet.json]")
		}
		file, err := os.Open(os.Args[2])
		if err != nil {
			return fmt.Errorf("无法读取重放文件")
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || info.Size() > 512<<10 {
			return fmt.Errorf("重放文件过大")
		}
		var p consumer.Packet
		decoder := json.NewDecoder(io.LimitReader(file, (512<<10)+1))
		if decoder.Decode(&p) != nil {
			return fmt.Errorf("重放包无效")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return fmt.Errorf("重放文件包含多段 JSON")
		}
		return handler.Replay(ctx, p)
	}
	group := os.Getenv("SEARCH_CONSUMER_GROUP")
	if group == "" {
		group = "remarket-search-v1"
	}
	for ctx.Err() == nil {
		worker, err := kafka.NewConsumer(connection, kafka.ConsumerConfig{Group: group, Topics: []string{cfg.Main, cfg.Retry}, HandleTimeout: 15 * time.Second, CommitTimeout: 5 * time.Second})
		if err != nil {
			return err
		}
		err = worker.Run(ctx, handler.Handle)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			slog.Warn("Search 消费者重建", "category", "consume_or_commit_failed")
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}
