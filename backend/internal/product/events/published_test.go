package events

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"remarket/backend/internal/product/model"
	"strings"
	"testing"
	"time"
)

// publishedProduct 构造发布后快照，包含超过 JavaScript 安全整数范围的 ID。
func publishedProduct() model.Product {
	now := time.Date(2026, 11, 12, 16, 0, 0, 123456789, time.FixedZone("CST", 8*3600))
	return model.Product{ID: 9007199254740993, SellerID: 7, CategoryID: 1, Title: "机械键盘", Description: "正常使用", PriceCent: 15000, Status: model.StatusOnSale, AggregateVersion: 2, FirstPublishedAt: &now}
}

// TestPublishedOutbox 验证事件编号、快照字符串精度、UTC 与 Outbox 初始发布状态。
func TestPublishedOutbox(t *testing.T) {
	product := publishedProduct()
	outbox, err := NewPublishedOutbox(product, *product.FirstPublishedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(outbox.EventID) {
		t.Fatal("UUID 不合法")
	}
	if outbox.PublishStatus != model.PublishPending || outbox.EventType != PublishedV1 || outbox.AggregateID != product.ID || outbox.AggregateVersion != 2 || outbox.SchemaVersion != 1 || outbox.Attempts != 0 || outbox.LockedUntil != nil || outbox.PublishedAt != nil {
		t.Fatal("初始字段不正确")
	}
	var payload map[string]any
	if err := json.Unmarshal(outbox.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["product_id"] != "9007199254740993" || payload["price_cent"] != "15000" || payload["event_id"] != nil {
		t.Fatal("快照精度或边界错误")
	}
	if payload["first_published_at"] != "2026-11-12T08:00:00.123456Z" || outbox.OccurredAt.Location() != time.UTC {
		t.Fatal("时间未对齐")
	}
	another, err := NewPublishedOutbox(product, *product.FirstPublishedAt)
	if err != nil {
		t.Fatal(err)
	}
	if another.EventID == outbox.EventID {
		t.Fatal("事件 ID 重复")
	}
}

// TestPublishedValidation 验证无效快照无法构造首次发布事件。
func TestPublishedValidation(t *testing.T) {
	for _, name := range []string{"id", "price", "version", "draft", "time", "title", "description"} {
		t.Run(name, func(t *testing.T) {
			product := publishedProduct()
			occurred := *product.FirstPublishedAt
			switch name {
			case "id":
				product.ID = 0
			case "price":
				product.PriceCent = 0
			case "version":
				product.AggregateVersion = 1
			case "draft":
				product.Status = model.StatusDraft
			case "time":
				product.FirstPublishedAt = nil
			case "title":
				product.Title = " "
			case "description":
				product.Description = strings.Repeat("x", 16385)
			}
			if _, err := NewPublishedOutbox(product, occurred); !errors.Is(err, ErrInvalidPublishedProduct) {
				t.Fatal("无效快照被接受")
			}
		})
	}
}

// TestPublishedContractExample 验证版本化示例可被 Go 契约无损解析并与信封保持一致。
func TestPublishedContractExample(t *testing.T) {
	raw, err := os.ReadFile("../../../../api/events/product/product.published.v1.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope PublishedEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.EventType != PublishedV1 || envelope.SchemaVersion != 1 || envelope.AggregateID != envelope.Data.ProductID || envelope.AggregateVersion < 2 {
		t.Fatal("契约示例不一致")
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var original, roundtrip any
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &roundtrip); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(original)
	b, _ := json.Marshal(roundtrip)
	if string(a) != string(b) {
		t.Fatal("示例序列化丢失字段")
	}
}
