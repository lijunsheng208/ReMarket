package handler

import (
	"context"
	"errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"net"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/product/model"
	"remarket/backend/internal/product/repository"
	"remarket/backend/internal/product/service"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/grpcx"
	"strings"
	"testing"
	"time"
)

type draftRepo struct {
	enabled              bool
	lookupErr, createErr error
	params               repository.CreateDraftParams
	calls                int
}

// GetCategoryByID 模拟分类存在、停用及查询故障。
func (r *draftRepo) GetCategoryByID(ctx context.Context, id int64) (*model.Category, error) {
	return &model.Category{ID: id, Enabled: r.enabled}, r.lookupErr
}

// CreateDraft 捕获入库参数，模拟固定草稿状态和初始版本。
func (r *draftRepo) CreateDraft(ctx context.Context, p repository.CreateDraftParams) (*model.Product, error) {
	r.calls++
	r.params = p
	return &model.Product{ID: 1, SellerID: p.SellerID, CategoryID: p.CategoryID, Title: p.Title, Description: p.Description, PriceCent: p.PriceCent, Status: model.StatusDraft, AggregateVersion: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}, r.createErr
}

// TestCreateProductRPC 验证真实协议下的身份、参数、分类状态及错误映射。
func TestCreateProductRPC(t *testing.T) {
	tokens, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "remarket", Audience: "api", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := tokens.IssueAccessToken(42, "USER")
	if err != nil {
		t.Fatal(err)
	}
	repo := &draftRepo{enabled: true}
	svc := service.New(repo)
	server := grpcx.NewServer(nil, grpcx.AuthInterceptor(tokens))
	productv1.RegisterProductServiceServer(server, New(svc))
	listener := bufconn.Listen(1 << 20)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///draft", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := productv1.NewProductServiceClient(conn)
	req := &productv1.CreateProductRequest{CategoryId: 1, Title: "  机械键盘  ", Description: " 描述保留空格 ", PriceCent: 15000}
	_, err = client.CreateProduct(context.Background(), req)
	if status.Code(err) != codes.Unauthenticated || repo.calls != 0 {
		t.Fatal("匿名写入未拒绝")
	}
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+raw, "x-user-id", "999"))
	result, err := client.CreateProduct(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Product.SellerId != 42 || result.Product.Status != productv1.ProductStatus_PRODUCT_STATUS_DRAFT || result.Product.AggregateVersion != 1 || result.Product.FirstPublishedAt != nil || repo.params.Title != "机械键盘" || repo.params.Description != req.Description {
		t.Fatal("身份或草稿字段错误")
	}
	for _, name := range []string{"title", "long-title", "description", "category", "price"} {
		t.Run(name, func(t *testing.T) {
			input := &productv1.CreateProductRequest{CategoryId: 1, Title: "商品", PriceCent: 1}
			switch name {
			case "title":
				input.Title = " "
			case "long-title":
				input.Title = strings.Repeat("中", 129)
			case "description":
				input.Description = strings.Repeat("x", 16385)
			case "category":
				input.CategoryId = 0
			case "price":
				input.PriceCent = 0
			}
			before := repo.calls
			_, err := client.CreateProduct(ctx, input)
			if status.Code(err) != codes.InvalidArgument || repo.calls != before {
				t.Fatal("无效输入仍写入", err)
			}
		})
	}
	for _, tc := range []struct {
		enabled        bool
		lookup, create error
		code           codes.Code
	}{
		{false, nil, nil, codes.FailedPrecondition}, {true, repository.ErrCategoryNotFound, nil, codes.InvalidArgument}, {true, errors.New("db secret"), nil, codes.Internal}, {true, nil, repository.ErrCategoryReference, codes.InvalidArgument},
	} {
		repo.enabled = tc.enabled
		repo.lookupErr = tc.lookup
		repo.createErr = tc.create
		_, err := client.CreateProduct(ctx, req)
		if status.Code(err) != tc.code {
			t.Fatal("错误映射失败", err)
		}
	}
	_, err = client.PublishProduct(ctx, &productv1.ChangeProductStateRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatal("发布不应实现")
	}
	_, err = New(svc).CreateProduct(context.Background(), nil)
	if status.Code(grpcx.Error(err)) != codes.InvalidArgument {
		t.Fatal("空请求未拒绝")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = svc.CreateDraft(canceled, service.CreateDraftInput{})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("取消丢失")
	}
	_, err = svc.CreateDraft(context.Background(), service.CreateDraftInput{})
	if status.Code(grpcx.Error(err)) != codes.Unauthenticated {
		t.Fatal("直接调用未校验身份")
	}
}
