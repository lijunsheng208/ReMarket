// Package model 定义 Order 服务独立拥有的数据结构，不执行迁移或状态变更。
package model

import (
	"encoding/json"
	"time"
)

type OrderStatus string

const (
	StatusPending    OrderStatus = "PENDING"
	StatusReserved   OrderStatus = "RESERVED"
	StatusConfirming OrderStatus = "CONFIRMING"
	StatusCancelling OrderStatus = "CANCELLING"
	StatusExpiring   OrderStatus = "EXPIRING"
	StatusCompleted  OrderStatus = "COMPLETED"
	StatusCancelled  OrderStatus = "CANCELLED"
	StatusExpired    OrderStatus = "EXPIRED"
	StatusFailed     OrderStatus = "FAILED"
)

type OperationType string

const (
	OperationReserve OperationType = "RESERVE"
	OperationRelease OperationType = "RELEASE"
	OperationConfirm OperationType = "CONFIRM"
)

type OperationStatus string

const (
	OperationPending    OperationStatus = "PENDING"
	OperationProcessing OperationStatus = "PROCESSING"
	OperationRetry      OperationStatus = "RETRY"
	OperationSucceeded  OperationStatus = "SUCCEEDED"
	OperationFailed     OperationStatus = "FAILED"
)

// Order 对应 orders；外部实体仅保存 ID，价格为可信商品快照。
type Order struct {
	ID                     int64 `gorm:"primaryKey;autoIncrement"`
	BuyerID                int64
	SellerID               int64
	ProductID              int64
	ExpectedProductVersion *int64
	PriceCent              int64
	Status                 OrderStatus
	IdempotencyKey         string
	RequestHash            string
	BuyerConfirmed         bool
	SellerConfirmed        bool
	AggregateVersion       int64
	ExpiresAt              time.Time
	ClosedAt               *time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// OrderOperation 对应 order_operations；可空字段区分尚未领取和已完成。
type OrderOperation struct {
	ID            int64 `gorm:"primaryKey;autoIncrement"`
	OperationID   string
	OrderID       int64
	OperationType OperationType
	Status        OperationStatus
	RetryCount    int32
	NextRetryAt   time.Time
	LockedUntil   *time.Time
	LockToken     *string
	LastError     *string
	FinishedAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type PublishStatus string

const (
	PublishPending    PublishStatus = "PENDING"
	PublishProcessing PublishStatus = "PROCESSING"
	PublishPublished  PublishStatus = "PUBLISHED"
	PublishFailed     PublishStatus = "FAILED"
)

// OutboxEvent 对应 outbox_events；Payload 仅存事件 data，不重复保存信封字段。
type OutboxEvent struct {
	ID               int64 `gorm:"primaryKey;autoIncrement"`
	EventID          string
	EventType        string
	AggregateType    string
	AggregateID      int64
	AggregateVersion int64
	SchemaVersion    int32
	Payload          json.RawMessage `gorm:"type:json"`
	OccurredAt       time.Time
	PublishStatus    PublishStatus
	Attempts         int32
	NextAttemptAt    time.Time
	LockedUntil      *time.Time
	LockToken        *string
	PublishedAt      *time.Time
	LastError        *string
	CreatedAt        time.Time
}

// TableName 明确订单表名。
func (Order) TableName() string { return "orders" }

// TableName 明确跨服务操作表名。
func (OrderOperation) TableName() string { return "order_operations" }

// TableName 明确 Order 独立的 Outbox 表名。
func (OutboxEvent) TableName() string { return "outbox_events" }
