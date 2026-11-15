// Package consumer 将商品事件写入 Feed 可重建时间线，不直接访问 User 或 Product 数据库。
package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	productv1 "remarket/backend/gen/product/v1"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/pkg/kafka"
	"strconv"
	"strings"
	"time"
)

type Index interface {
	Apply(context.Context, *productv1.Product, int64, bool, []int64, bool) error
}
type Sender interface {
	PublishRaw(context.Context, string, []byte, []byte) (kafka.Receipt, error)
}
type Consumer struct {
	index      Index
	users      userv1.UserServiceClient
	products   productv1.ProductServiceClient
	sender     Sender
	token, dlt string
	threshold  int64
}

// New 校验粉丝查询服务凭据与热门阈值，失败由启动层拒绝运行。
func New(index Index, users userv1.UserServiceClient, products productv1.ProductServiceClient, sender Sender, token, dlt string, threshold int64) (*Consumer, error) {
	if index == nil || users == nil || products == nil || sender == nil || len(token) < 32 || strings.ContainsAny(token, " \t\r\n") || dlt == "" || threshold < 1 || threshold > 10000 {
		return nil, fmt.Errorf("Feed 消费配置无效")
	}
	return &Consumer{index, users, products, sender, token, dlt, threshold}, nil
}

// reject 先收到 DLT Broker ACK 才允许原消息提交，不记录原始业务载荷。
func (c *Consumer) reject(ctx context.Context, m kafka.Message) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = c.sender.PublishRaw(ctx, c.dlt, m.Key, raw)
	return err
}

// Handle 对合法事件至少一次处理；Redis、RPC 或 ACK 失败不提交，由入口重建消费者重试。
func (c *Consumer) Handle(ctx context.Context, m kafka.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(m.Value) > 256<<10 {
		return c.reject(ctx, m)
	}
	e, err := kafka.DecodeEnvelope(m.Value)
	if err != nil {
		return c.reject(ctx, m)
	}
	states := map[string]string{"product.published.v1": "ON_SALE", "product.updated.v1": "ON_SALE", "product.released.v1": "ON_SALE", "product.off_shelf.v1": "OFF_SHELF", "product.reserved.v1": "RESERVED", "product.sold.v1": "SOLD"}
	var data struct {
		ProductID        int64     `json:"product_id,string"`
		SellerID         int64     `json:"seller_id,string"`
		Status           string    `json:"status"`
		CreatedAt        time.Time `json:"created_at"`
		FirstPublishedAt time.Time `json:"first_published_at"`
	}
	if e.AggregateType != "product" || states[e.EventType] == "" || string(m.Key) != strconv.FormatInt(e.AggregateID, 10) || json.Unmarshal(e.Data, &data) != nil || data.ProductID != e.AggregateID || data.SellerID <= 0 || data.Status != states[e.EventType] || data.FirstPublishedAt.IsZero() || data.FirstPublishedAt.After(e.OccurredAt) {
		return c.reject(ctx, m)
	}
	p := &productv1.Product{Id: data.ProductID, SellerId: data.SellerID, CreatedAt: timestamppb.New(data.CreatedAt)}
	visible := data.Status == "ON_SALE"
	// 兼容历史事件缺少创建时间；可见事件同时核实当前公开状态，过期事件不能复活已下架商品。
	if visible || data.CreatedAt.IsZero() {
		current, readErr := c.products.GetProduct(ctx, &productv1.GetProductRequest{Id: p.Id})
		if readErr != nil {
			if status.Code(readErr) != codes.NotFound {
				return readErr
			}
			visible = false
			if data.CreatedAt.IsZero() {
				p.CreatedAt = timestamppb.New(data.FirstPublishedAt)
			}
		} else {
			p = current.Product
			visible = visible && p.Status == productv1.ProductStatus_PRODUCT_STATUS_ON_SALE
			if p.SellerId != data.SellerID {
				return c.reject(ctx, m)
			}
		}
	}
	if p.CreatedAt == nil || !p.CreatedAt.IsValid() {
		return c.reject(ctx, m)
	}
	// 独立服务令牌只用于内部粉丝 RPC，不能覆盖 Product 的公开查询凭据。
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set("authorization", "Bearer "+c.token)
	userCtx := metadata.NewOutgoingContext(ctx, md)
	page, err := c.users.ListFeedFollowers(userCtx, &userv1.ListFeedFollowersRequest{SellerId: p.SellerId, PageSize: 500})
	if err != nil {
		return err
	}
	hot := page.Total >= c.threshold
	followers := []int64{}
	if !hot {
		followers = append(followers, page.UserIds...)
		after := int64(0)
		for page.NextAfterId != 0 {
			if page.NextAfterId <= after {
				return fmt.Errorf("Feed 粉丝游标无效")
			}
			after = page.NextAfterId
			page, err = c.users.ListFeedFollowers(userCtx, &userv1.ListFeedFollowersRequest{SellerId: p.SellerId, AfterId: after, PageSize: 500})
			if err != nil {
				return err
			}
			followers = append(followers, page.UserIds...)
			if int64(len(followers)) >= c.threshold {
				hot = true
				followers = nil
				break
			}
		}
	}
	return c.index.Apply(ctx, p, e.AggregateVersion, visible, followers, hot)
}
