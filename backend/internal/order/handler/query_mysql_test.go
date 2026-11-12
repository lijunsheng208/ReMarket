package handler

import (
	"context"
	"database/sql"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"net"
	"os"
	orderv1 "remarket/backend/gen/order/v1"
	"remarket/backend/internal/order/model"
	"remarket/backend/internal/order/repository"
	"remarket/backend/internal/order/service"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/grpcx"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"remarket/backend/pkg/database"
)

// TestOrderQueryMySQL 验证真实 gRPC 查询、角色隔离及分页，不修改业务库，不修改业务库。
func TestOrderQueryMySQL(t *testing.T) {
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
		t.Fatal("无法创建测试数据库连接")
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := fmt.Sprintf("remarket_order_query_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name+" CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal("创建临时数据库失败")
	}
	// 清理仅针对本测试生成的库，使用独立超时以免主测试取消后残留。
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := admin.ExecContext(cleanup, "DROP DATABASE "+name); err != nil {
			t.Error("清理临时数据库失败：", name)
		}
	}()
	db, pool, err := database.OpenMySQL(ctx, database.MySQLConfig{Address: cfg.Addr, Username: cfg.User, Password: cfg.Passwd, Database: name})
	if err != nil {
		t.Fatal("连接临时数据库失败")
	}
	defer pool.Close()
	data, err := os.ReadFile("../../../../migrations/order/000001_core.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}
	for _, stmt := range strings.Split(strings.Join(lines, "\n"), ";") {
		if strings.TrimSpace(stmt) != "" {
			if _, err := pool.ExecContext(ctx, stmt); err != nil {
				t.Fatal("迁移失败：", err)
			}
		}
	}

	migration, err := os.ReadFile("../../../../migrations/order/000002_product_snapshot.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := int64(1); i <= 3; i++ {
		buyer, seller := int64(1), int64(2)
		if i == 3 {
			buyer, seller = 3, 1
		}
		order := model.Order{BuyerID: buyer, SellerID: seller, ProductID: i, PriceCent: 100, Status: model.StatusPending, IdempotencyKey: fmt.Sprint(i), RequestHash: strings.Repeat("a", 64), AggregateVersion: 1, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
		if err := db.WithContext(ctx).Create(&order).Error; err != nil {
			t.Fatal(err)
		}
	}
	tokens, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("j", 32), Issuer: "test", Audience: "test", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	server := grpcx.NewServer(nil, grpcx.AuthInterceptor(tokens))
	orderv1.RegisterOrderServiceServer(server, New(service.NewQuery(repository.New(db))))
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()
	conn, err := grpc.NewClient("passthrough:///test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := orderv1.NewOrderServiceClient(conn)
	// 测试用户由验签签发，伪造身份 metadata 不参与授权。
	userCtx := func(id int64) context.Context {
		t.Helper()
		token, _, err := tokens.IssueAccessToken(id, "USER")
		if err != nil {
			t.Fatal(err)
		}
		return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+token, "x-user-id", "1", "x-role", "ADMIN", "x-request-id", "order-query-test"))
	}
	owner := userCtx(1)
	var header metadata.MD
	got, err := client.GetOrder(owner, &orderv1.GetOrderRequest{Id: 1}, grpc.Header(&header))
	if err != nil || got.Order.BuyerId != 1 || got.Order.PriceCent != 100 || got.Order.Status != orderv1.OrderStatus_ORDER_STATUS_PENDING {
		t.Fatal("详情查询错误", err)
	}
	if len(header.Get("x-request-id")) != 1 || header.Get("x-request-id")[0] != "order-query-test" {
		t.Fatal("Request ID 未传递")
	}
	if _, err := client.GetOrder(userCtx(2), &orderv1.GetOrderRequest{Id: 1}); err != nil {
		t.Fatal("卖家无法读取订单", err)
	}
	if _, err := client.GetOrder(userCtx(4), &orderv1.GetOrderRequest{Id: 1}); status.Code(err) != codes.NotFound {
		t.Fatal("非参与人可读取订单", err)
	}
	if _, err := client.GetOrder(ctx, &orderv1.GetOrderRequest{Id: 1}); status.Code(err) != codes.Unauthenticated {
		t.Fatal("匿名读取未拒绝", err)
	}
	if _, err := client.GetOrder(owner, &orderv1.GetOrderRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatal("无效 ID 未拒绝", err)
	}
	if _, err := client.GetOrder(owner, &orderv1.GetOrderRequest{Id: 999}); status.Code(err) != codes.NotFound {
		t.Fatal("不存在订单错误", err)
	}
	list, err := client.ListOrders(owner, &orderv1.ListOrdersRequest{Role: orderv1.OrderRole_ORDER_ROLE_BUYER, PageSize: 1})
	if err != nil || list.Total != 2 || len(list.Orders) != 1 || list.Orders[0].Id != 2 {
		t.Fatal("买家分页错误", err)
	}
	list, err = client.ListOrders(owner, &orderv1.ListOrdersRequest{Role: orderv1.OrderRole_ORDER_ROLE_BUYER, PageSize: 1, Page: 2})
	if err != nil || list.Total != 2 || len(list.Orders) != 1 || list.Orders[0].Id != 1 {
		t.Fatal("第二页错误", err)
	}
	list, err = client.ListOrders(owner, &orderv1.ListOrdersRequest{Role: orderv1.OrderRole_ORDER_ROLE_SELLER})
	if err != nil || list.Total != 1 || len(list.Orders) != 1 || list.Orders[0].Id != 3 {
		t.Fatal("卖家列表错误", err)
	}
	list, err = client.ListOrders(userCtx(4), &orderv1.ListOrdersRequest{Role: orderv1.OrderRole_ORDER_ROLE_BUYER})
	if err != nil || list.Total != 0 || len(list.Orders) != 0 {
		t.Fatal("空列表错误", err)
	}
	for _, req := range []*orderv1.ListOrdersRequest{{}, {Role: 99}, {Role: 1, Page: -1}, {Role: 1, PageSize: 101}, {Role: 1, Page: 2147483647, PageSize: 100}} {
		if _, err := client.ListOrders(owner, req); status.Code(err) != codes.InvalidArgument {
			t.Fatal("无效分页或角色未拒绝", err)
		}
	}
	if _, err := client.CreateOrder(owner, &orderv1.CreateOrderRequest{ProductId: 1, IdempotencyKey: "test"}); status.Code(err) != codes.Unimplemented {
		t.Fatal("创建订单伪造成功", err)
	}
	if _, err := client.CancelOrder(owner, &orderv1.CancelOrderRequest{Id: 1}); status.Code(err) != codes.Unimplemented {
		t.Fatal("取消订单伪造成功", err)
	}
	if _, err := client.ConfirmOrder(owner, &orderv1.ConfirmOrderRequest{Id: 1}); status.Code(err) != codes.Unimplemented {
		t.Fatal("确认订单伪造成功", err)
	}
}
