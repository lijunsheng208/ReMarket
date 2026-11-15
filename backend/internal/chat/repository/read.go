package repository

import (
	"context"
	"database/sql"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"remarket/backend/internal/chat/model"
	"remarket/backend/pkg/apperror"
)

// ReadState 保存当前用户的权威已读游标及同一事务内计算的未读数。
type ReadState struct{ ConversationID, UserID, LastReadSeq, UnreadCount, LastSeq int64 }

// ReadResult 返回已提交的状态及服务端确定的通知参与者。
type ReadResult struct {
	State        ReadState
	Participants []int64
}

// SyncPage 固定当前补拉轮次的序号上界，消息始终正序。
type SyncPage struct {
	Messages []model.Message
	Until    int64
}

// accessDenied 隐藏不存在会话与非成员的区别。
func accessDenied() error {
	return &apperror.Error{Kind: apperror.Forbidden, Message: "无权访问会话"}
}

// Read 按与发送相同的会话→成员锁顺序推进游标，避免并发发送/已读产生锁顺序反转。
func (r *Repository) Read(ctx context.Context, user, conversation, seq int64) (*ReadResult, error) {
	var result ReadResult
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var c model.Conversation
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", conversation).Take(&c).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return accessDenied()
		}
		if err != nil {
			return err
		}
		var m model.Member
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("conversation_id = ? AND user_id = ?", conversation, user).Take(&m).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return accessDenied()
		}
		if err != nil {
			return err
		}
		if seq < 0 || seq > c.LastSeq {
			return &apperror.Error{Kind: apperror.Invalid, Message: "已读位置超过已保存消息范围"}
		}
		if seq > m.LastReadSeq {
			if err := tx.Model(&model.Member{}).Where("conversation_id = ? AND user_id = ?", conversation, user).Update("last_read_seq", seq).Error; err != nil {
				return err
			}
			m.LastReadSeq = seq
		}
		var unread int64
		if err := tx.Model(&model.Message{}).Where("conversation_id = ? AND seq > ? AND sender_id <> ?", conversation, m.LastReadSeq, user).Count(&unread).Error; err != nil {
			return err
		}
		result.State = ReadState{ConversationID: conversation, UserID: user, LastReadSeq: m.LastReadSeq, UnreadCount: unread, LastSeq: c.LastSeq}
		result.Participants = []int64{c.BuyerID, c.SellerID}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Sync 在只读可重复读快照中校验成员并捕获上界，后续页不得越过这个上界。
func (r *Repository) Sync(ctx context.Context, user, conversation, after, until int64, limit int) (*SyncPage, error) {
	result := &SyncPage{}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var m model.Member
		err := tx.Where("conversation_id = ? AND user_id = ?", conversation, user).Take(&m).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return accessDenied()
		}
		if err != nil {
			return err
		}
		var c model.Conversation
		if err := tx.Where("id = ?", conversation).Take(&c).Error; err != nil {
			return err
		}
		if until == 0 {
			until = c.LastSeq
		}
		if after < 0 || until < after || until > c.LastSeq {
			return &apperror.Error{Kind: apperror.Invalid, Message: "补拉位置无效"}
		}
		result.Until = until
		return tx.Where("conversation_id = ? AND seq > ? AND seq <= ?", conversation, after, until).Order("seq ASC").Limit(limit).Find(&result.Messages).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return result, nil
}
