package kafka

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Envelope 使用现有商品契约的 data 字段，业务载荷仍由各服务定义。
type Envelope struct {
	EventID          string          `json:"event_id"`
	EventType        string          `json:"event_type"`
	AggregateType    string          `json:"aggregate_type"`
	AggregateID      int64           `json:"aggregate_id,string"`
	AggregateVersion int64           `json:"aggregate_version,string"`
	SchemaVersion    int32           `json:"schema_version"`
	OccurredAt       time.Time       `json:"occurred_at"`
	Data             json.RawMessage `json:"data"`
}

var eventIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,128}$`)

var eventTypePattern = regexp.MustCompile(`^(product|order)\.[a-z_]+\.v1$`)

// Validate 校验信封，不验证业务载荷字段，不接受未知信封版本。
func (e Envelope) Validate() error {
	if !eventIDPattern.MatchString(e.EventID) || !eventTypePattern.MatchString(e.EventType) || !strings.HasPrefix(e.EventType, e.AggregateType+".") || (e.AggregateType != "product" && e.AggregateType != "order") || e.AggregateID <= 0 || e.AggregateVersion <= 0 || e.SchemaVersion != 1 || e.OccurredAt.IsZero() {
		return fmt.Errorf("事件信封无效")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(e.Data, &object) != nil || object == nil {
		return fmt.Errorf("事件 data 必须为 JSON 对象")
	}
	return nil
}

// DecodeEnvelope 解码并校验公共元信息，未知业务字段留给处理器验证。
func DecodeEnvelope(data []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return e, fmt.Errorf("事件 JSON 无效")
	}
	return e, e.Validate()
}

// Topics 配置商品及订单主事件、重试、死信 Topic；不会自动建 Topic。
type Topics struct{ Product, Order, ProductRetry, OrderRetry, ProductDLT, OrderDLT string }

// DefaultTopics 与 Compose 初始化脚本保持一致。
func DefaultTopics() Topics {
	return Topics{"remarket.product.events.v1", "remarket.order.events.v1", "remarket.product.events.retry.v1", "remarket.order.events.retry.v1", "remarket.product.events.dlt.v1", "remarket.order.events.dlt.v1"}
}

var topicPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,249}$`)

// Validate 拒绝非法或重复 Topic，防止误将主事件写入重试链路。
func (t Topics) Validate() error {
	seen := map[string]bool{}
	for _, name := range []string{t.Product, t.Order, t.ProductRetry, t.OrderRetry, t.ProductDLT, t.OrderDLT} {
		if !topicPattern.MatchString(name) || name == "." || name == ".." || seen[name] {
			return fmt.Errorf("Kafka Topic 配置无效")
		}
		seen[name] = true
	}
	return nil
}

// EventTopic 只选择主事件 Topic，重试与死信可靠转移留给后续模块。
func (t Topics) EventTopic(aggregate string) (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}
	switch aggregate {
	case "product":
		return t.Product, nil
	case "order":
		return t.Order, nil
	default:
		return "", fmt.Errorf("事件聚合类型无效")
	}
}

// LoadTopics 允许部署覆盖 Topic 名称，默认与本地 Compose 一致。
func LoadTopics() (Topics, error) {
	t := DefaultTopics()
	for key, target := range map[string]*string{"KAFKA_PRODUCT_TOPIC": &t.Product, "KAFKA_ORDER_TOPIC": &t.Order, "KAFKA_PRODUCT_RETRY_TOPIC": &t.ProductRetry, "KAFKA_ORDER_RETRY_TOPIC": &t.OrderRetry, "KAFKA_PRODUCT_DLT_TOPIC": &t.ProductDLT, "KAFKA_ORDER_DLT_TOPIC": &t.OrderDLT} {
		if value := os.Getenv(key); value != "" {
			*target = value
		}
	}
	return t, t.Validate()
}
