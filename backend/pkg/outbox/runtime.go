package outbox

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"remarket/backend/pkg/kafka"
	"strconv"
	"strings"
	"time"
)

// LoadConfig 从 OUTBOX_* 环境变量读取部署调度配置，不记录输入值。
func LoadConfig() (Config, error) {
	c := DefaultConfig()
	if raw := os.Getenv("OUTBOX_CONCURRENCY"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return c, fmt.Errorf("OUTBOX_CONCURRENCY 必须为整数")
		}
		c.Concurrency = n
	}
	for key, target := range map[string]*time.Duration{"OUTBOX_POLL_INTERVAL": &c.PollInterval, "OUTBOX_DB_TIMEOUT": &c.DBTimeout, "OUTBOX_PUBLISH_TIMEOUT": &c.PublishTimeout, "OUTBOX_LEASE": &c.Lease, "OUTBOX_RETRY_BASE": &c.RetryBase, "OUTBOX_RETRY_MAX": &c.RetryMax} {
		if raw := os.Getenv(key); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return c, fmt.Errorf("%s 必须为时长", key)
			}
			*target = d
		}
	}
	return c, c.Validate()
}

// Start 在所属服务入口启动发布器，每个入口只注入自己的数据库连接；默认关闭。
func Start(ctx context.Context, db *sql.DB, aggregate string, observe func(string)) (func(), error) {
	if aggregate != "product" && aggregate != "order" {
		return nil, fmt.Errorf("Outbox 服务类型无效")
	}
	raw := os.Getenv(strings.ToUpper(aggregate) + "_OUTBOX_ENABLED")
	enabled := false
	var err error
	if raw != "" {
		enabled, err = strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("Outbox 开关必须为布尔值")
		}
	}
	if !enabled {
		return func() {}, nil
	}
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	store, err := NewStore(db, aggregate)
	if err != nil {
		return nil, err
	}
	probe, cancel := context.WithTimeout(ctx, cfg.DBTimeout)
	err = store.Check(probe)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("Outbox 表未就绪")
	}
	connection, err := kafka.LoadConfig()
	if err != nil {
		return nil, err
	}
	topics, err := kafka.LoadTopics()
	if err != nil {
		return nil, err
	}
	sender, err := kafka.NewProducer(connection, topics, cfg.PublishTimeout)
	if err != nil {
		return nil, err
	}
	publisher, err := New(store, sender, cfg, observe)
	if err != nil {
		sender.Close()
		return nil, err
	}
	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); publisher.Run(run) }()
	// Kafka 不可用时不阻止业务服务启动，已提交事件保留在本地任务中等待重试。
	return func() { stop(); <-done; sender.Close() }, nil
}
