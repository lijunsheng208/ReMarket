// event-consumer 是显式启动的测试消费者，不包含 Feed/Search 业务。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"remarket/backend/internal/eventtest"
	"remarket/backend/pkg/database"
	"remarket/backend/pkg/kafka"
	"strconv"
	"syscall"
	"time"
)

// main 运行测试消费者或显式重放，错误日志不输出消息或凭据。
func main() {
	if err := run(); err != nil {
		slog.Error("测试消费者退出", "error", err)
		os.Exit(1)
	}
}

// run 使用独立数据库及消费组，取消后关闭消费者再释放 Producer 与数据库。
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	aggregate := os.Getenv("EVENT_CONSUMER_AGGREGATE")
	if aggregate == "" {
		aggregate = "product"
	}
	topics, err := kafka.LoadTopics()
	if err != nil {
		return err
	}
	cfg := eventtest.Config{Aggregate: aggregate, MaxRetries: 3, Backoff: time.Second}
	if aggregate == "product" {
		cfg.Main, cfg.Retry, cfg.DLT = topics.Product, topics.ProductRetry, topics.ProductDLT
	} else if aggregate == "order" {
		cfg.Main, cfg.Retry, cfg.DLT = topics.Order, topics.OrderRetry, topics.OrderDLT
	} else {
		return fmt.Errorf("测试消费者聚合类型无效")
	}
	if raw := os.Getenv("EVENT_CONSUMER_MAX_RETRIES"); raw != "" {
		cfg.MaxRetries, err = strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("重试次数无效")
		}
	}
	if raw := os.Getenv("EVENT_CONSUMER_BACKOFF"); raw != "" {
		cfg.Backoff, err = time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("重试退避无效")
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	connection, err := kafka.LoadConfig()
	if err != nil {
		return err
	}
	sender, err := kafka.NewProducer(connection, topics, 10*time.Second)
	if err != nil {
		return err
	}
	defer sender.Close()
	if len(os.Args) > 1 {
		if len(os.Args) != 3 || os.Args[1] != "replay" {
			return fmt.Errorf("用法: event-consumer [replay DLT消息JSON文件]")
		}
		info, err := os.Stat(os.Args[2])
		if err != nil || info.Size() > 512*1024 {
			return fmt.Errorf("重放文件不存在或超过512KiB")
		}
		raw, err := os.ReadFile(os.Args[2])
		if err != nil {
			return fmt.Errorf("无法读取重放文件")
		}
		var p eventtest.Packet
		if json.Unmarshal(raw, &p) != nil || p.Version != 1 || p.Source != cfg.Main {
			return fmt.Errorf("重放包无效")
		}
		e, err := kafka.DecodeEnvelope(p.Value)
		if err != nil || e.AggregateType != aggregate {
			return fmt.Errorf("重放前需修复原始消息契约")
		}
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		p.Key = []byte(strconv.FormatInt(e.AggregateID, 10))
		p.Round = hex.EncodeToString(b[:])
		p.Stage = 0
		p.Replay = true
		p.ReadyAt = time.Now().UTC()
		p.Reason = "operator_replay"
		raw, err = json.Marshal(p)
		if err != nil {
			return err
		}
		_, err = sender.PublishRaw(ctx, cfg.Retry, []byte(strconv.FormatInt(e.AggregateID, 10)), raw)
		return err
	}
	// 独立消费者账号由管理员显式建库授权，禁止回退到 Product/Order 数据库。
	name := os.Getenv("EVENT_CONSUMER_MYSQL_DATABASE")
	if name == "" || name == "remarket_product" || name == "remarket_order" || name == "remarket_user" || name == "remarket_chat" {
		return fmt.Errorf("必须配置独立消费者数据库")
	}
	_, pool, err := database.OpenMySQL(ctx, database.MySQLConfig{Address: os.Getenv("EVENT_CONSUMER_MYSQL_ADDR"), Username: os.Getenv("EVENT_CONSUMER_MYSQL_USERNAME"), Password: os.Getenv("EVENT_CONSUMER_MYSQL_PASSWORD"), Database: name, ConnectTimeout: 3 * time.Second})
	if err != nil {
		return fmt.Errorf("消费者数据库连接失败")
	}
	defer pool.Close()
	for _, table := range []string{"event_inbox", "event_transfers", "event_views"} {
		rows, err := pool.QueryContext(ctx, "SELECT * FROM "+table+" LIMIT 0")
		if err != nil {
			return fmt.Errorf("消费者表未就绪，请先执行迁移")
		}
		rows.Close()
	}
	handler, err := eventtest.New(eventtest.NewStore(pool, nil), sender, cfg)
	if err != nil {
		return err
	}
	group := os.Getenv("EVENT_CONSUMER_GROUP")
	if group == "" {
		group = "remarket.eventtest." + aggregate + ".v1"
	}
	for ctx.Err() == nil {
		consumer, err := kafka.NewConsumer(connection, kafka.ConsumerConfig{Group: group, Topics: []string{cfg.Main, cfg.Retry}, HandleTimeout: 30 * time.Second, CommitTimeout: 5 * time.Second})
		if err != nil {
			return err
		}
		err = consumer.Run(ctx, handler.Handle)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			slog.Warn("测试消费者重建", "category", "consume_or_commit_failed")
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
