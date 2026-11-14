// Package outbox 提供服务本地消息投递机制，不共享业务模型或访问其他服务库。
package outbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"remarket/backend/pkg/kafka"
	"time"
)

// Task 是投递元信息投影，不是共享 GORM 业务模型。
type Task struct {
	ID       int64
	Token    string
	Attempts int
	Event    kafka.Envelope
}

// Store 仅使用入口注入的本服务连接池；不关闭连接池。
type Store struct {
	db        *sql.DB
	aggregate string
}

// NewStore 限制服务聚合类型，防止错误配置向其他 Topic 投递。
func NewStore(db *sql.DB, aggregate string) (*Store, error) {
	if db == nil || (aggregate != "product" && aggregate != "order") {
		return nil, errors.New("Outbox 存储配置无效")
	}
	return &Store{db, aggregate}, nil
}

// Check 在启用时验证投递字段存在，不执行迁移。
func (s *Store) Check(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT id,event_id,event_type,aggregate_type,aggregate_id,aggregate_version,schema_version,payload,occurred_at,publish_status,attempts,next_attempt_at,locked_until,lock_token,published_at,last_error FROM outbox_events LIMIT 0")
	if err != nil {
		return err
	}
	return rows.Close()
}

// Claim 用短事务领取到期记录或接管过期租约，不持有事务等待 Kafka。
func (s *Store) Claim(ctx context.Context, lease time.Duration) (*Task, error) {
	if lease < time.Millisecond {
		return nil, errors.New("Outbox 租约无效")
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	task := &Task{Token: hex.EncodeToString(raw[:])}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT id,event_id,event_type,aggregate_type,aggregate_id,aggregate_version,schema_version,payload,occurred_at,attempts FROM outbox_events WHERE aggregate_type=? AND ((publish_status='PENDING' AND next_attempt_at<=UTC_TIMESTAMP(6)) OR (publish_status='PROCESSING' AND locked_until<=UTC_TIMESTAMP(6))) ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`, s.aggregate).Scan(&task.ID, &task.Event.EventID, &task.Event.EventType, &task.Event.AggregateType, &task.Event.AggregateID, &task.Event.AggregateVersion, &task.Event.SchemaVersion, &task.Event.Data, &task.Event.OccurredAt, &task.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE outbox_events SET publish_status='PROCESSING',attempts=LEAST(attempts+1,2147483647),lock_token=?,locked_until=TIMESTAMPADD(MICROSECOND,?,UTC_TIMESTAMP(6)) WHERE id=?`, task.Token, lease.Microseconds(), task.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if task.Attempts < 2147483647 {
		task.Attempts++
	}
	return task, nil
}

// Finish 仅允许未过期的当前令牌推进状态；PUBLISHED 对应计划中的 SENT。
func (s *Store) Finish(ctx context.Context, task *Task, status, category string, backoff time.Duration) error {
	if task == nil || task.ID <= 0 || task.Token == "" || backoff < 0 {
		return errors.New("Outbox 完成参数无效")
	}
	if status != "PUBLISHED" && status != "PENDING" && status != "FAILED" {
		return errors.New("Outbox 完成状态无效")
	}
	// 错误类别由内部固定枚举决定，不保存 Kafka/SQL 原始错误或业务载荷。
	if category != "" && category != "publish_unconfirmed" && category != "invalid_envelope" {
		return errors.New("Outbox 错误类别无效")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE outbox_events SET publish_status=?,last_error=NULLIF(?,''),published_at=IF(?='PUBLISHED',UTC_TIMESTAMP(6),NULL),next_attempt_at=TIMESTAMPADD(MICROSECOND,?,UTC_TIMESTAMP(6)),locked_until=NULL,lock_token=NULL WHERE id=? AND aggregate_type=? AND publish_status='PROCESSING' AND lock_token=? AND locked_until>UTC_TIMESTAMP(6)`, status, category, status, backoff.Microseconds(), task.ID, s.aggregate, task.Token)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("Outbox 租约已丢失")
	}
	return nil
}
