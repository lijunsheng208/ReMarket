package eventtest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"os"
	"remarket/backend/pkg/kafka"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fixture struct {
	db         *sql.DB
	ctx        context.Context
	cfg        Config
	connection kafka.Config
	topics     kafka.Topics
	sender     *kafka.Producer
	event      kafka.Envelope
}

// setup 创建独立消费者数据库及三条临时 Topic，不使用业务消费者组。
func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("REMARKET_TEST_MYSQL_DSN")
	if dsn == "" || os.Getenv("KAFKA_INTEGRATION") != "1" {
		t.Skip("需要 MySQL/Kafka 集成开关")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	cfg, err := driver.ParseDSN(dsn)
	check(t, err)
	admin, err := sql.Open("mysql", dsn)
	check(t, err)
	name := fmt.Sprintf("remarket_consumer_test_%d", time.Now().UnixNano())
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+name)
	check(t, err)
	t.Cleanup(func() {
		_, err := admin.Exec("DROP DATABASE " + name)
		if err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	cfg.DBName = name
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	db, err := sql.Open("mysql", cfg.FormatDSN())
	check(t, err)
	t.Cleanup(func() { db.Close() })
	raw, err := os.ReadFile("../../../migrations/eventtest/000001_core.up.sql")
	check(t, err)
	for _, stmt := range strings.Split(string(raw), ";") {
		if strings.TrimSpace(stmt) != "" {
			_, err := db.ExecContext(ctx, stmt)
			check(t, err)
		}
	}
	connection, err := kafka.LoadConfig()
	check(t, err)
	broker, err := kafka.NewClient(connection)
	check(t, err)
	t.Cleanup(broker.Close)
	topics := kafka.DefaultTopics()
	topics.Product = name + ".main"
	topics.ProductRetry = name + ".retry"
	topics.ProductDLT = name + ".dlt"
	names := []string{topics.Product, topics.ProductRetry, topics.ProductDLT}
	req := kmsg.NewPtrCreateTopicsRequest()
	req.TimeoutMillis = 10000
	for _, n := range names {
		req.Topics = append(req.Topics, kmsg.CreateTopicsRequestTopic{Topic: n, NumPartitions: 1, ReplicationFactor: 1})
	}
	response, err := req.RequestWith(ctx, broker)
	check(t, err)
	for _, r := range response.Topics {
		if r.ErrorCode != 0 {
			t.Fatal("创建测试 Topic 失败")
		}
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		d := kmsg.NewPtrDeleteTopicsRequest()
		d.TimeoutMillis = 10000
		d.TopicNames = names
		for _, n := range names {
			copy := n
			d.Topics = append(d.Topics, kmsg.DeleteTopicsRequestTopic{Topic: &copy})
		}
		_, err := d.RequestWith(cleanup, broker)
		if err != nil {
			t.Error(err)
		}
	})
	sender, err := kafka.NewProducer(connection, topics, 5*time.Second)
	check(t, err)
	t.Cleanup(sender.Close)
	return &fixture{db: db, ctx: ctx, cfg: Config{Main: topics.Product, Retry: topics.ProductRetry, DLT: topics.ProductDLT, Aggregate: "product", MaxRetries: 2, Backoff: time.Millisecond}, connection: connection, topics: topics, sender: sender, event: kafka.Envelope{EventID: "event-1", EventType: "product.updated.v1", AggregateType: "product", AggregateID: 1, AggregateVersion: 2, SchemaVersion: 1, OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"title":"test"}`)}}
}

// check 将测试错误定位到调用处。
func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// message 构造主链路原始消息。
func (f *fixture) message(t *testing.T) kafka.Message {
	t.Helper()
	raw, err := json.Marshal(f.event)
	check(t, err)
	return kafka.Message{Topic: f.cfg.Main, Key: []byte("1"), Value: raw}
}

// read 获取真实 Broker 已确认的转移消息，不提交消费者组 Offset。
func (f *fixture) read(t *testing.T, topic string, offset int64) kafka.Message {
	t.Helper()
	r, err := kgo.NewClient(kgo.SeedBrokers(f.connection.Brokers...), kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{topic: {0: kgo.NewOffset().At(offset)}}))
	check(t, err)
	defer r.Close()
	for {
		fetch := r.PollRecords(f.ctx, 1)
		check(t, f.ctx.Err())
		if errs := fetch.Errors(); len(errs) > 0 {
			t.Fatal(errs[0].Err)
		}
		for _, record := range fetch.Records() {
			return kafka.Message{Topic: topic, Partition: record.Partition, Offset: record.Offset, Key: record.Key, Value: record.Value}
		}
	}
}

// TestIdempotentAndVersion 验证同 event_id 并发去重、版本校验与业务失败保存点回滚。
func TestIdempotentAndVersion(t *testing.T) {
	f := setup(t)
	var calls atomic.Int32
	store := NewStore(f.db, func(ctx context.Context, tx *sql.Tx, e kafka.Envelope) error {
		calls.Add(1)
		return ApplyView(ctx, tx, e)
	})
	h, err := New(store, f.sender, f.cfg)
	check(t, err)
	m := f.message(t)
	var wg sync.WaitGroup
	fail := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); fail <- h.Handle(f.ctx, m) }()
	}
	wg.Wait()
	close(fail)
	for err := range fail {
		check(t, err)
	}
	if calls.Load() != 1 {
		t.Fatal("重复业务执行")
	}
	// 重建 Store 不丢失去重状态。
	rebuilt, _ := New(NewStore(f.db, nil), f.sender, f.cfg)
	check(t, rebuilt.Handle(f.ctx, m))
	f.event.EventID = "old-event"
	f.event.AggregateVersion = 1
	check(t, rebuilt.Handle(f.ctx, f.message(t)))
	var version int64
	check(t, f.db.QueryRow("SELECT aggregate_version FROM event_views").Scan(&version))
	if version != 2 {
		t.Fatal("旧版本覆盖新值")
	}
	// 同 ID 内容变化必须死信，而非静默当成已处理。
	f.event.EventID = "event-1"
	check(t, rebuilt.Handle(f.ctx, f.message(t)))
	packet := f.read(t, f.cfg.DLT, 0)
	var p Packet
	check(t, json.Unmarshal(packet.Value, &p))
	if p.Reason != "event_id_conflict" {
		t.Fatal("冲突未隔离")
	}
}

// TestRetryDLTReplay 验证有限重试、真实 Retry/DLT ACK、持久化重放及去重。
func TestRetryDLTReplay(t *testing.T) {
	f := setup(t)
	var calls atomic.Int32
	var recovered atomic.Bool
	apply := func(ctx context.Context, tx *sql.Tx, e kafka.Envelope) error {
		calls.Add(1)
		if err := ApplyView(ctx, tx, e); err != nil {
			return err
		}
		if !recovered.Load() {
			return &BusinessError{Temporary: true}
		}
		return nil
	}
	h, err := New(NewStore(f.db, apply), f.sender, f.cfg)
	check(t, err)
	original := f.message(t)
	check(t, h.Handle(f.ctx, original))
	retry1 := f.read(t, f.cfg.Retry, 0)
	check(t, h.Handle(f.ctx, retry1))
	retry2 := f.read(t, f.cfg.Retry, 1)
	check(t, h.Handle(f.ctx, retry2))
	dead := f.read(t, f.cfg.DLT, 0)
	if calls.Load() != 3 {
		t.Fatal("重试次数未封顶")
	}
	var count int
	check(t, f.db.QueryRow("SELECT COUNT(*) FROM event_views").Scan(&count))
	if count != 0 {
		t.Fatal("失败业务没有回滚")
	}
	// 原主消息重复只会复用转移决定，不增加业务尝试。
	check(t, h.Handle(f.ctx, original))
	if calls.Load() != 3 {
		t.Fatal("重复消息重新执行业务")
	}
	var p Packet
	check(t, json.Unmarshal(dead.Value, &p))
	if p.Reason != "retry_exhausted" {
		t.Fatal("未进入死信")
	}
	recovered.Store(true)
	p.Round = "0123456789abcdef0123456789abcdef"
	p.Replay = true
	p.Stage = 0
	p.ReadyAt = time.Now().UTC()
	raw, err := json.Marshal(p)
	check(t, err)
	_, err = f.sender.PublishRaw(f.ctx, f.cfg.Retry, p.Key, raw)
	check(t, err)
	// retry offset2 是前面重复原消息产生的可接受重复转移，重放在 offset3。
	replay := f.read(t, f.cfg.Retry, 3)
	check(t, h.Handle(f.ctx, replay))
	check(t, h.Handle(f.ctx, replay))
	if calls.Load() != 4 {
		t.Fatal("重放重复执行业务")
	}
	check(t, f.db.QueryRow("SELECT COUNT(*) FROM event_views").Scan(&count))
	if count != 1 {
		t.Fatal("重放未落业务结果")
	}
}

type failingSender struct{}

// PublishRaw 模拟 Broker 未确认，不可让原 Offset 推进。
func (failingSender) PublishRaw(context.Context, string, []byte, []byte) (kafka.Receipt, error) {
	return kafka.Receipt{}, errors.New("injected transfer failure")
}

// TestTransferFailureOffset 验证 DLT 未获确认不提交，重建同组后重发并在 ACK 后推进。
func TestTransferFailureOffset(t *testing.T) {
	for _, mode := range []string{"dlt", "retry"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			value := []byte("invalid-json")
			var key []byte // 验证无 Key 的损坏消息也能可靠隔离。
			var apply Apply
			if mode == "retry" {
				message := f.message(t)
				value = message.Value
				key = message.Key
				apply = func(context.Context, *sql.Tx, kafka.Envelope) error { return &BusinessError{Temporary: true} }
			}
			_, err := f.sender.PublishRaw(f.ctx, f.cfg.Main, key, value)
			check(t, err)
			_, err = f.sender.PublishRaw(f.ctx, f.cfg.Main, []byte("stop"), []byte("stop"))
			check(t, err)
			opts := kafka.ConsumerConfig{Group: f.cfg.Main + ".group", Topics: []string{f.cfg.Main}, HandleTimeout: 10 * time.Second, CommitTimeout: 5 * time.Second}
			broker, err := kafka.NewClient(f.connection)
			check(t, err)
			defer broker.Close()
			defer func() {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				g := kmsg.NewPtrDeleteGroupsRequest()
				g.Groups = []string{opts.Group}
				_, err := g.RequestWith(cleanup, broker)
				if err != nil {
					t.Error(err)
				}
			}()
			bad, _ := New(NewStore(f.db, apply), failingSender{}, f.cfg)
			first, err := kafka.NewConsumer(f.connection, opts)
			check(t, err)
			if err := first.Run(f.ctx, bad.Handle); err == nil {
				t.Fatal("转移失败仍成功")
			}
			good, _ := New(NewStore(f.db, apply), f.sender, f.cfg)
			second, err := kafka.NewConsumer(f.connection, opts)
			check(t, err)
			sentinel := errors.New("stop")
			seen := 0
			err = second.Run(f.ctx, func(ctx context.Context, m kafka.Message) error {
				if string(m.Value) == "stop" {
					return sentinel
				}
				seen++
				return good.Handle(ctx, m)
			})
			if !errors.Is(err, sentinel) || seen != 1 {
				t.Fatalf("原消息未恢复: seen=%d err=%v", seen, err)
			}
			target := f.cfg.DLT
			if mode == "retry" {
				target = f.cfg.Retry
			}
			dead := f.read(t, target, 0)
			var packet Packet
			check(t, json.Unmarshal(dead.Value, &packet))
			if string(packet.Value) != string(value) {
				t.Fatal("死信原始消息丢失")
			}
			third, err := kafka.NewConsumer(f.connection, opts)
			check(t, err)
			err = third.Run(f.ctx, func(_ context.Context, m kafka.Message) error {
				if string(m.Value) != "stop" {
					t.Error("ACK 后 Offset 未推进")
				}
				return sentinel
			})
			if !errors.Is(err, sentinel) {
				t.Fatal(err)
			}
		})
	}

}

// TestPermanentFailure 验证永久错误直接死信，不安排业务重试且回滚已写入的视图。
func TestPermanentFailure(t *testing.T) {
	f := setup(t)
	var calls atomic.Int32
	store := NewStore(f.db, func(ctx context.Context, tx *sql.Tx, e kafka.Envelope) error {
		calls.Add(1)
		if err := ApplyView(ctx, tx, e); err != nil {
			return err
		}
		return &BusinessError{Temporary: false}
	})
	h, err := New(store, f.sender, f.cfg)
	check(t, err)
	check(t, h.Handle(f.ctx, f.message(t)))
	dead := f.read(t, f.cfg.DLT, 0)
	var p Packet
	check(t, json.Unmarshal(dead.Value, &p))
	if p.Reason != "permanent_business_error" || calls.Load() != 1 {
		t.Fatal("永久错误未直接死信")
	}
	var count int
	check(t, f.db.QueryRow("SELECT COUNT(*) FROM event_views").Scan(&count))
	if count != 0 {
		t.Fatal("永久失败业务未回滚")
	}
}
