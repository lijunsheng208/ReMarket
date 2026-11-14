package kafka

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

type Config struct {
	Brokers        []string
	ClientID       string
	ConnectTimeout time.Duration
}

// LoadConfig 从环境变量读取本地 Kafka 连接配置，不自动加载 .env 文件。
func LoadConfig() (Config, error) {
	c := Config{ClientID: os.Getenv("KAFKA_CLIENT_ID"), ConnectTimeout: 10 * time.Second}
	raw := os.Getenv("KAFKA_BROKERS")
	if raw == "" {
		raw = "127.0.0.1:9092"
	}
	for _, broker := range strings.Split(raw, ",") {
		broker = strings.TrimSpace(broker)
		if broker == "" {
			return Config{}, fmt.Errorf("KAFKA_BROKERS 包含空地址")
		}
		c.Brokers = append(c.Brokers, broker)
	}
	if c.ClientID == "" {
		c.ClientID = "remarket"
	}
	if raw := os.Getenv("KAFKA_CONNECT_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("KAFKA_CONNECT_TIMEOUT 必须为正时长")
		}
		c.ConnectTimeout = d
	}
	return c, nil
}

// NewClient 创建明文本地开发客户端；调用方负责设置操作超时并在退出时 Close。
func NewClient(c Config) (*kgo.Client, error) {
	if len(c.Brokers) == 0 || c.ConnectTimeout <= 0 {
		return nil, fmt.Errorf("Kafka 地址及连接超时不能为空")
	}
	// 保留 franz-go 默认幂等生产能力，单节点 ACK 仅代表本地 Broker 确认。
	return kgo.NewClient(kgo.SeedBrokers(c.Brokers...), kgo.ClientID(c.ClientID), kgo.DialTimeout(c.ConnectTimeout), kgo.RequiredAcks(kgo.AllISRAcks()))
}
