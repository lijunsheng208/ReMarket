package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"remarket/backend/internal/order/events"
	"remarket/backend/internal/order/model"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/auth"
	"strconv"
	"time"
)

type ProductSnapshot struct {
	ID, SellerID, PriceCent, Version int64
	OnSale                           bool
}
type ProductReader interface {
	GetForOrder(context.Context, int64) (*ProductSnapshot, error)
}
type CreationRepository interface {
	FindByKey(context.Context, int64, string) (*model.Order, error)
	CreatePending(context.Context, *model.Order, func(*model.Order) (*model.OrderOperation, *model.OutboxEvent, error)) (*model.Order, error)
}
type CreationService struct {
	orders   CreationRepository
	products ProductReader
}

// NewCreation 注入持久化和可信商品读取，本批只保存 PENDING 意图，预占由后续执行器完成。
func NewCreation(orders CreationRepository, products ProductReader) *CreationService {
	return &CreationService{orders: orders, products: products}
}

// Create 校验用户及幂等键，优先返回既有订单，再读取商品快照并原子保存待执行操作。
func (s *CreationService) Create(ctx context.Context, productID int64, key string) (*model.Order, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok {
		return nil, &apperror.Error{Kind: apperror.Unauthenticated, Message: "请先登录"}
	}
	if productID <= 0 || len(key) < 1 || len(key) > 128 {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "商品 ID 或幂等键无效"}
	}
	for _, b := range []byte(key) {
		if b < 33 || b > 126 {
			return nil, &apperror.Error{Kind: apperror.Invalid, Message: "幂等键仅允许无空格可打印 ASCII 字符"}
		}
	}
	// 摘要只绑定客户端业务参数，不含可变商品快照、Token 或请求编号。
	digest := sha256.Sum256([]byte("create-order:v1:product_id=" + strconv.FormatInt(productID, 10)))
	hash := fmt.Sprintf("%x", digest)
	existing, err := s.orders.FindByKey(ctx, identity.UserID, key)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.RequestHash != hash {
			return nil, &apperror.Error{Kind: apperror.Conflict, Message: "幂等键已用于不同请求"}
		}
		return existing, nil
	}
	snapshot, err := s.products.GetForOrder(ctx, productID)
	if err != nil {
		return nil, err
	}
	if snapshot == nil || snapshot.ID != productID || snapshot.SellerID <= 0 || snapshot.PriceCent <= 0 || snapshot.Version <= 0 {
		return nil, fmt.Errorf("商品快照无效")
	}
	if !snapshot.OnSale || snapshot.SellerID == identity.UserID {
		return nil, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "商品不可购买"}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	order := &model.Order{BuyerID: identity.UserID, SellerID: snapshot.SellerID, ProductID: productID, PriceCent: snapshot.PriceCent, ExpectedProductVersion: &snapshot.Version, Status: model.StatusPending, IdempotencyKey: key, RequestHash: hash, AggregateVersion: 1, ExpiresAt: now.Add(30 * time.Minute), CreatedAt: now, UpdatedAt: now}
	return s.orders.CreatePending(ctx, order, func(saved *model.Order) (*model.OrderOperation, *model.OutboxEvent, error) {
		operationID, err := events.NewID()
		if err != nil {
			return nil, nil, err
		}
		event, err := events.NewCreated(*saved, now)
		if err != nil {
			return nil, nil, err
		}
		return &model.OrderOperation{OperationID: operationID, OrderID: saved.ID, OperationType: model.OperationReserve, Status: model.OperationPending, NextRetryAt: now, CreatedAt: now, UpdatedAt: now}, event, nil
	})
}
