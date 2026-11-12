// Package events 构造 Order 自有事件，不执行消息投递。
package events

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"remarket/backend/internal/order/model"
	"time"
)

// NewID 生成 UUID v4，逻辑操作重试必须复用持久化 ID。
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// NewCreated 构造订单创建快照，金额与 ID 按字符串编码，不含幂等键或请求摘要。
func NewCreated(order model.Order, now time.Time) (*model.OutboxEvent, error) {
	data := struct {
		OrderID         int64             `json:"order_id,string"`
		BuyerID         int64             `json:"buyer_id,string"`
		SellerID        int64             `json:"seller_id,string"`
		ProductID       int64             `json:"product_id,string"`
		PriceCent       int64             `json:"price_cent,string"`
		Status          model.OrderStatus `json:"status"`
		ExpiresAt       time.Time         `json:"expires_at"`
		BuyerConfirmed  bool              `json:"buyer_confirmed"`
		SellerConfirmed bool              `json:"seller_confirmed"`
	}{order.ID, order.BuyerID, order.SellerID, order.ProductID, order.PriceCent, order.Status, order.ExpiresAt, order.BuyerConfirmed, order.SellerConfirmed}
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	return &model.OutboxEvent{EventID: id, EventType: "order.created.v1", AggregateType: "order", AggregateID: order.ID, AggregateVersion: order.AggregateVersion, SchemaVersion: 1, Payload: payload, OccurredAt: now, PublishStatus: model.PublishPending, NextAttemptAt: now, CreatedAt: now}, nil
}

// NewStateChanged 构造订单状态变更事件，不改变订单和操作状态。
func NewStateChanged(order model.Order, now time.Time) (*model.OutboxEvent, error) {
	event, err := NewCreated(order, now)
	if err != nil {
		return nil, err
	}
	switch order.Status {
	case model.StatusReserved:
		event.EventType = "order.reserved.v1"
	case model.StatusConfirming:
		event.EventType = "order.confirming.v1"
	case model.StatusCancelling:
		event.EventType = "order.cancelling.v1"
	case model.StatusExpiring:
		event.EventType = "order.expiring.v1"
	case model.StatusCompleted:
		event.EventType = "order.completed.v1"
	case model.StatusCancelled:
		event.EventType = "order.cancelled.v1"
	case model.StatusExpired:
		event.EventType = "order.expired.v1"
	case model.StatusFailed:
		event.EventType = "order.failed.v1"
	default:
		return nil, fmt.Errorf("不支持的订单事件状态")
	}
	return event, nil
}
