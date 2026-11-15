package repository

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"math"
	"remarket/backend/internal/chat/model"
	"remarket/backend/pkg/apperror"
	"time"
)

// SendResult 返回已提交消息及会话参与者，通知失败不影响持久化结果。
type SendResult struct {
	Message      model.Message
	Participants []int64
}

// sameMessage 核对幂等请求的完整参数，禁止相同键发送不同内容或会话。
func sameMessage(m model.Message, conversation int64, content string) error {
	if m.ConversationID != conversation || m.Content != content || m.MessageType != "TEXT" {
		return &apperror.Error{Kind: apperror.Conflict, Message: "消息幂等键已用于其他请求"}
	}
	return nil
}

// Send 锁定会话后分配序号，消息与最后消息元数据同事务提交。
func (r *Repository) Send(ctx context.Context, user, conversation int64, key, content string) (*SendResult, error) {
	var result SendResult
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var c model.Conversation
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", conversation).Take(&c).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &apperror.Error{Kind: apperror.Forbidden, Message: "无权访问会话"}
		}
		if err != nil {
			return err
		}
		var member model.Member
		err = tx.Where("conversation_id = ? AND user_id = ?", conversation, user).Take(&member).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &apperror.Error{Kind: apperror.Forbidden, Message: "无权访问会话"}
		}
		if err != nil {
			return err
		}
		result.Participants = []int64{c.BuyerID, c.SellerID}
		// 成员查询在会话锁取得后才建立快照，可看到前一持锁事务提交的消息。
		// 幂等键不加间隙锁，跨会话竞争交由唯一索引仲裁。
		err = tx.Where("sender_id = ? AND client_msg_id = ?", user, key).Take(&result.Message).Error
		if err == nil {
			return sameMessage(result.Message, conversation, content)
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if c.LastSeq == math.MaxInt64 {
			return &apperror.Error{Kind: apperror.FailedPrecondition, Message: "会话序号已达上限"}
		}
		result.Message = model.Message{ConversationID: conversation, SenderID: user, ClientMsgID: key, Seq: c.LastSeq + 1, Content: content, MessageType: "TEXT", CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		if err := tx.Create(&result.Message).Error; err != nil {
			return err
		}
		return tx.Model(&model.Conversation{}).Where("id = ?", conversation).Updates(map[string]interface{}{"last_seq": result.Message.Seq, "last_message_id": result.Message.ID, "last_message_at": result.Message.CreatedAt}).Error
	})
	// 不同会话复用同一键的并发请求由唯一索引仲裁；失败事务不消耗序号。
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		var saved model.Message
		if e := r.db.WithContext(ctx).Where("sender_id = ? AND client_msg_id = ?", user, key).Take(&saved).Error; e != nil {
			return nil, e
		}
		if e := sameMessage(saved, conversation, content); e != nil {
			return nil, e
		}
		result.Message = saved
		return &result, nil
	}
	if err != nil {
		return nil, err
	}
	return &result, nil
}
