package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"remarket/backend/internal/search/repository"
	"remarket/backend/pkg/kafka"
	"testing"
	"time"
)

type fakeIndex struct {
	calls   int
	err     error
	version int64
}

// Put 模拟 ES 严格版本行为并计数，不将旧事件覆盖新版本。
func (f *fakeIndex) Put(_ context.Context, d repository.Document) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	if d.AggregateVersion <= f.version {
		return false, nil
	}
	f.version = d.AggregateVersion
	return true, nil
}

type fakeSender struct {
	topic  string
	packet Packet
	err    error
	calls  int
}

// PublishRaw 记录重试决定，模拟 ACK 失败边界。
func (f *fakeSender) PublishRaw(_ context.Context, topic string, _, raw []byte) (kafka.Receipt, error) {
	f.calls++
	f.topic = topic
	_ = json.Unmarshal(raw, &f.packet)
	return kafka.Receipt{}, f.err
}

// event 构造与生产端字符串大整数约定一致的商品发布事件。
func event() kafka.Message {
	return kafka.Message{Topic: "main", Key: []byte("42"), Value: []byte(`{"event_id":"test-1","event_type":"product.published.v1","aggregate_type":"product","aggregate_id":"42","aggregate_version":"3","schema_version":1,"occurred_at":"2026-11-14T02:00:00Z","data":{"product_id":"42","seller_id":"1","category_id":"2","title":"手机","description":"正常","price_cent":"100","status":"ON_SALE","first_published_at":"2026-11-14T01:00:00Z"}}`)}
}

// setup 构建独立消费者及有界重试测试依赖。
func setup(t *testing.T) (*Consumer, *fakeIndex, *fakeSender) {
	t.Helper()
	i := &fakeIndex{}
	s := &fakeSender{}
	c, err := New(i, s, Config{Main: "main", Retry: "retry", DLT: "dlt", MaxRetries: 2, Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c, i, s
}

// TestVersionsAndValidation 验证重复及乱序幂等，并拒绝 Key 或状态不匹配。
func TestVersionsAndValidation(t *testing.T) {
	c, i, s := setup(t)
	i.version = 4
	if err := c.Handle(context.Background(), event()); err != nil || i.version != 4 || s.calls != 0 {
		t.Fatal("旧事件未被安全处理")
	}
	m := event()
	m.Key = []byte("43")
	if err := c.Handle(context.Background(), m); err != nil || s.topic != "dlt" || i.calls != 1 {
		t.Fatal("错误 Key 未隔离")
	}
	var e kafka.Envelope
	_ = json.Unmarshal(event().Value, &e)
	e.EventType = "product.sold.v1"
	m = event()
	m.Value, _ = json.Marshal(e)
	if err := c.Handle(context.Background(), m); err != nil || s.packet.Reason != "invalid_event" {
		t.Fatal("状态不匹配未隔离")
	}
}

// TestRetryACKAndExhaustion 验证转移失败返回错误，次数跨重试消息保留，耗尽进入死信。
func TestRetryACKAndExhaustion(t *testing.T) {
	c, i, s := setup(t)
	i.err = repository.ErrUnavailable
	s.err = errors.New("ack lost")
	if err := c.Handle(context.Background(), event()); err == nil {
		t.Fatal("ACK 失败不能允许提交")
	}
	s.err = nil
	if err := c.Handle(context.Background(), event()); err != nil || s.topic != "retry" || s.packet.Attempt != 1 {
		t.Fatal("首轮重试错误")
	}
	for attempt := 1; attempt <= 2; attempt++ {
		p := s.packet
		p.ReadyAt = time.Now().Add(-time.Second)
		raw, _ := json.Marshal(p)
		if err := c.Handle(context.Background(), kafka.Message{Topic: "retry", Key: []byte("42"), Value: raw}); err != nil {
			t.Fatal(err)
		}
	}
	if s.topic != "dlt" || s.packet.Attempt != 2 || s.packet.Reason != "index_retries_exhausted" {
		t.Fatal("重试未有界终止")
	}
}

// TestCancelReplayAndMalformed 验证取消不转移、损坏重试包隔离以及显式重放。
func TestCancelReplayAndMalformed(t *testing.T) {
	c, i, s := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Handle(ctx, event()); !errors.Is(err, context.Canceled) || i.calls != 0 || s.calls != 0 {
		t.Fatal("取消仍产生副作用")
	}
	if err := c.Handle(context.Background(), kafka.Message{Topic: "retry", Value: []byte("bad")}); err != nil || s.topic != "dlt" {
		t.Fatal("损坏包未隔离")
	}
	m := event()
	p := Packet{Version: 1, Source: "main", Key: m.Key, Value: m.Value}
	if err := c.Replay(context.Background(), p); err != nil || s.topic != "retry" || s.packet.Attempt != 1 {
		t.Fatal("重放失败")
	}
}

// TestAllProductStates 验证六种商品事件的快照状态映射，非在售快照仍写入版本。
func TestAllProductStates(t *testing.T) {
	for eventType, status := range map[string]string{"published": "ON_SALE", "updated": "ON_SALE", "released": "ON_SALE", "off_shelf": "OFF_SHELF", "reserved": "RESERVED", "sold": "SOLD"} {
		var e kafka.Envelope
		_ = json.Unmarshal(event().Value, &e)
		var data map[string]json.RawMessage
		_ = json.Unmarshal(e.Data, &data)
		data["status"], _ = json.Marshal(status)
		e.Data, _ = json.Marshal(data)
		e.EventType = "product." + eventType + ".v1"
		raw, _ := json.Marshal(e)
		d, err := decode([]byte("42"), raw)
		if err != nil || d.Status != status {
			t.Fatalf("%s: %v", eventType, err)
		}
	}
}
