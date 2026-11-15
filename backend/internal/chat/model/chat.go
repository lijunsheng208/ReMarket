// Package model 定义 Chat 自有数据模型，不自动执行迁移。
package model

import "time"

// Conversation 保存商品咨询双方及事务内分配的最后序号。
type Conversation struct {
	// 查询派生字段，只读，不写入会话表；用户身份由成员关联决定。
	LastReadSeq        int64  `gorm:"->;-:migration"`
	UnreadCount        int64  `gorm:"->;-:migration"`
	LastMessageContent string `gorm:"->;-:migration"`
	ID                 int64  `gorm:"primaryKey;autoIncrement"`
	ProductID          int64
	BuyerID            int64
	SellerID           int64
	LastSeq            int64
	LastMessageID      *int64
	LastMessageAt      *time.Time
	CreatedAt          time.Time
}

// Message 保存不可变文本消息；客户端幂等键按发送者全局唯一。
type Message struct {
	ID             int64 `gorm:"primaryKey;autoIncrement"`
	ConversationID int64
	SenderID       int64
	ClientMsgID    string
	Seq            int64
	Content        string
	MessageType    string
	CreatedAt      time.Time
}

// Member 保存会话权限和单调递增的已读游标。
type Member struct {
	ConversationID int64 `gorm:"primaryKey;autoIncrement:false"`
	UserID         int64 `gorm:"primaryKey;autoIncrement:false"`
	LastReadSeq    int64
	CreatedAt      time.Time
}

// TableName 指定会话表名。
func (Conversation) TableName() string { return "conversations" }

// TableName 指定消息表名。
func (Message) TableName() string { return "chat_messages" }

// TableName 指定成员表名。
func (Member) TableName() string { return "conversation_members" }
