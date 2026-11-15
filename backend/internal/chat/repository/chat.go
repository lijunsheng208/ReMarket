// Package repository 只访问 Chat 自有数据库。
package repository

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"remarket/backend/internal/chat/model"
	"remarket/backend/pkg/apperror"
	"time"
)

type Repository struct{ db *gorm.DB }

// conversationFields 在同一次快照查询内计算当前成员的未读数，排除本人发送的消息。
const conversationFields = "conversations.*, m.last_read_seq, COALESCE(lm.content, '') AS last_message_content, (SELECT COUNT(*) FROM chat_messages cm WHERE cm.conversation_id = conversations.id AND cm.seq > m.last_read_seq AND cm.sender_id <> m.user_id) AS unread_count"

type Position struct {
	At time.Time
	ID int64
}

// New 注入 Chat 数据库，连接生命周期由入口负责。
func New(db *gorm.DB) *Repository { return &Repository{db: db} }

// FindExisting 按买家和商品复用既有会话，不依赖商品当前可见性。
func (r *Repository) FindExisting(ctx context.Context, buyer, product int64) (*model.Conversation, error) {
	var c model.Conversation
	err := r.db.WithContext(ctx).Model(&model.Conversation{}).Select(conversationFields).Joins("LEFT JOIN chat_messages lm ON lm.id = conversations.last_message_id").Joins("JOIN conversation_members m ON m.conversation_id = conversations.id AND m.user_id = ?", buyer).Where("buyer_id = ? AND product_id = ?", buyer, product).Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &c, err
}

// Create 在同一事务内创建会话及两个成员；唯一键仲裁并发复用。
func (r *Repository) Create(ctx context.Context, product, buyer, seller int64) (*model.Conversation, error) {
	c := model.Conversation{ProductID: product, BuyerID: buyer, SellerID: seller}
	// 同键冲突采用无操作更新，不先查询再插入，避免并发创建窗口。
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "product_id"}, {Name: "buyer_id"}, {Name: "seller_id"}}, DoUpdates: clause.Assignments(map[string]interface{}{"id": gorm.Expr("id")})}).Create(&c).Error; err != nil {
			return err
		}
		if err := tx.Where("product_id = ? AND buyer_id = ? AND seller_id = ?", product, buyer, seller).Take(&c).Error; err != nil {
			return err
		}
		members := []model.Member{{ConversationID: c.ID, UserID: buyer}, {ConversationID: c.ID, UserID: seller}}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&members).Error
	})
	if err != nil {
		return nil, err
	}
	return r.FindExisting(ctx, buyer, product)
}

// List 通过成员表限定权限，以活动时间和 ID 做排他游标分页。
func (r *Repository) List(ctx context.Context, user int64, p *Position, limit int) ([]model.Conversation, error) {
	var rows []model.Conversation
	q := r.db.WithContext(ctx).Model(&model.Conversation{}).Select(conversationFields).
		Joins("LEFT JOIN chat_messages lm ON lm.id = conversations.last_message_id").
		Joins("JOIN conversation_members m ON m.conversation_id = conversations.id AND m.user_id = ?", user)
	if p != nil {
		q = q.Where("(COALESCE(last_message_at, conversations.created_at) < ?) OR (COALESCE(last_message_at, conversations.created_at) = ? AND conversations.id < ?)", p.At, p.At, p.ID)
	}
	err := q.Order("COALESCE(last_message_at, conversations.created_at) DESC").Order("conversations.id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

// Messages 先核实成员再查询消息，非成员与不存在会话统一拒绝。
func (r *Repository) Messages(ctx context.Context, user, conversation, before, after int64, limit int) ([]model.Message, error) {
	var member model.Member
	err := r.db.WithContext(ctx).Where("conversation_id = ? AND user_id = ?", conversation, user).Take(&member).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, &apperror.Error{Kind: apperror.Forbidden, Message: "无权访问会话"}
	}
	if err != nil {
		return nil, err
	}
	var rows []model.Message
	q := r.db.WithContext(ctx).Where("conversation_id = ?", conversation)
	if after > 0 {
		q = q.Where("seq > ?", after).Order("seq ASC")
	} else {
		if before > 0 {
			q = q.Where("seq < ?", before)
		}
		q = q.Order("seq DESC")
	}
	err = q.Limit(limit).Find(&rows).Error
	return rows, err
}
