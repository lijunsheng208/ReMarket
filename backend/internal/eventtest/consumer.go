// Package eventtest 实现独立测试消费者，不读写 Product/Order 业务表。
package eventtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"remarket/backend/pkg/kafka"
	"strconv"
	"time"
)

// Packet 保存原消息及有限重试位置；字节字段使用 JSON base64，避免损坏原始消息。
type Packet struct {
	OriginTopic     string    `json:"origin_topic"`
	OriginPartition int32     `json:"origin_partition"`
	OriginOffset    int64     `json:"origin_offset,string"`
	Version         int       `json:"version"`
	Source          string    `json:"source"`
	Key             []byte    `json:"original_key"`
	Value           []byte    `json:"original_value"`
	Round           string    `json:"round"`
	Stage           int       `json:"stage"`
	ReadyAt         time.Time `json:"ready_at"`
	Replay          bool      `json:"replay"`
	Reason          string    `json:"reason"`
}

// Config 每个消费者只拥有一个业务聚合的主/重试/死信链路。
type Config struct {
	Main, Retry, DLT, Aggregate string
	MaxRetries                  int
	Backoff                     time.Duration
}

// Validate 限制回退等待，确保处理预算内可释放 rebalance。
func (c Config) Validate() error {
	if c.Aggregate != "product" && c.Aggregate != "order" {
		return fmt.Errorf("消费者聚合类型无效")
	}
	if c.Main == "" || c.Retry == "" || c.DLT == "" || c.Main == c.Retry || c.Main == c.DLT || c.Retry == c.DLT || c.MaxRetries < 0 || c.MaxRetries > 10 || c.Backoff < time.Millisecond || c.Backoff > 5*time.Second {
		return fmt.Errorf("消费者重试配置无效")
	}
	return nil
}

// Transfer 是持久化后的转移决定，发送失败时保留以便原消息再次处理。
type Transfer struct {
	Topic string
	Key   []byte `json:"original_key"`
	Value []byte `json:"original_value"`
}

// Repository 保证业务、去重和转移决定同事务提交。
type Repository interface {
	Decide(context.Context, kafka.Envelope, Packet, Config) (*Transfer, error)
	Quarantine(context.Context, string, Packet, Config) (*Transfer, error)
}

// Sender 必须等 Broker ACK 成功后才返回 nil。
type Sender interface {
	PublishRaw(context.Context, string, []byte, []byte) (kafka.Receipt, error)
}

// Consumer 不自动提交 Offset，成功返回交给 kafka.Consumer 提交。
type Consumer struct {
	repo   Repository
	sender Sender
	cfg    Config
}

// New 校验协议配置并注入独立持久化与发布器。
func New(repo Repository, sender Sender, cfg Config) (*Consumer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if repo == nil || sender == nil {
		return nil, fmt.Errorf("消费者依赖缺失")
	}
	return &Consumer{repo, sender, cfg}, nil
}

// digest 生成固定长度决定 ID，不保存 Kafka 错误或凭据。
func digest(value []byte) string { d := sha256.Sum256(value); return hex.EncodeToString(d[:]) }

// Handle 转移未获 ACK 则返回错误，原 Offset 不会提交；取消不脱离上游。
func (c *Consumer) Handle(ctx context.Context, m kafka.Message) error {
	p := Packet{Version: 1, Source: c.cfg.Main, Key: m.Key, Value: m.Value, OriginTopic: m.Topic, OriginPartition: m.Partition, OriginOffset: m.Offset}
	malformed := false
	if m.Topic == c.cfg.Retry {
		if json.Unmarshal(m.Value, &p) != nil || p.Version != 1 || p.Source != c.cfg.Main || p.Stage < 0 || p.Stage > c.cfg.MaxRetries || len(p.Round) > 32 || (p.Stage == 0 && !p.Replay) || p.ReadyAt.After(time.Now().Add(5*time.Second)) {
			malformed = true
			p = Packet{Version: 1, Source: c.cfg.Main, Key: m.Key, Value: m.Value, Reason: "invalid_retry_packet", OriginTopic: m.Topic, OriginPartition: m.Partition, OriginOffset: m.Offset}
		}
	} else if m.Topic != c.cfg.Main {
		return fmt.Errorf("非预期消费 Topic")
	}
	var transfer *Transfer
	var err error
	e, decodeErr := kafka.DecodeEnvelope(p.Value)
	if !malformed && decodeErr == nil && (e.AggregateType != c.cfg.Aggregate || string(p.Key) != strconv.FormatInt(e.AggregateID, 10)) {
		decodeErr = fmt.Errorf("事件来源或 Key 无效")
	}
	if malformed || decodeErr != nil {
		p.Reason = "invalid_message"
		id := digest([]byte(fmt.Sprintf("%s/%d/%d", m.Topic, m.Partition, m.Offset)))
		transfer, err = c.repo.Quarantine(ctx, id, p, c.cfg)
	} else {
		if wait := time.Until(p.ReadyAt); wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
		transfer, err = c.repo.Decide(ctx, e, p, c.cfg)
	}
	if err != nil {
		return err
	}
	if transfer == nil {
		return nil
	}
	_, err = c.sender.PublishRaw(ctx, transfer.Topic, transfer.Key, transfer.Value)
	return err
}
