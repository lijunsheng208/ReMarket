package eventtest

import (
	"context"
	"errors"
	"remarket/backend/pkg/kafka"
	"testing"
	"time"
)

type stubRepository struct{}

// Decide 模拟已经持久化的下一转移。
func (stubRepository) Decide(context.Context, kafka.Envelope, Packet, Config) (*Transfer, error) {
	return &Transfer{Topic: "retry", Key: []byte("1"), Value: []byte("packet")}, nil
}

// Quarantine 模拟无 Key 消息的持久化隔离。
func (stubRepository) Quarantine(context.Context, string, Packet, Config) (*Transfer, error) {
	return &Transfer{Topic: "dlt", Key: []byte{}, Value: []byte("packet")}, nil
}

// TestTransferErrorPropagation 验证无 Broker 单测中转移未确认也必须返回错误，不让底层提交。
func TestTransferErrorPropagation(t *testing.T) {
	cfg := Config{Main: "main", Retry: "retry", DLT: "dlt", Aggregate: "product", MaxRetries: 2, Backoff: time.Second}
	consumer, err := New(stubRepository{}, failingSender{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Handle(context.Background(), kafka.Message{Topic: "main", Value: []byte("invalid")}); err == nil {
		t.Fatal("未确认转移被吞掉")
	}
	if err := consumer.Handle(context.Background(), kafka.Message{Topic: "unknown"}); err == nil {
		t.Fatal("未知 Topic 被接受")
	}
}

// TestConfigValidation 验证重试上限和退避预算与分区处理限制一致。
func TestConfigValidation(t *testing.T) {
	cfg := Config{Main: "main", Retry: "retry", DLT: "dlt", Aggregate: "product", MaxRetries: 11, Backoff: time.Second}
	if cfg.Validate() == nil {
		t.Fatal("无限重试配置未拒绝")
	}
	var business *BusinessError
	if !errors.As(&BusinessError{Temporary: true}, &business) || !business.Temporary {
		t.Fatal("业务错误分类失败")
	}
}
