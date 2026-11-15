package service

import (
	"context"
	"errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	productv1 "remarket/backend/gen/product/v1"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/internal/chat/model"
	"remarket/backend/pkg/auth"
	"strings"
	"testing"
	"time"
)

type fakeUsers struct {
	userv1.UserServiceClient
	err error
	t   *testing.T
}

// GetCurrentUser 检查原始凭据继续传递，模拟实时账号核实。
func (f fakeUsers) GetCurrentUser(ctx context.Context, r *userv1.GetCurrentUserRequest, _ ...grpc.CallOption) (*userv1.UserResponse, error) {
	md, _ := metadata.FromOutgoingContext(ctx)
	if len(md.Get("authorization")) != 1 {
		f.t.Fatal("凭据未传递")
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > time.Second {
		f.t.Fatal("延长上游期限")
	}
	return &userv1.UserResponse{}, f.err
}

type fakeProducts struct {
	productv1.ProductServiceClient
	product *productv1.Product
	calls   int
}

// GetProduct 返回可信商品快照并记录调用次数。
func (f *fakeProducts) GetProduct(context.Context, *productv1.GetProductRequest, ...grpc.CallOption) (*productv1.ProductResponse, error) {
	f.calls++
	return &productv1.ProductResponse{Product: f.product}, nil
}

type fakeStore struct {
	Store
	existing *model.Conversation
	created  bool
}

// FindExisting 模拟既有会话读取。
func (f *fakeStore) FindExisting(context.Context, int64, int64) (*model.Conversation, error) {
	return f.existing, nil
}

// Create 记录服务端决定的参与者。
func (f *fakeStore) Create(ctx context.Context, p, b, s int64) (*model.Conversation, error) {
	f.created = true
	return &model.Conversation{ProductID: p, BuyerID: b, SellerID: s}, nil
}

// TestCreateRules 验证可信参与者、复用、自聊限制、账号禁用、凭据和截止时间。
func TestCreateRules(t *testing.T) {
	tokens, e := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "test", Audience: "test", AccessTTL: time.Hour})
	if e != nil {
		t.Fatal(e)
	}
	raw, _, e := tokens.IssueAccessToken(1, "USER")
	if e != nil {
		t.Fatal(e)
	}
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx, e := auth.Authenticate(parent, raw, tokens)
	if e != nil {
		t.Fatal(e)
	}
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+raw))
	store := &fakeStore{}
	products := &fakeProducts{product: &productv1.Product{Id: 10, SellerId: 2, Status: productv1.ProductStatus_PRODUCT_STATUS_ON_SALE}}
	s := New(store, fakeUsers{t: t}, products)
	c, e := s.Create(ctx, 10)
	if e != nil || c.BuyerID != 1 || c.SellerID != 2 {
		t.Fatal("参与者错误", e)
	}
	store.existing = c
	products.product = nil
	if _, e = s.Create(ctx, 10); e != nil || products.calls != 1 {
		t.Fatal("复用仍依赖商品查询", e)
	}
	store.existing = nil
	products.product = &productv1.Product{Id: 10, SellerId: 1, Status: productv1.ProductStatus_PRODUCT_STATUS_ON_SALE}
	if _, e = s.Create(ctx, 10); e == nil {
		t.Fatal("允许自聊")
	}
	s.users = fakeUsers{t: t, err: errors.New("disabled")}
	if _, e = s.Create(ctx, 10); e == nil {
		t.Fatal("禁用账号被放行")
	}
	if _, e = s.Create(context.Background(), 10); e == nil {
		t.Fatal("匿名被放行")
	}
}

// TestInvalidPagination 验证无效游标和互斥条件在访问依赖前被拒绝。
func TestInvalidPagination(t *testing.T) {
	s := New(nil, nil, nil)
	for _, cursor := range []string{"bad!", "e30"} {
		if _, _, e := s.List(context.Background(), cursor, 20); e == nil {
			t.Fatal("接受非法游标")
		}
	}
	for _, size := range []int{-1, 101} {
		if _, _, e := s.List(context.Background(), "", size); e == nil {
			t.Fatal("接受非法页大小")
		}
	}
	if _, _, e := s.Messages(context.Background(), 1, 2, 3, 20); e == nil {
		t.Fatal("接受互斥游标")
	}
}
