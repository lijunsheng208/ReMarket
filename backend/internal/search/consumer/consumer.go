// Package consumer 将商品事件同步到 Search 索引，失败转移采用至少一次语义。
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"remarket/backend/internal/search/repository"
	"remarket/backend/pkg/kafka"
	"strconv"
	"time"
	"unicode/utf8"
)

// Indexer 仅依赖 Search 自有快照；旧版本返回 false/nil。
type Indexer interface {
	Put(context.Context, repository.Document) (bool, error)
}

// Sender 返回成功前必须收到 Broker ACK。
type Sender interface {
	PublishRaw(context.Context, string, []byte, []byte) (kafka.Receipt, error)
}

// Config 定义 Search 独立重试链路及有界退避。
type Config struct {
	Main, Retry, DLT string
	MaxRetries       int
	Backoff          time.Duration
}

// Packet 保留原始消息及来源位置；不记录凭据或错误正文。
type Packet struct {
	Version   int       `json:"version"`
	Source    string    `json:"source"`
	Partition int32     `json:"partition"`
	Offset    int64     `json:"offset,string"`
	Key       []byte    `json:"key"`
	Value     []byte    `json:"value"`
	Attempt   int       `json:"attempt"`
	ReadyAt   time.Time `json:"ready_at"`
	Reason    string    `json:"reason"`
}

// Consumer 不持有 Offset；Handle 成功后由公共 Kafka Consumer 提交。
type Consumer struct {
	index  Indexer
	sender Sender
	cfg    Config
}

// New 校验依赖及 Topic 互异；运行入口负责配置 Search 独立失败链路。
func New(index Indexer, sender Sender, cfg Config) (*Consumer, error) {
	pattern := regexp.MustCompile(`^[a-zA-Z0-9._-]{1,249}$`)
	seen := map[string]bool{}
	for _, t := range []string{cfg.Main, cfg.Retry, cfg.DLT} {
		if !pattern.MatchString(t) || t == "." || t == ".." || seen[t] {
			return nil, fmt.Errorf("Search Topic 配置无效")
		}
		seen[t] = true
	}
	if cfg.MaxRetries < 0 || cfg.MaxRetries > 10 || cfg.Backoff < time.Millisecond || cfg.Backoff > 5*time.Second || index == nil || sender == nil {
		return nil, fmt.Errorf("Search 消费依赖或重试参数无效")
	}
	return &Consumer{index, sender, cfg}, nil
}

// decode 验证事件信封、Key 和状态转换快照，不导入 Product 内部模型。
func decode(key, value []byte) (repository.Document, error) {
	if !utf8.Valid(value) {
		return repository.Document{}, repository.ErrInvalid
	}
	e, err := kafka.DecodeEnvelope(value)
	if err != nil {
		return repository.Document{}, repository.ErrInvalid
	}
	var data struct {
		ProductID        int64     `json:"product_id,string"`
		SellerID         int64     `json:"seller_id,string"`
		CategoryID       int64     `json:"category_id,string"`
		Title            string    `json:"title"`
		Description      string    `json:"description"`
		PriceCent        int64     `json:"price_cent,string"`
		Status           string    `json:"status"`
		FirstPublishedAt time.Time `json:"first_published_at"`
	}
	status := map[string]string{"product.published.v1": "ON_SALE", "product.updated.v1": "ON_SALE", "product.released.v1": "ON_SALE", "product.off_shelf.v1": "OFF_SHELF", "product.reserved.v1": "RESERVED", "product.sold.v1": "SOLD"}[e.EventType]
	if e.AggregateType != "product" || status == "" || string(key) != strconv.FormatInt(e.AggregateID, 10) || json.Unmarshal(e.Data, &data) != nil || data.ProductID != e.AggregateID || data.Status != status || data.FirstPublishedAt.After(e.OccurredAt) {
		return repository.Document{}, repository.ErrInvalid
	}
	d := repository.Document{ProductID: data.ProductID, SellerID: data.SellerID, CategoryID: data.CategoryID, Title: data.Title, Description: data.Description, PriceCent: data.PriceCent, AggregateVersion: e.AggregateVersion, Status: data.Status, FirstPublishedAt: data.FirstPublishedAt}
	return d, d.Validate()
}

// transfer 先等待转移 ACK，再允许提交原 Offset；崩溃可能产生重复包，ES 版本保证效果幂等。
func (c *Consumer) transfer(ctx context.Context, topic string, p Packet, reason string) error {
	p.Reason = reason
	raw, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("Search 转移编码失败")
	}
	_, err = c.sender.PublishRaw(ctx, topic, p.Key, raw)
	return err
}

// Handle 串行处理主/重试消息；取消不转移、不提交，存储失败按消息中的次数有界重试。
func (c *Consumer) Handle(ctx context.Context, m kafka.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.Topic != c.cfg.Main && m.Topic != c.cfg.Retry {
		return fmt.Errorf("Search 非预期 Topic")
	}
	p := Packet{Version: 1, Source: c.cfg.Main, Partition: m.Partition, Offset: m.Offset, Key: m.Key, Value: m.Value}
	if m.Topic == c.cfg.Retry {
		if len(m.Value) > 512<<10 || json.Unmarshal(m.Value, &p) != nil || p.Version != 1 || p.Source != c.cfg.Main || p.Attempt < 1 || p.Attempt > c.cfg.MaxRetries || p.ReadyAt.IsZero() || p.ReadyAt.After(time.Now().Add(c.cfg.Backoff+time.Second)) || string(p.Key) != string(m.Key) {
			p = Packet{Version: 1, Source: c.cfg.Retry, Partition: m.Partition, Offset: m.Offset, Key: m.Key, Value: m.Value}
			return c.transfer(ctx, c.cfg.DLT, p, "invalid_retry_packet")
		}
	}
	if len(p.Value) > 256<<10 {
		return c.transfer(ctx, c.cfg.DLT, p, "message_too_large")
	}
	d, err := decode(p.Key, p.Value)
	if err != nil {
		return c.transfer(ctx, c.cfg.DLT, p, "invalid_event")
	}
	if wait := time.Until(p.ReadyAt); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	_, err = c.index.Put(ctx, d)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, repository.ErrInvalid) {
		return c.transfer(ctx, c.cfg.DLT, p, "invalid_document")
	}
	if p.Attempt >= c.cfg.MaxRetries {
		return c.transfer(ctx, c.cfg.DLT, p, "index_retries_exhausted")
	}
	p.Attempt++
	p.ReadyAt = time.Now().UTC().Add(c.cfg.Backoff)
	return c.transfer(ctx, c.cfg.Retry, p, "index_unavailable")
}

// Replay 校验操作员提供的死信包，将修复后的原事件重置到首轮重试；不自动消费死信。
func (c *Consumer) Replay(ctx context.Context, p Packet) error {
	if p.Version != 1 || p.Source != c.cfg.Main || len(p.Value) > 256<<10 {
		return repository.ErrInvalid
	}
	if _, err := decode(p.Key, p.Value); err != nil {
		return err
	}
	if c.cfg.MaxRetries < 1 {
		return fmt.Errorf("重放要求至少一次重试")
	}
	p.Attempt = 1
	p.ReadyAt = time.Now().UTC()
	return c.transfer(ctx, c.cfg.Retry, p, "operator_replay")
}
