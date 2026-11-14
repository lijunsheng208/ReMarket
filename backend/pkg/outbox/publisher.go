package outbox

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"remarket/backend/pkg/kafka"
	"sync"
	"time"
)

// Repository 定义服务本地租约及状态推进协议。
type Repository interface {
	Claim(context.Context, time.Duration) (*Task, error)
	Finish(context.Context, *Task, string, string, time.Duration) error
}

// Sender 成功必须表示 Broker ACK，不表示消费者已处理。
type Sender interface {
	Publish(context.Context, kafka.Envelope) (kafka.Receipt, error)
}

// Config 为投递并发、操作预算、租约和退避设置上界。
type Config struct {
	Concurrency                                                         int
	PollInterval, DBTimeout, PublishTimeout, Lease, RetryBase, RetryMax time.Duration
}

// DefaultConfig 返回开发初始值，租约覆盖发布及完成预算。
func DefaultConfig() Config {
	return Config{4, time.Second, 3 * time.Second, 10 * time.Second, 30 * time.Second, time.Second, time.Minute}
}

// Validate 防止短租约与无界工作者，运行中不续租。
func (c Config) Validate() error {
	if c.Concurrency < 1 || c.Concurrency > 64 || c.PollInterval <= 0 || c.PollInterval > time.Minute || c.DBTimeout <= 0 || c.DBTimeout > 30*time.Second || c.PublishTimeout <= 0 || c.PublishTimeout > time.Minute || c.Lease > 5*time.Minute || c.Lease <= c.PublishTimeout || c.Lease-c.PublishTimeout <= 2*c.DBTimeout || c.RetryBase < time.Millisecond || c.RetryMax < c.RetryBase || c.RetryMax > 24*time.Hour {
		return fmt.Errorf("Outbox 调度配置无效")
	}
	return nil
}

// Publisher 不拥有 Repository/Sender 资源，入口需在 Run 返回后关闭连接。
type Publisher struct {
	repo    Repository
	sender  Sender
	cfg     Config
	observe func(string)
}

// New 创建可独立验证的发布循环；observe 必须并发安全且不可阻塞。
func New(repo Repository, sender Sender, cfg Config, observe func(string)) (*Publisher, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if repo == nil || sender == nil {
		return nil, fmt.Errorf("Outbox 依赖缺失")
	}
	return &Publisher{repo, sender, cfg, observe}, nil
}

// note 只暴露固定类别，避免日志包含业务数据或凭据。
func (p *Publisher) note(event string) {
	if p.observe != nil {
		p.observe(event)
	}
}

// backoff 返回封顶指数退避并添加不超过四分之一的抖动。
func (p *Publisher) backoff(attempts int) time.Duration {
	d := p.cfg.RetryBase
	for i := 1; i < attempts && d < p.cfg.RetryMax; i++ {
		if d > p.cfg.RetryMax/2 {
			d = p.cfg.RetryMax
			break
		}
		d *= 2
	}
	room := min(d/4, p.cfg.RetryMax-d)
	if room > 0 {
		if n, err := rand.Int(rand.Reader, big.NewInt(int64(room))); err == nil {
			d += time.Duration(n.Int64())
		}
	}
	return d
}

// Step 完成一次认领和投递，任何未确认结果都不能标记已发布。
func (p *Publisher) Step(ctx context.Context) bool {
	bounded, cancel := context.WithTimeout(ctx, p.cfg.DBTimeout)
	task, err := p.repo.Claim(bounded, p.cfg.Lease)
	cancel()
	if err != nil {
		p.note("claim_failed")
		return false
	}
	if task == nil {
		return false
	}
	status, category := "PUBLISHED", ""
	if task.Event.Validate() != nil {
		status, category = "FAILED", "invalid_envelope"
	} else {
		send, stop := context.WithTimeout(ctx, p.cfg.PublishTimeout)
		_, err = p.sender.Publish(send, task.Event)
		stop()
		if err != nil {
			status, category = "PENDING", "publish_unconfirmed"
			p.note("publish_unconfirmed")
		}
	}
	// 上游取消时不另起后台确认，租约到期后重投，允许重复而不冒险丢失。
	if ctx.Err() != nil {
		return true
	}
	finish, stop := context.WithTimeout(ctx, p.cfg.DBTimeout)
	defer stop()
	if err := p.repo.Finish(finish, task, status, category, p.backoff(task.Attempts)); err != nil {
		p.note("finish_failed")
	}
	if status == "FAILED" {
		p.note("invalid_envelope")
	}
	if task.Attempts >= 10 {
		p.note("retry_threshold_exceeded")
	}
	return true
}

// Run 启动有界并发工作者，空闲/数据库错误按轮询间隔等待，取消后等待所有工作者退出。
func (p *Publisher) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < p.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if p.Step(ctx) {
					continue
				}
				timer := time.NewTimer(p.cfg.PollInterval)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
	wg.Wait()
}
