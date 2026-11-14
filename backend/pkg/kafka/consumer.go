package kafka

import (
	"context"
	"fmt"
	"github.com/twmb/franz-go/pkg/kgo"
	"strings"
	"sync/atomic"
	"time"
)

// ConsumerConfig 显式指定消费组及 Topic，无历史 Offset 时从最早保留记录开始。
type ConsumerConfig struct {
	Group                        string
	Topics                       []string
	HandleTimeout, CommitTimeout time.Duration
}

// Message 保留原始消息供业务校验或后续 DLT，处理器不得自行推进 Offset。
type Message struct {
	Topic      string
	Partition  int32
	Offset     int64
	Key, Value []byte
}

// Handler 成功返回 nil 表示业务已持久化完成；不得仅启动后台处理就返回成功。
type Handler func(context.Context, Message) error

// Consumer 串行处理并显式提交，一个实例只允许运行一次，失败后应重建。
type Consumer struct {
	client  *kgo.Client
	cfg     ConsumerConfig
	started atomic.Bool
}

// NewConsumer 禁用自动提交并限制每次一条，避免跨过未处理记录的 Offset 空洞。
func NewConsumer(cfg Config, consume ConsumerConfig) (*Consumer, error) {
	if strings.TrimSpace(consume.Group) == "" || len(consume.Topics) == 0 || consume.HandleTimeout <= 0 || consume.CommitTimeout <= 0 || consume.HandleTimeout > time.Minute || consume.CommitTimeout > time.Minute {
		return nil, fmt.Errorf("消费组或处理/提交超时无效")
	}
	consume.Topics = append([]string(nil), consume.Topics...)
	for _, t := range consume.Topics {
		if !topicPattern.MatchString(t) || t == "." || t == ".." {
			return nil, fmt.Errorf("消费 Topic 无效")
		}
	}
	if len(cfg.Brokers) == 0 || cfg.ConnectTimeout <= 0 {
		return nil, fmt.Errorf("Kafka 连接配置无效")
	}
	client, err := kgo.NewClient(kgo.SeedBrokers(cfg.Brokers...), kgo.ClientID(cfg.ClientID), kgo.DialTimeout(cfg.ConnectTimeout), kgo.ConsumerGroup(consume.Group), kgo.ConsumeTopics(consume.Topics...), kgo.DisableAutoCommit(), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()), kgo.BlockRebalanceOnPoll(), kgo.RebalanceTimeout(3*time.Minute))
	if err != nil {
		return nil, err
	}
	return &Consumer{client: client, cfg: consume}, nil
}

// Run 处理成功后同步提交；任何处理或提交错误立即退出并关闭客户端，禁止继续消费越过失败记录。
func (c *Consumer) Run(ctx context.Context, handler Handler) error {
	if handler == nil {
		return fmt.Errorf("消费处理器不能为空")
	}
	if !c.started.CompareAndSwap(false, true) {
		return fmt.Errorf("消费者不能重复运行")
	}
	defer c.client.CloseAllowingRebalance()
	for {
		fetches := c.client.PollRecords(ctx, 1)
		if err := ctx.Err(); err != nil {
			return err
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			return fmt.Errorf("Kafka 拉取失败: %w", errs[0].Err)
		}
		for _, record := range fetches.Records() {
			err := c.process(ctx, record, handler)
			c.client.AllowRebalance()
			if err != nil {
				return err
			}
		}
	}
}

// process 在同一轮分区所有权内处理和提交；取消时不脱离上游继续确认。
func (c *Consumer) process(ctx context.Context, record *kgo.Record, handler Handler) error {
	handle, cancel := context.WithTimeout(ctx, c.cfg.HandleTimeout)
	err := handler(handle, Message{Topic: record.Topic, Partition: record.Partition, Offset: record.Offset, Key: append([]byte(nil), record.Key...), Value: append([]byte(nil), record.Value...)})
	if err == nil {
		err = handle.Err()
	}
	cancel()
	if err != nil {
		return fmt.Errorf("Kafka 处理失败: %w", err)
	}
	commit, stop := context.WithTimeout(ctx, c.cfg.CommitTimeout)
	defer stop()
	if err := c.client.CommitRecords(commit, record); err != nil {
		return fmt.Errorf("Kafka Offset 未确认: %w", err)
	}
	return nil
}

// Close 允许未启动实例释放资源；运行中应先取消 Run 上下文并等待返回。
func (c *Consumer) Close() { c.client.CloseAllowingRebalance() }
