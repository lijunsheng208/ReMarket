package handler

import (
	"context"
	"database/sql"

	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"net"
	"os"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/product/repository"
	"remarket/backend/internal/product/service"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/database"
	"remarket/backend/pkg/grpcx"
	"strings"

	"testing"
	"time"
)

type adminCheck struct{ allowed bool }

// IsCurrentAdmin 模拟对当前管理员角色的核实，不直接相信 Token 角色。
func (a *adminCheck) IsCurrentAdmin(context.Context) (bool, error) { return a.allowed, nil }

// TestQueryMySQL 验证真实 SQL 分页、图片与可选鉴权下的可见性。
func TestQueryMySQL(t *testing.T) {
	dsn := os.Getenv("REMARKET_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未配置 REMARKET_TEST_MYSQL_DSN，跳过真实 MySQL 验证")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("测试 DSN 无效")
	}
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("remarket_query_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name + " CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	// 只清理本测试生成的独立库，绝不删除用户提供的现有库。
	defer func() {
		if _, err := admin.Exec("DROP DATABASE " + name); err != nil {
			t.Error(err)
		}
	}()
	db, pool, err := database.OpenMySQL(context.Background(), database.MySQLConfig{Address: cfg.Addr, Username: cfg.User, Password: cfg.Passwd, Database: name})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	migration, err := os.ReadFile("../../../../migrations/product/000001_core.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	// 先移除整行 SQL 注释，避免注释中的分号被误当作语句分隔符。
	var statements []string
	for _, line := range strings.Split(string(migration), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			statements = append(statements, line)
		}
	}
	for _, statement := range strings.Split(strings.Join(statements, "\n"), ";") {
		if strings.TrimSpace(statement) != "" {
			if _, err := pool.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}

	seed, err := os.ReadFile("../../../../migrations/product/000002_seed_categories.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(string(seed)); err != nil {
		t.Fatal(err)
	}

	repo := repository.NewProductRepository(db)
	tokens, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "remarket", Audience: "api", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	server := grpcx.NewServer(nil, grpcx.OptionalAuthInterceptor(tokens, []string{productv1.ProductService_GetProduct_FullMethodName, productv1.ProductService_ListProducts_FullMethodName}))
	adminVerifier := &adminCheck{allowed: true}
	productv1.RegisterProductServiceServer(server, NewWithQuery(nil, nil, service.NewQuery(repo, adminVerifier)))
	listener := bufconn.Listen(1 << 20)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///query", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := productv1.NewProductServiceClient(conn)
	ctx := context.Background()
	var ids []int64
	for _, state := range []string{"DRAFT", "ON_SALE", "ON_SALE", "SOLD", "OFF_SHELF", "RESERVED"} {
		result, err := pool.Exec("INSERT INTO products(seller_id,category_id,title,description,price_cent,status,aggregate_version,first_published_at) VALUES(7,1,'商品','说明',100,?,2,?)", state, time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := pool.Exec("INSERT INTO product_images(product_id,object_key,sort_order) VALUES (?, 'second', 2), (?, 'first', 0)", ids[1], ids[1]); err != nil {
		t.Fatal(err)
	}
	list, err := client.ListProducts(ctx, &productv1.ListProductsRequest{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if list.Total != 2 || len(list.Products) != 1 || list.Products[0].Id != ids[2] {
		t.Fatal("列表排序或过滤错误")
	}
	list, err = client.ListProducts(ctx, &productv1.ListProductsRequest{Page: 2, PageSize: 1})
	if err != nil || len(list.Products) != 1 || list.Products[0].Id != ids[1] {
		t.Fatal("第二页错误", err)
	}
	list, err = client.ListProducts(ctx, &productv1.ListProductsRequest{CategoryId: 2})
	if err != nil || list.Total != 0 || len(list.Products) != 0 {
		t.Fatal("分类过滤错误")
	}
	for _, request := range []*productv1.ListProductsRequest{{Page: -1}, {PageSize: 101}, {CategoryId: -1}} {
		if _, err := client.ListProducts(ctx, request); status.Code(err) != codes.InvalidArgument {
			t.Fatal("无效分页未拒绝")
		}
	}
	detail, err := client.GetProduct(ctx, &productv1.GetProductRequest{Id: ids[1]})
	if err != nil || len(detail.Product.Images) != 2 || detail.Product.Images[0].ObjectKey != "first" {
		t.Fatal("详情图片排序错误", err)
	}
	ownerToken, _, err := tokens.IssueAccessToken(7, "USER")
	if err != nil {
		t.Fatal(err)
	}
	owner := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+ownerToken))
	for _, id := range []int64{ids[0], ids[3], ids[4], ids[5]} {
		if _, err := client.GetProduct(ctx, &productv1.GetProductRequest{Id: id}); status.Code(err) != codes.NotFound {
			t.Fatal("匿名看到非在售商品")
		}
		if _, err := client.GetProduct(owner, &productv1.GetProductRequest{Id: id}); err != nil {
			t.Fatal("卖家不可见", err)
		}
	}
	otherToken, _, err := tokens.IssueAccessToken(8, "USER")
	if err != nil {
		t.Fatal(err)
	}
	other := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+otherToken, "x-user-id", "7"))
	if _, err := client.GetProduct(other, &productv1.GetProductRequest{Id: ids[0]}); status.Code(err) != codes.NotFound {
		t.Fatal("越权详情")
	}
	administratorToken, _, err := tokens.IssueAccessToken(8, "ADMIN")
	if err != nil {
		t.Fatal(err)
	}
	administrator := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+administratorToken))
	if _, err := client.GetProduct(administrator, &productv1.GetProductRequest{Id: ids[0]}); err != nil {
		t.Fatal("管理员不可见", err)
	}
	adminVerifier.allowed = false
	if _, err := client.GetProduct(administrator, &productv1.GetProductRequest{Id: ids[0]}); status.Code(err) != codes.NotFound {
		t.Fatal("旧管理员角色仍生效")
	}
	invalid := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer bad"))
	if _, err := client.GetProduct(invalid, &productv1.GetProductRequest{Id: ids[1]}); status.Code(err) != codes.Unauthenticated {
		t.Fatal("无效令牌降级匿名")
	}
	if _, err := client.GetProduct(ctx, &productv1.GetProductRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatal("零 ID 未拒绝")
	}
	if _, err := client.GetProduct(ctx, &productv1.GetProductRequest{Id: 99999}); status.Code(err) != codes.NotFound {
		t.Fatal("不存在商品错误")
	}
}
