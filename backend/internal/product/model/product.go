// Package model 定义 Product 服务拥有的数据结构，不包含状态变更或数据库访问逻辑。
package model

import (
	"encoding/json"
	"time"
)

type ProductStatus string

const (
	StatusDraft    ProductStatus = "DRAFT"
	StatusOnSale   ProductStatus = "ON_SALE"
	StatusReserved ProductStatus = "RESERVED"
	StatusSold     ProductStatus = "SOLD"
	StatusOffShelf ProductStatus = "OFF_SHELF"
)

type ReservationStatus string

const (
	ReservationActive    ReservationStatus = "ACTIVE"
	ReservationConfirmed ReservationStatus = "CONFIRMED"
	ReservationReleased  ReservationStatus = "RELEASED"
)

type PublishStatus string

const (
	PublishPending    PublishStatus = "PENDING"
	PublishProcessing PublishStatus = "PROCESSING"
	PublishPublished  PublishStatus = "PUBLISHED"
	PublishFailed     PublishStatus = "FAILED"
)

// Product 对应 products；首次发布时间可空，状态变化由后续 Service 统一负责。
type Product struct {
	ID               int64 `gorm:"primaryKey;autoIncrement"`
	SellerID         int64
	CategoryID       int64
	Title            string
	Description      string
	PriceCent        int64
	Status           ProductStatus
	AggregateVersion int64
	FirstPublishedAt *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Category 对应 categories，禁用分类保留历史引用。
type Category struct {
	ID        int64 `gorm:"primaryKey;autoIncrement"`
	Code      string
	Name      string
	SortOrder int32
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ProductImage 对应 product_images，仅保存对象键，最小排序位置为封面。
type ProductImage struct {
	ID        int64 `gorm:"primaryKey;autoIncrement"`
	ProductID int64
	ObjectKey string
	SortOrder int32
	CreatedAt time.Time
}

// ProductReservation 对应 product_reservations，生成列只读，不由 GORM 插入或更新。
type ProductReservation struct {
	ID              int64 `gorm:"primaryKey;autoIncrement"`
	ProductID       int64
	OrderID         int64
	BuyerID         int64
	Status          ReservationStatus
	ExpiresAt       time.Time
	ConfirmedAt     *time.Time
	ReleasedAt      *time.Time
	ReleaseReason   *string
	ActiveProductID *int64 `gorm:"->"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

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

// TableName 明确商品表名，避免依赖自动命名规则。
func (Product) TableName() string { return "products" }

// TableName 明确分类表名。
func (Category) TableName() string { return "categories" }

// TableName 明确商品图片表名。
func (ProductImage) TableName() string { return "product_images" }

// TableName 明确商品预占表名。
func (ProductReservation) TableName() string { return "product_reservations" }

// TableName 明确 Outbox 表名。
func (OutboxEvent) TableName() string { return "outbox_events" }
