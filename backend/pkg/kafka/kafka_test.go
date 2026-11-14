package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/twmb/franz-go/pkg/kmsg"
	"os"
	"strings"
	"testing"
	"time"
)

// testEvent 构造高精度 ID 信封，逻辑事件 ID 在重试时保持不变。
func testEvent(id string) Envelope {
	return Envelope{EventID: id, EventType: "product.published.v1", AggregateType: "product", AggregateID: 9007199254740993, AggregateVersion: 2, SchemaVersion: 1, OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"product_id":"9007199254740993"}`)}
}

// TestEnvelope 验证现有 data 契约、大整数精度和非法元信息拒绝。
func TestEnvelope(t *testing.T) {
	e := testEvent("event-1")
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"aggregate_id":"9007199254740993"`) {
		t.Fatal("大整数丢失")
	}
	decoded, err := DecodeEnvelope(raw)
	if err != nil || decoded.AggregateID != e.AggregateID {
		t.Fatal("信封解码失败")
	}
	for _, change := range []func(*Envelope){func(e *Envelope) { e.Data = json.RawMessage(`null`) }, func(e *Envelope) { e.SchemaVersion = 2 }, func(e *Envelope) { e.AggregateType = "order" }, func(e *Envelope) { e.EventID = "" }} {
		bad := e
		change(&bad)
		if bad.Validate() == nil {
			t.Fatal("非法信封未拒绝")
		}
	}
}

// TestTopics 验证默认映射、重复名称和环境覆盖校验。
func TestTopics(t *testing.T) {
	topics := DefaultTopics()
	if topic, err := topics.EventTopic("order"); err != nil || topic != "remarket.order.events.v1" {
		t.Fatal("Topic 映射错误")
	}
	topics.Order = topics.Product
	if topics.Validate() == nil {
		t.Fatal("重复 Topic 被接受")
	}
	t.Setenv("KAFKA_PRODUCT_TOPIC", "bad topic")
	if _, err := LoadTopics(); err == nil {
		t.Fatal("非法 Topic 被接受")
	}
}

// TestKafkaIntegration 在唯一临时 Topic/Group 验证真实 ACK、失败重投与成功后提交。
func TestKafkaIntegration(t *testing.T) {
	if os.Getenv("KAFKA_INTEGRATION") != "1" {
		t.Skip("设置 KAFKA_INTEGRATION=1 运行真实 Kafka 验证")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	admin, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	name := fmt.Sprintf("remarket.test.wrapper.%d", time.Now().UnixNano())
	group := name + ".group"
	req := kmsg.NewPtrCreateTopicsRequest()
	req.TimeoutMillis = 10000
	req.Topics = []kmsg.CreateTopicsRequestTopic{{Topic: name, NumPartitions: 1, ReplicationFactor: 1}}
	response, err := req.RequestWith(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, topic := range response.Topics {
		if topic.ErrorCode != 0 {
			t.Fatalf("建测试 Topic 错误码 %d", topic.ErrorCode)
		}
	}
	// 只清理本次生成的 Topic 和 Group，不删除项目事件或其他消费者状态。
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		d := kmsg.NewPtrDeleteTopicsRequest()
		d.TopicNames = []string{name}
		d.Topics = []kmsg.DeleteTopicsRequestTopic{{Topic: &name}}
		d.TimeoutMillis = 10000
		if _, err := d.RequestWith(cleanup, admin); err != nil {
			t.Error("清理测试 Topic 失败")
		}
		g := kmsg.NewPtrDeleteGroupsRequest()
		g.Groups = []string{group}
		if _, err := g.RequestWith(cleanup, admin); err != nil {
			t.Error("清理测试 Group 失败")
		}
	}()
	topics := DefaultTopics()
	topics.Product = name
	producer, err := NewProducer(cfg, topics, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()
	for _, id := range []string{"first", "second"} {
		receipt, err := producer.Publish(ctx, testEvent(id))
		if err != nil || receipt.Topic != name || receipt.Offset < 0 {
			t.Fatalf("ACK 失败: %v", err)
		}
	}
	options := ConsumerConfig{Group: group, Topics: []string{name}, HandleTimeout: time.Second, CommitTimeout: 5 * time.Second}
	sentinel := errors.New("测试处理拒绝")
	// 首次处理失败不能提交，重建同组后必须再次收到 first。
	first, err := NewConsumer(cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	err = first.Run(ctx, func(_ context.Context, m Message) error {
		e, err := DecodeEnvelope(m.Value)
		if err != nil {
			return err
		}
		if e.EventID != "first" {
			t.Error("新组未从最早开始")
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("处理错误丢失: %v", err)
	}
	second, err := NewConsumer(cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	err = second.Run(ctx, func(_ context.Context, m Message) error {
		e, err := DecodeEnvelope(m.Value)
		if err != nil {
			return err
		}
		count++
		if count == 1 {
			if e.EventID != "first" {
				t.Error("失败事件被跳过")
			}
			return nil
		}
		if e.EventID != "second" {
			t.Error("顺序错误")
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) || count != 2 {
		t.Fatalf("提交前后处理失败: %v", err)
	}
	third, err := NewConsumer(cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	err = third.Run(ctx, func(_ context.Context, m Message) error {
		e, err := DecodeEnvelope(m.Value)
		if err != nil {
			return err
		}
		if e.EventID != "second" {
			t.Error("已成功提交的 first 被再次消费")
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("重建消费失败: %v", err)
	}
	if err := third.Run(ctx, func(context.Context, Message) error { return nil }); err == nil {
		t.Fatal("失败消费者被复用")
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := producer.Publish(canceled, testEvent("cancel")); err == nil {
		t.Fatal("发布取消未传播")
	}
}

// TestExistingProductContract 验证公共信封可直接读取已有商品消息，不改成 payload 字段。
func TestExistingProductContract(t *testing.T) {
	raw, err := os.ReadFile("../../../api/events/product/product.published.v1.example.json")
	if err != nil {
		t.Fatal(err)
	}
	e, err := DecodeEnvelope(raw)
	if err != nil || e.AggregateType != "product" || len(e.Data) == 0 {
		t.Fatalf("现有事件不兼容: %v", err)
	}
}
