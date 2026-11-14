package eventtest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"remarket/backend/pkg/kafka"
	"time"
)

// BusinessError 区分测试业务的暂时/永久错误；普通数据库错误必须直接回滚重投。
type BusinessError struct{ Temporary bool }

// Error 返回固定分类，不持久化业务敏感信息。
func (e *BusinessError) Error() string { return "测试业务处理失败" }

// Apply 必须在传入事务中提交本消费者的业务结果，不允许外部不可回滚副作用。
type Apply func(context.Context, *sql.Tx, kafka.Envelope) error

// Store 拥有独立消费者库的连接引用，不共享业务服务模型。
type Store struct {
	db    *sql.DB
	apply Apply
}

// NewStore 注入事务内处理器，nil 使用版本化测试视图处理器。
func NewStore(db *sql.DB, apply Apply) *Store {
	if apply == nil {
		apply = ApplyView
	}
	return &Store{db, apply}
}

// ApplyView 只为更高聚合版本更新可重建测试视图，旧版或同版事件不覆盖新值。
func ApplyView(ctx context.Context, tx *sql.Tx, e kafka.Envelope) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO event_views(aggregate_type,aggregate_id,aggregate_version,event_id,payload) VALUES(?,?,?,?,?) ON DUPLICATE KEY UPDATE event_id=IF(VALUES(aggregate_version)>aggregate_version,VALUES(event_id),event_id),payload=IF(VALUES(aggregate_version)>aggregate_version,VALUES(payload),payload),aggregate_version=GREATEST(aggregate_version,VALUES(aggregate_version))`, e.AggregateType, e.AggregateID, e.AggregateVersion, e.EventID, []byte(e.Data))
	return err
}

// transferFrom 读取已持久化决定，重复投递不能增加业务尝试次数。
func transferFrom(ctx context.Context, tx *sql.Tx, id string) (*Transfer, error) {
	var t Transfer
	err := tx.QueryRowContext(ctx, "SELECT target_topic,message_key,message_value FROM event_transfers WHERE decision_id=?", id).Scan(&t.Topic, &t.Key, &t.Value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

// saveTransfer 在当前事务保存不可变转移消息，ACK 丢失后复用完全相同字节。
func saveTransfer(ctx context.Context, tx *sql.Tx, id string, p Packet, target string) (*Transfer, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	key := append([]byte{}, p.Key...)
	t := &Transfer{target, key, raw}
	_, err = tx.ExecContext(ctx, "INSERT INTO event_transfers(decision_id,target_topic,message_key,message_value) VALUES(?,?,?,?)", id, target, key, raw)
	return t, err
}

// Quarantine 对无效信封按原始 Kafka 坐标持久化死信决定。
func (s *Store) Quarantine(ctx context.Context, id string, p Packet, cfg Config) (*Transfer, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	t, err := transferFrom(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		t, err = saveTransfer(ctx, tx, id, p, cfg.DLT)
		if err != nil {
			return nil, err
		}
	}
	return t, tx.Commit()
}

// Decide 原子记录业务结果、event_id 去重及下一次可靠转移；业务失败用保存点回滚其写入。
func (s *Store) Decide(ctx context.Context, e kafka.Envelope, p Packet, cfg Config) (*Transfer, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	canonical, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	fingerprint := digest(canonical)
	_, err = tx.ExecContext(ctx, "INSERT INTO event_inbox(event_id,fingerprint,round_id) VALUES(?,?,?) ON DUPLICATE KEY UPDATE event_id=event_id", e.EventID, fingerprint, p.Round)
	if err != nil {
		return nil, err
	}
	var saved, round, status string
	var stage int
	err = tx.QueryRowContext(ctx, "SELECT fingerprint,round_id,stage,status FROM event_inbox WHERE event_id=? FOR UPDATE", e.EventID).Scan(&saved, &round, &stage, &status)
	if err != nil {
		return nil, err
	}
	decisionID := digest([]byte(fmt.Sprintf("%s/%s/%d", e.EventID, p.Round, p.Stage)))
	if saved != fingerprint {
		p.Reason = "event_id_conflict"
		id := digest(append([]byte(decisionID), canonical...))
		t, err := transferFrom(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if t == nil {
			t, err = saveTransfer(ctx, tx, id, p, cfg.DLT)
			if err != nil {
				return nil, err
			}
		}
		return t, tx.Commit()
	}
	if status == "DONE" {
		return nil, tx.Commit()
	}
	if old, err := transferFrom(ctx, tx, decisionID); err != nil {
		return nil, err
	} else if old != nil {
		return old, tx.Commit()
	}
	if round != p.Round {
		if status != "DLT" || !p.Replay || p.Stage != 0 {
			return nil, tx.Commit()
		}
		round = p.Round
		stage = 0
	}
	if p.Stage != stage {
		return nil, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, "SAVEPOINT business_effect"); err != nil {
		return nil, err
	}
	err = s.apply(ctx, tx, e)
	var transfer *Transfer
	if err == nil {
		status = "DONE"
	} else {
		var business *BusinessError
		if !errors.As(err, &business) {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT business_effect"); err != nil {
			return nil, err
		}
		target := cfg.DLT
		status = "DLT"
		p.Reason = "permanent_business_error"
		if business.Temporary && stage < cfg.MaxRetries {
			status = "RETRY"
			target = cfg.Retry
			p.Stage = stage + 1
			p.ReadyAt = time.Now().UTC().Add(cfg.Backoff)
			p.Reason = "temporary_business_error"
		} else if business.Temporary {
			p.Reason = "retry_exhausted"
		}
		transfer, err = saveTransfer(ctx, tx, decisionID, p, target)
		if err != nil {
			return nil, err
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE event_inbox SET round_id=?,stage=?,status=?,updated_at=UTC_TIMESTAMP(6) WHERE event_id=?", round, p.Stage, status, e.EventID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return transfer, nil
}
