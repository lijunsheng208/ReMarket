// Package events 定义商品事件契约与构造，不执行业务状态转换或消息投递。
package events

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"remarket/backend/internal/product/model"
	"strings"
	"time"
	"unicode/utf8"
)

const PublishedV1 = "product.published.v1"

var ErrInvalidPublishedProduct = errors.New("首次发布商品快照无效")

// PublishedData 是首次上架快照；大整数通过 JSON 字符串避免消费者精度丢失。
type PublishedData struct {
	ProductID        int64               `json:"product_id,string"`
	SellerID         int64               `json:"seller_id,string"`
	CategoryID       int64               `json:"category_id,string"`
	Title            string              `json:"title"`
	Description      string              `json:"description"`
	PriceCent        int64               `json:"price_cent,string"`
	Status           model.ProductStatus `json:"status"`
	FirstPublishedAt time.Time           `json:"first_published_at"`
}

// PublishedEnvelope 定义消息总线中的完整事件；发布器从 Outbox 元信息及 Payload 组装。
type PublishedEnvelope struct {
	EventID          string        `json:"event_id"`
	EventType        string        `json:"event_type"`
	AggregateType    string        `json:"aggregate_type"`
	AggregateID      int64         `json:"aggregate_id,string"`
	AggregateVersion int64         `json:"aggregate_version,string"`
	SchemaVersion    int32         `json:"schema_version"`
	OccurredAt       time.Time     `json:"occurred_at"`
	Data             PublishedData `json:"data"`
}

// NewPublishedOutbox 从发布后的商品构造待插入 Outbox 记录，不修改商品、不落库。
// 是否首次发布必须由 Service 根据更新前状态判定，此函数无法从快照证明历史转换。
func NewPublishedOutbox(product model.Product, occurredAt time.Time) (*model.OutboxEvent, error) {
	return newVisibleOutbox(product, occurredAt, PublishedV1)
}

const UpdatedV1 = "product.updated.v1"
const OffShelfV1 = "product.off_shelf.v1"

// NewUpdatedOutbox 构造在售编辑或重新上架快照，保留原首次发布时间。
func NewUpdatedOutbox(product model.Product, occurredAt time.Time) (*model.OutboxEvent, error) {
	return newVisibleOutbox(product, occurredAt, UpdatedV1)
}

// NewOffShelfOutbox 构造下架快照，供搜索及 Feed 移除可见视图。
func NewOffShelfOutbox(product model.Product, occurredAt time.Time) (*model.OutboxEvent, error) {
	return newVisibleOutbox(product, occurredAt, OffShelfV1)
}

// newVisibleOutbox 统一生成公开视图事件，状态必须与事件类型一致。
func newVisibleOutbox(product model.Product, occurredAt time.Time, eventType string) (*model.OutboxEvent, error) {
	if product.ID <= 0 || product.SellerID <= 0 || product.CategoryID <= 0 || product.PriceCent <= 0 || product.AggregateVersion < 2 || ((eventType != OffShelfV1 && product.Status != model.StatusOnSale) || (eventType == OffShelfV1 && product.Status != model.StatusOffShelf)) || product.FirstPublishedAt == nil || product.FirstPublishedAt.IsZero() || occurredAt.IsZero() || product.FirstPublishedAt.After(occurredAt) || !utf8.ValidString(product.Title) || strings.TrimSpace(product.Title) == "" || utf8.RuneCountInString(product.Title) > 128 || !utf8.ValidString(product.Description) || len(product.Description) > 16<<10 {
		return nil, ErrInvalidPublishedProduct
	}
	// 统一 UTC 微秒精度，与数据库 DATETIME(6) 对齐。
	occurredAt = occurredAt.UTC().Truncate(time.Microsecond)
	data := PublishedData{ProductID: product.ID, SellerID: product.SellerID, CategoryID: product.CategoryID, Title: product.Title, Description: product.Description, PriceCent: product.PriceCent, Status: product.Status, FirstPublishedAt: product.FirstPublishedAt.UTC().Truncate(time.Microsecond)}
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	// UUID v4 标识逻辑事件；投递重试必须复用已持久化的 EventID，不能重新构造。
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return nil, err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
	return &model.OutboxEvent{EventID: id, EventType: eventType, AggregateType: "product", AggregateID: product.ID, AggregateVersion: product.AggregateVersion, SchemaVersion: 1, Payload: payload, OccurredAt: occurredAt, PublishStatus: model.PublishPending, NextAttemptAt: occurredAt, CreatedAt: occurredAt}, nil
}
