// feed-consumer 以独立消费组维护普通收件箱及热门作者动态。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	productv1 "remarket/backend/gen/product/v1"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/internal/feed/consumer"
	"remarket/backend/internal/feed/timeline"
	"remarket/backend/pkg/grpcx"
	"remarket/backend/pkg/kafka"
	"syscall"
	"time"
)

// main 输出稳定退出分类，不打印服务凭据和事件载荷。
func main() {
	if err := run(); err != nil {
		slog.Error("Feed 消费者退出", "category", "startup_or_consume_failed")
		os.Exit(1)
	}
}

// env 返回配置或明确开发默认值。
func env(key, value string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return value
}

// run 处理成功且索引完成后提交 Offset；失败重建消费组客户端，不能越过失败记录。
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := timeline.LoadConfig()
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return fmt.Errorf("Feed Redis 开关未启用")
	}
	store, redisClient, err := timeline.OpenLazy(cfg)
	if err != nil {
		return err
	}
	defer redisClient.Close()
	users, err := grpcx.Dial(env("USER_GRPC_TARGET", "127.0.0.1:50051"), nil, 3*time.Second)
	if err != nil {
		return err
	}
	defer users.Close()
	products, err := grpcx.Dial(env("PRODUCT_GRPC_TARGET", "127.0.0.1:50052"), nil, 3*time.Second)
	if err != nil {
		return err
	}
	defer products.Close()
	connection, err := kafka.LoadConfig()
	if err != nil {
		return err
	}
	topics, err := kafka.LoadTopics()
	if err != nil {
		return err
	}
	sender, err := kafka.NewProducer(connection, topics, 5*time.Second)
	if err != nil {
		return err
	}
	defer sender.Close()
	handler, err := consumer.New(store, userv1.NewUserServiceClient(users), productv1.NewProductServiceClient(products), sender, os.Getenv("FEED_USER_SERVICE_TOKEN"), "remarket.feed.product.dlt.v1", cfg.Threshold)
	if err != nil {
		return err
	}
	group := env("FEED_CONSUMER_GROUP", "remarket-feed-v1")
	if group == env("SEARCH_CONSUMER_GROUP", "remarket-search-v1") {
		return fmt.Errorf("Feed 消费组不得与 Search 共用")
	}
	for ctx.Err() == nil {
		worker, e := kafka.NewConsumer(connection, kafka.ConsumerConfig{Group: group, Topics: []string{topics.Product}, HandleTimeout: 30 * time.Second, CommitTimeout: 5 * time.Second})
		if e != nil {
			return e
		}
		e = worker.Run(ctx, handler.Handle)
		if ctx.Err() != nil {
			return nil
		}
		if e != nil {
			slog.Warn("Feed 消费重试", "category", "uncommitted_or_unavailable")
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
