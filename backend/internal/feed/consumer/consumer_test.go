package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"
	productv1 "remarket/backend/gen/product/v1"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/pkg/kafka"
	"strings"
	"testing"
	"time"
)

type userFake struct {
	userv1.UserServiceClient
	total int64
	token string
}

// ListFeedFollowers 断言内部服务凭据，并提供粉丝数据。
func (u *userFake) ListFeedFollowers(ctx context.Context, r *userv1.ListFeedFollowersRequest, _ ...grpc.CallOption) (*userv1.ListFeedFollowersResponse, error) {
	md, _ := metadata.FromOutgoingContext(ctx)
	if len(md.Get("authorization")) != 1 || md.Get("authorization")[0] != "Bearer "+u.token {
		return nil, fmt.Errorf("未传递独立服务凭据")
	}
	return &userv1.ListFeedFollowersResponse{UserIds: []int64{11, 12}, Total: u.total}, nil
}

type productFake struct {
	productv1.ProductServiceClient
	p *productv1.Product
}

// GetProduct 模拟 Product 的公开当前状态查询。
func (p *productFake) GetProduct(context.Context, *productv1.GetProductRequest, ...grpc.CallOption) (*productv1.ProductResponse, error) {
	return &productv1.ProductResponse{Product: p.p}, nil
}

type indexFake struct {
	hot       bool
	followers []int64
	err       error
	calls     int
}

// Apply 捕获分流及失败传播，不伪造异步写入完成。
func (i *indexFake) Apply(_ context.Context, _ *productv1.Product, _ int64, _ bool, followers []int64, hot bool) error {
	i.calls++
	i.hot = hot
	i.followers = followers
	return i.err
}

type senderFake struct {
	calls int
	err   error
}

// PublishRaw 模拟 DLT ACK，失败必须阻止原 Offset 提交。
func (s *senderFake) PublishRaw(context.Context, string, []byte, []byte) (kafka.Receipt, error) {
	s.calls++
	return kafka.Receipt{}, s.err
}

// TestConsumer 验证普通与热门分流、Redis 失败传播、毒消息 ACK 与取消处理。
func TestConsumer(t *testing.T) {
	now := time.Now().UTC()
	token := strings.Repeat("f", 32)
	users := &userFake{total: 2, token: token}
	index := &indexFake{}
	sender := &senderFake{}
	p := &productv1.Product{Id: 1, SellerId: 22, CreatedAt: timestamppb.New(now), Status: productv1.ProductStatus_PRODUCT_STATUS_ON_SALE}
	c, err := New(index, users, &productFake{p: p}, sender, token, "feed.dlt", 3)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"product_id": "1", "seller_id": "22", "status": "ON_SALE", "created_at": now, "first_published_at": now})
	raw, _ := json.Marshal(kafka.Envelope{EventID: "feed-test", EventType: "product.published.v1", AggregateType: "product", AggregateID: 1, AggregateVersion: 2, SchemaVersion: 1, OccurredAt: now, Data: data})
	m := kafka.Message{Key: []byte("1"), Value: raw}
	if err := c.Handle(context.Background(), m); err != nil || index.hot || len(index.followers) != 2 {
		t.Fatalf("普通分流失败 %v", err)
	}
	users.total = 3
	if err := c.Handle(context.Background(), m); err != nil || !index.hot || len(index.followers) != 0 {
		t.Fatalf("热门分流失败 %v", err)
	}
	index.err = fmt.Errorf("redis down")
	if err := c.Handle(context.Background(), m); err == nil {
		t.Fatal("索引失败被吞掉")
	}
	if sender.calls != 0 {
		t.Fatal("临时故障不能转移并提交")
	}
	sender.err = fmt.Errorf("broker down")
	if err := c.Handle(context.Background(), kafka.Message{Value: []byte("bad")}); err == nil {
		t.Fatal("DLT ACK 失败被吞掉")
	}
	sender.err = nil
	if err := c.Handle(context.Background(), kafka.Message{Value: []byte("bad")}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Handle(ctx, m); err == nil {
		t.Fatal("取消后继续消费")
	}
}
