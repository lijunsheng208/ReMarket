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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"remarket/backend/pkg/database"
)

// TestOrderCreationMySQL 验证幂等创建、恢复参数和操作/事件原子落库，不修改业务库，不修改业务库。
func TestOrderCreationMySQL(t *testing.T) {
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
	name := fmt.Sprintf("remarket_order_create_test_%d", time.Now().UnixNano())
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

	tokens, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("j", 32), Issuer: "test", Audience: "test", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := tokens.IssueAccessToken(1, "USER")
	if err != nil {
		t.Fatal(err)
	}
	products := &creationProduct{}
	orders := repository.New(db)
	listener := bufconn.Listen(1 << 20)
	server := grpcx.NewServer(nil, grpcx.AuthInterceptor(tokens))
	orderv1.RegisterOrderServiceServer(server, New(service.NewQuery(orders)).WithCreator(service.NewCreation(orders, products)))
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()
	conn, err := grpc.NewClient("passthrough:///test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := orderv1.NewOrderServiceClient(conn)
	rpcCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
	request := &orderv1.CreateOrderRequest{ProductId: 1, IdempotencyKey: "same-key"}
	var wg sync.WaitGroup
	results := make(chan *orderv1.OrderResponse, 20)
	failures := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := client.CreateOrder(rpcCtx, request)
			if err != nil {
				failures <- err
			} else {
				results <- result
			}
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var id int64
	for result := range results {
		if result.Order.Status != orderv1.OrderStatus_ORDER_STATUS_PENDING {
			t.Fatal("未预占却宣告成功预占")
		}
		if id != 0 && id != result.Order.Id {
			t.Fatal("同键产生多个订单")
		}
		id = result.Order.Id
	}
	if id == 0 {
		t.Fatal("创建没有成功结果")
	}
	var order model.Order
	if err := db.WithContext(ctx).First(&order, id).Error; err != nil {
		t.Fatal(err)
	}
	if order.ExpectedProductVersion == nil || *order.ExpectedProductVersion != 7 || order.PriceCent != 100 || order.BuyerID != 1 || order.SellerID != 2 {
		t.Fatal("恢复请求快照不完整")
	}
	var operation model.OrderOperation
	if err := db.WithContext(ctx).Where("order_id = ?", id).Take(&operation).Error; err != nil {
		t.Fatal(err)
	}
	if operation.Status != model.OperationPending || operation.OperationType != model.OperationReserve || operation.OperationID == "" {
		t.Fatal("预占操作未持久化")
	}
	for _, table := range []string{"orders", "order_operations", "outbox_events"} {
		var count int64
		if err := db.WithContext(ctx).Table(table).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("%s 不止一条记录：%d %v", table, count, err)
		}
	}
	// 商品端不可用时重复请求仍返回原订单，不重新读取或延长快照。
	products.unavailable.Store(true)
	calls := products.calls.Load()
	duplicate, err := client.CreateOrder(rpcCtx, request)
	if err != nil || duplicate.Order.Id != id || !duplicate.Order.ExpiresAt.AsTime().Equal(order.ExpiresAt) || products.calls.Load() != calls {
		t.Fatal("重试依赖了商品或改变期限", err)
	}
	if _, err := client.CreateOrder(rpcCtx, &orderv1.CreateOrderRequest{ProductId: 2, IdempotencyKey: "same-key"}); status.Code(err) != codes.AlreadyExists {
		t.Fatal("同键不同参数未拒绝", err)
	}
	for _, key := range []string{"", "a b", strings.Repeat("a", 129), "中文"} {
		if _, err := client.CreateOrder(rpcCtx, &orderv1.CreateOrderRequest{ProductId: 1, IdempotencyKey: key}); status.Code(err) != codes.InvalidArgument {
			t.Fatal("非法幂等键未拒绝", err)
		}
	}
	if _, err := client.CreateOrder(ctx, request); status.Code(err) != codes.Unauthenticated {
		t.Fatal("匿名创建未拒绝", err)
	}
	products.unavailable.Store(false)
	if _, err := pool.ExecContext(ctx, "CREATE TRIGGER fail_order_event BEFORE INSERT ON outbox_events FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='test failure'"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateOrder(rpcCtx, &orderv1.CreateOrderRequest{ProductId: 1, IdempotencyKey: "rollback"}); status.Code(err) != codes.Internal {
		t.Fatal("事件失败未返回内部错误", err)
	}
	for _, table := range []string{"orders", "order_operations", "outbox_events"} {
		var count int64
		if err := db.WithContext(ctx).Table(table).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("%s 事务未回滚：%d %v", table, count, err)
		}
	}
}

type creationProduct struct {
	unavailable atomic.Bool
	calls       atomic.Int64
}

// GetForOrder 模拟可信商品快照与下游故障，检查幂等重试不依赖新的商品读取。
func (p *creationProduct) GetForOrder(ctx context.Context, id int64) (*service.ProductSnapshot, error) {
	p.calls.Add(1)
	if p.unavailable.Load() {
		return nil, status.Error(codes.Unavailable, "test unavailable")
	}
	return &service.ProductSnapshot{ID: id, SellerID: 2, PriceCent: 100, Version: 7, OnSale: true}, nil
}
