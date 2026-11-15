// Package service 实现独立于 HTTP 和 gRPC 的聊天业务。
package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"google.golang.org/grpc/metadata"
	productv1 "remarket/backend/gen/product/v1"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/internal/chat/model"
	"remarket/backend/internal/chat/repository"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/auth"
	"time"
)

type Store interface {
	FindExisting(context.Context, int64, int64) (*model.Conversation, error)
	Create(context.Context, int64, int64, int64) (*model.Conversation, error)
	List(context.Context, int64, *repository.Position, int) ([]model.Conversation, error)
	Messages(context.Context, int64, int64, int64, int64, int) ([]model.Message, error)
}
type Service struct {
	store    Store
	users    userv1.UserServiceClient
	products productv1.ProductServiceClient
}

// New 注入业务存储及跨服务客户端，不访问外部业务库。
func New(store Store, users userv1.UserServiceClient, products productv1.ProductServiceClient) *Service {
	return &Service{store, users, products}
}

// session 为整个业务操作派生超时并实时核实当前账号状态。
func (s *Service) session(ctx context.Context) (context.Context, context.CancelFunc, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok {
		return ctx, cancel, 0, &apperror.Error{Kind: apperror.Unauthenticated, Message: "请先登录"}
	}
	incoming, _ := metadata.FromIncomingContext(ctx)
	outgoing, _ := metadata.FromOutgoingContext(ctx)
	outgoing = outgoing.Copy()
	if values := incoming.Get("authorization"); len(values) > 0 {
		outgoing.Set("authorization", values...)
	}
	ctx = metadata.NewOutgoingContext(ctx, outgoing)
	_, err := s.users.GetCurrentUser(ctx, &userv1.GetCurrentUserRequest{})
	return ctx, cancel, identity.UserID, err
}

// Create 复用既有会话；首次创建仅允许公开在售商品且禁止自聊。
func (s *Service) Create(ctx context.Context, product int64) (*model.Conversation, error) {
	if product <= 0 {
		return nil, invalid()
	}
	ctx, cancel, user, err := s.session(ctx)
	defer cancel()
	if err != nil {
		return nil, err
	}
	existing, err := s.store.FindExisting(ctx, user, product)
	if err != nil || existing != nil {
		return existing, err
	}
	result, err := s.products.GetProduct(ctx, &productv1.GetProductRequest{Id: product})
	if err != nil {
		return nil, err
	}
	p := result.GetProduct()
	if p == nil || p.Id != product || p.SellerId <= 0 {
		return nil, errors.New("商品 RPC 返回无效")
	}
	if p.SellerId == user {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "不能与自己创建会话"}
	}
	if p.Status != productv1.ProductStatus_PRODUCT_STATUS_ON_SALE {
		return nil, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "商品当前不可咨询"}
	}
	// Product 查询与本地创建不是分布式事务；查询后商品状态变化不撤销已创建会话。
	return s.store.Create(ctx, product, user, p.SellerId)
}

// invalid 返回可公开的统一分页参数错误。
func invalid() error { return &apperror.Error{Kind: apperror.Invalid, Message: "聊天参数无效"} }

// pageSize 统一分页上限，避免无界查询。
func pageSize(n int) (int, error) {
	if n < 0 || n > 100 {
		return 0, invalid()
	}
	if n == 0 {
		n = 20
	}
	return n, nil
}

// List 返回当前账号会话，游标仅限定排序位置，不授予权限。
func (s *Service) List(ctx context.Context, cursor string, size int) ([]model.Conversation, string, error) {
	size, err := pageSize(size)
	if err != nil {
		return nil, "", err
	}
	var p *repository.Position
	if cursor != "" {
		if len(cursor) > 256 {
			return nil, "", invalid()
		}
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil {
			return nil, "", invalid()
		}
		p = &repository.Position{}
		if json.Unmarshal(raw, p) != nil || p.ID <= 0 || p.At.IsZero() {
			return nil, "", invalid()
		}
	}
	ctx, cancel, user, err := s.session(ctx)
	defer cancel()
	if err != nil {
		return nil, "", err
	}
	rows, err := s.store.List(ctx, user, p, size+1)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > size {
		rows = rows[:size]
		last := rows[len(rows)-1]
		at := last.CreatedAt
		if last.LastMessageAt != nil {
			at = *last.LastMessageAt
		}
		raw, _ := json.Marshal(repository.Position{At: at, ID: last.ID})
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	return rows, next, nil
}

// Messages 校验历史与增量游标互斥，再由存储检查当前成员权限。
func (s *Service) Messages(ctx context.Context, id, before, after int64, size int) ([]model.Message, bool, error) {
	size, err := pageSize(size)
	if err != nil {
		return nil, false, err
	}
	if id <= 0 || before < 0 || after < 0 || (before > 0 && after > 0) {
		return nil, false, invalid()
	}
	ctx, cancel, user, err := s.session(ctx)
	defer cancel()
	if err != nil {
		return nil, false, err
	}
	rows, err := s.store.Messages(ctx, user, id, before, after, size+1)
	if err != nil {
		return nil, false, err
	}
	more := len(rows) > size
	if more {
		rows = rows[:size]
	}
	return rows, more, nil
}
