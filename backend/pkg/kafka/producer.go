package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/twmb/franz-go/pkg/kgo"
	"strconv"
	"time"
)

// Receipt 仅在 Broker ACK 成功后返回，Offset 是写入位置而非消费提交位置。
type Receipt struct {
	Topic     string
	Partition int32
	Offset    int64
}

// Producer 拥有独立客户端，调用方结束时必须 Close。
type Producer struct {
	client  *kgo.Client
	topics  Topics
	timeout time.Duration
}

// NewProducer 创建 acks=all 且保留默认幂等机制的生产者，操作超时不延长上游 Deadline。
func NewProducer(cfg Config, topics Topics, timeout time.Duration) (*Producer, error) {
	if err := topics.Validate(); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("发布超时必须为正")
	}
	client, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Producer{client, topics, timeout}, nil
}

// Publish 等待最终发送结果；错误可能表示结果未知，调用者必须复用持久化 event_id 重试。
func (p *Producer) Publish(ctx context.Context, event Envelope) (Receipt, error) {
	if err := event.Validate(); err != nil {
		return Receipt{}, err
	}
	topic, err := p.topics.EventTopic(event.AggregateType)
	if err != nil {
		return Receipt{}, err
	}
	event.OccurredAt = event.OccurredAt.UTC()
	data, err := json.Marshal(event)
	if err != nil {
		return Receipt{}, err
	}
	bounded, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	record := &kgo.Record{Topic: topic, Key: []byte(strconv.FormatInt(event.AggregateID, 10)), Value: data, Timestamp: event.OccurredAt}
	if err := p.client.ProduceSync(bounded, record).FirstErr(); err != nil {
		return Receipt{}, fmt.Errorf("Kafka 发布未确认: %w", err)
	}
	return Receipt{record.Topic, record.Partition, record.Offset}, nil
}

// Close 释放生产者连接，应在所有 Publish 调用返回后执行。
func (p *Producer) Close() { p.client.Close() }
