package outbox

import (
	"context"
	"errors"
	"remarket/backend/pkg/kafka"
	"testing"
	"time"
)

type fakeRepo struct {
	task     *Task
	status   string
	finished int
	sequence *[]string
}

// Claim 模拟一次领取。
func (r *fakeRepo) Claim(context.Context, time.Duration) (*Task, error) {
	*r.sequence = append(*r.sequence, "claim")
	return r.task, nil
}

// Finish 记录 ACK 与状态推进顺序。
func (r *fakeRepo) Finish(_ context.Context, _ *Task, status, _ string, _ time.Duration) error {
	*r.sequence = append(*r.sequence, "finish")
	r.status = status
	r.finished++
	return nil
}

type fakeSender struct {
	err      error
	sequence *[]string
}

// Publish 模拟 Broker 确认或发送结果未知。
func (s *fakeSender) Publish(context.Context, kafka.Envelope) (kafka.Receipt, error) {
	*s.sequence = append(*s.sequence, "send")
	return kafka.Receipt{}, s.err
}

// event 构造不含业务模型依赖的测试信封。
func event(aggregate string) kafka.Envelope {
	return kafka.Envelope{EventID: "550e8400-e29b-41d4-a716-446655440000", EventType: aggregate + ".created.v1", AggregateType: aggregate, AggregateID: 1, AggregateVersion: 1, SchemaVersion: 1, OccurredAt: time.Now().UTC(), Data: []byte(`{"id":"1"}`)}
}

// TestACKOrdering 验证只有 ACK 成功才能标记已发布，错误回到待重试，无效信封单独隔离。
func TestACKOrdering(t *testing.T) {
	for _, mode := range []string{"ack", "unknown", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			seq := []string{}
			e := event("product")
			repo := &fakeRepo{task: &Task{ID: 1, Event: e, Attempts: 1}, sequence: &seq}
			sender := &fakeSender{sequence: &seq}
			if mode == "unknown" {
				sender.err = errors.New("unknown")
			}
			if mode == "invalid" {
				repo.task.Event.SchemaVersion = 2
			}
			p, err := New(repo, sender, DefaultConfig(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if !p.Step(context.Background()) {
				t.Fatal("未处理")
			}
			expected := "PUBLISHED"
			if mode == "unknown" {
				expected = "PENDING"
			}
			if mode == "invalid" {
				expected = "FAILED"
			}
			if repo.status != expected {
				t.Fatal("状态错误")
			}
			if seq[0] != "claim" || seq[len(seq)-1] != "finish" {
				t.Fatal("顺序错误")
			}
			if mode != "invalid" && (len(seq) != 3 || seq[1] != "send") {
				t.Fatal("ACK 前推进")
			}
			if d := p.backoff(1000); d > time.Minute || d < time.Second {
				t.Fatal("退避越界")
			}
		})
	}
}

// TestConfig 验证短租约和无界并发被拒绝。
func TestConfig(t *testing.T) {
	c := DefaultConfig()
	c.Lease = c.PublishTimeout
	if c.Validate() == nil {
		t.Fatal("短租约被接受")
	}
	c = DefaultConfig()
	c.Concurrency = 0
	if c.Validate() == nil {
		t.Fatal("无限并发被接受")
	}
}
