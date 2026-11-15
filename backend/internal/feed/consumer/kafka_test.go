package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/twmb/franz-go/pkg/kmsg"
	"google.golang.org/protobuf/types/known/timestamppb"
	"os"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/feed/timeline"
	"remarket/backend/pkg/kafka"
	"strings"
	"testing"
	"time"
)

// TestKafkaFeed 在唯一 Topic/Group 和 Redis 前缀验证重复投递、写入失败重投及成功提交。
func TestKafkaFeed(t *testing.T) {
	if os.Getenv("KAFKA_INTEGRATION") != "1" {
		t.Skip("设置 KAFKA_INTEGRATION=1 运行真实 Kafka 验证")
	}
	cfg, err := kafka.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	admin, err := kafka.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	name := fmt.Sprintf("remarket.test.feed.%d", time.Now().UnixNano())
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

	rc, e := timeline.LoadConfig()
	if e != nil {
		t.Fatal(e)
	}
	rc.Prefix = fmt.Sprintf("feed:test:kafka:%d:", time.Now().UnixNano())
	store, client, e := timeline.OpenLazy(rc)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	defer func() {
		keys, e := client.Keys(context.Background(), rc.Prefix+"*").Result()
		if e != nil {
			t.Error(e)
		} else if len(keys) > 0 {
			if e := client.Del(context.Background(), keys...).Err(); e != nil {
				t.Error(e)
			}
		}
	}()
	topics := kafka.DefaultTopics()
	topics.Product = name
	producer, e := kafka.NewProducer(cfg, topics, 5*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer producer.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	p := &productv1.Product{Id: 1, SellerId: 22, Status: productv1.ProductStatus_PRODUCT_STATUS_ON_SALE, CreatedAt: timestamppb.New(now)}
	token := strings.Repeat("f", 32)
	h, e := New(store, &userFake{total: 2, token: token}, &productFake{p: p}, producer, token, name+".dlt", 3)
	if e != nil {
		t.Fatal(e)
	}
	data, _ := json.Marshal(map[string]any{"product_id": "1", "seller_id": "22", "status": "ON_SALE", "created_at": now, "first_published_at": now})
	raw, _ := json.Marshal(kafka.Envelope{EventID: "kafka-feed", EventType: "product.published.v1", AggregateType: "product", AggregateID: 1, AggregateVersion: 2, SchemaVersion: 1, OccurredAt: now, Data: data})
	for i := 0; i < 2; i++ {
		if _, e := producer.PublishRaw(ctx, name, []byte("1"), raw); e != nil {
			t.Fatal(e)
		}
	}
	options := kafka.ConsumerConfig{Group: group, Topics: []string{name}, HandleTimeout: 3 * time.Second, CommitTimeout: 3 * time.Second}
	sentinel := errors.New("模拟写入后未提交")
	worker, e := kafka.NewConsumer(cfg, options)
	if e != nil {
		t.Fatal(e)
	}
	// 已写 Redis 后模拟提交前崩溃，重建后必须重投，ZSet 不得重复。
	e = worker.Run(ctx, func(ctx context.Context, m kafka.Message) error {
		if e := h.Handle(ctx, m); e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(e, sentinel) {
		t.Fatal(e)
	}
	worker, e = kafka.NewConsumer(cfg, options)
	if e != nil {
		t.Fatal(e)
	}
	count := 0
	e = worker.Run(ctx, func(ctx context.Context, m kafka.Message) error {
		if e := h.Handle(ctx, m); e != nil {
			return e
		}
		count++
		if count == 2 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(e, sentinel) || count != 2 {
		t.Fatalf("重投验证失败 %v %d", e, count)
	}
	if n := client.ZCard(ctx, rc.Prefix+"user:11").Val(); n != 1 {
		t.Fatalf("重复投递未幂等 %d", n)
	}
}
