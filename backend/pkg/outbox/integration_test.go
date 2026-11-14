package outbox

import (
	"context"
	"database/sql"
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

// TestOutboxIntegration 验证真实 MySQL/Kafka 双服务隔离、并发租约、ACK 与恢复重投。
func TestOutboxIntegration(t *testing.T) {
	dsn := os.Getenv("REMARKET_TEST_MYSQL_DSN")
	if dsn == "" || os.Getenv("KAFKA_INTEGRATION") != "1" {
		t.Skip("需要 REMARKET_TEST_MYSQL_DSN 与 KAFKA_INTEGRATION=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	connection, err := kafka.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	adminKafka, err := kafka.NewClient(connection)
	if err != nil {
		t.Fatal(err)
	}
	defer adminKafka.Close()
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("测试 DSN 无效")
	}
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, aggregate := range []string{"product", "order"} {
		t.Run(aggregate, func(t *testing.T) {
			name := fmt.Sprintf("remarket_outbox_test_%s_%d", aggregate, time.Now().UnixNano())
			if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := admin.Exec("DROP DATABASE " + name); err != nil {
					t.Error(err)
				}
			}()
			dbCfg := *cfg
			dbCfg.DBName = name
			dbCfg.ParseTime = true
			dbCfg.Loc = time.UTC
			db, err := sql.Open("mysql", dbCfg.FormatDSN())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			raw, err := os.ReadFile("../../../migrations/" + aggregate + "/000001_core.up.sql")
			if err != nil {
				t.Fatal(err)
			}
			start := strings.Index(string(raw), "CREATE TABLE outbox_events (")
			schema := string(raw)[start:]
			schema = schema[:strings.Index(schema, ";")+1]
			if _, err := db.ExecContext(ctx, schema); err != nil {
				t.Fatal(err)
			}
			topic := strings.ReplaceAll(name, "_", ".")
			request := kmsg.NewPtrCreateTopicsRequest()
			request.TimeoutMillis = 10000
			request.Topics = []kmsg.CreateTopicsRequestTopic{{Topic: topic, NumPartitions: 1, ReplicationFactor: 1}}
			response, err := request.RequestWith(ctx, adminKafka)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range response.Topics {
				if r.ErrorCode != 0 {
					t.Fatal("临时 Topic 创建失败")
				}
			}
			defer func() {
				clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
				defer stop()
				request := kmsg.NewPtrDeleteTopicsRequest()
				request.TimeoutMillis = 10000
				request.TopicNames = []string{topic}
				request.Topics = []kmsg.DeleteTopicsRequestTopic{{Topic: &topic}}
				if _, err := request.RequestWith(clean, adminKafka); err != nil {
					t.Error(err)
				}
			}()
			topics := kafka.DefaultTopics()
			if aggregate == "product" {
				topics.Product = topic
			} else {
				topics.Order = topic
			}
			sender, err := kafka.NewProducer(connection, topics, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer sender.Close()
			e := event(aggregate)
			_, err = db.ExecContext(ctx, `INSERT INTO outbox_events(event_id,event_type,aggregate_type,aggregate_id,aggregate_version,schema_version,payload,occurred_at) VALUES(?,?,?,?,?,?,?,?)`, e.EventID, e.EventType, e.AggregateType, e.AggregateID, e.AggregateVersion, e.SchemaVersion, []byte(e.Data), e.OccurredAt)
			if err != nil {
				t.Fatal(err)
			}
			store, err := NewStore(db, aggregate)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Check(ctx); err != nil {
				t.Fatal(err)
			}
			// 真实数据库配合发送结果未知，必须保留待重试且没有 published_at。
			seq := []string{}
			failurePublisher, err := New(store, &fakeSender{err: fmt.Errorf("injected"), sequence: &seq}, DefaultConfig(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if !failurePublisher.Step(ctx) {
				t.Fatal("失败路径未处理")
			}
			var failureStatus string
			var failurePublished sql.NullTime
			if err := db.QueryRowContext(ctx, "SELECT publish_status,published_at FROM outbox_events").Scan(&failureStatus, &failurePublished); err != nil {
				t.Fatal(err)
			}
			if failureStatus != "PENDING" || failurePublished.Valid {
				t.Fatal("发送错误仍被标记已发布")
			}
			if _, err := db.ExecContext(ctx, "UPDATE outbox_events SET next_attempt_at=UTC_TIMESTAMP(6)"); err != nil {
				t.Fatal(err)
			}
			var wins atomic.Int32
			var failures atomic.Int32
			var claimed *Task
			var mu sync.Mutex
			var wg sync.WaitGroup
			gate := make(chan struct{})
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-gate
					task, err := store.Claim(ctx, 30*time.Second)
					if err != nil {
						failures.Add(1)
					} else if task != nil {
						wins.Add(1)
						mu.Lock()
						claimed = task
						mu.Unlock()
					}
				}()
			}
			close(gate)
			wg.Wait()
			if wins.Load() != 1 || failures.Load() != 0 {
				t.Fatalf("并发认领异常 %d/%d", wins.Load(), failures.Load())
			}
			// 模拟 ACK 后进程退出：实际写 Kafka，刻意不标记数据库完成。
			if _, err := sender.Publish(ctx, claimed.Event); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, "UPDATE outbox_events SET locked_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND"); err != nil {
				t.Fatal(err)
			}
			replacement, err := NewStore(db, aggregate)
			if err != nil {
				t.Fatal(err)
			}
			task, err := replacement.Claim(ctx, 30*time.Second)
			if err != nil || task == nil || task.Token == claimed.Token {
				t.Fatal("租约接管失败")
			}
			if err := store.Finish(ctx, claimed, "PUBLISHED", "", 0); err == nil {
				t.Fatal("旧令牌可推进状态")
			}
			// 故障路径不标记已发布，回到 PENDING 后使用同一事件 ID 恢复。
			if err := replacement.Finish(ctx, task, "PENDING", "publish_unconfirmed", 0); err != nil {
				t.Fatal(err)
			}
			publisher, err := New(replacement, sender, DefaultConfig(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if !publisher.Step(ctx) {
				t.Fatal("重建后未恢复")
			}
			var status string
			var published sql.NullTime
			var attempts int
			if err := db.QueryRowContext(ctx, "SELECT publish_status,published_at,attempts FROM outbox_events").Scan(&status, &published, &attempts); err != nil {
				t.Fatal(err)
			}
			if status != "PUBLISHED" || !published.Valid || attempts != 4 {
				t.Fatal("ACK 或重试状态错误")
			}
			// 直接读取独立测试分区，验证结果未知窗口允许重复但逻辑 ID 不变。
			reader, err := kgo.NewClient(kgo.SeedBrokers(connection.Brokers...), kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{topic: {0: kgo.NewOffset().AtStart()}}))
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			count := 0
			for count < 2 {
				fetch := reader.PollRecords(ctx, 2-count)
				if err := ctx.Err(); err != nil {
					t.Fatal(err)
				}
				if errs := fetch.Errors(); len(errs) > 0 {
					t.Fatal(errs[0].Err)
				}
				for _, r := range fetch.Records() {
					decoded, err := kafka.DecodeEnvelope(r.Value)
					if err != nil || decoded.EventID != e.EventID || string(r.Key) != "1" {
						t.Fatal("重试改变了事件身份或分区 Key")
					}
					count++
				}
			}
			// 服务类型过滤不会认领其他聚合的记录。
			other := "product"
			if aggregate == "product" {
				other = "order"
			}
			wrong, _ := NewStore(db, other)
			if task, err := wrong.Claim(ctx, time.Second); err != nil || task != nil {
				t.Fatal("跨聚合领取")
			}
		})
	}
}
