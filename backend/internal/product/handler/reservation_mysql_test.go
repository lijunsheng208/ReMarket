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
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/product/model"
	"remarket/backend/internal/product/repository"
	"remarket/backend/internal/product/service"
	"remarket/backend/pkg/grpcx"
	"strings"
	"sync"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"remarket/backend/pkg/database"
)

// TestReservationLifecycleMySQL 验证真实 RPC、事务回滚、重复请求及确认释放竞争，不修改业务库。
func TestReservationLifecycleMySQL(t *testing.T) {
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
	name := fmt.Sprintf("remarket_lifecycle_test_%d", time.Now().UnixNano())
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
	data, err := os.ReadFile("../../../../migrations/product/000001_core.up.sql")
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

	migration, err := os.ReadFile("../../../../migrations/product/000003_reservation_snapshot.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	interceptor, err := NewAuthInterceptor(nil, strings.Repeat("s", 32), strings.Repeat("j", 32))
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	server := grpcx.NewServer(nil, interceptor)
	// 失效任务表是所有商品写事务的新前置迁移，仅在测试临时库执行。
	cacheDDL, cacheErr := os.ReadFile("../../../../migrations/product/000004_cache_invalidations.up.sql")
	if cacheErr != nil {
		t.Fatal(cacheErr)
	}
	if cacheErr := db.Exec(string(cacheDDL)).Error; cacheErr != nil {
		t.Fatal(cacheErr)
	}

	productv1.RegisterProductServiceServer(server, New(nil).WithReservations(service.NewReservations(repository.NewProductRepository(db))))
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()
	conn, err := grpc.NewClient("passthrough:///test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := productv1.NewProductServiceClient(conn)
	rpcCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+strings.Repeat("s", 32)))
	if _, err := pool.ExecContext(ctx, "INSERT INTO categories (code,name) VALUES ('test','测试')"); err != nil {
		t.Fatal(err)
	}
	// 直接构造 ACTIVE 夹具，不代表 ReserveProduct 已实现；到期记录用于验证查询与成交不按本地时间自动释放。
	seed := func(id int64) {
		t.Helper()
		now := time.Now().UTC().Truncate(time.Microsecond)
		product := model.Product{ID: id, SellerID: 2, CategoryID: 1, Title: "测试", Description: "测试", PriceCent: 100, Status: model.StatusReserved, AggregateVersion: 2, FirstPublishedAt: &now, CreatedAt: now, UpdatedAt: now}
		if err := db.WithContext(ctx).Create(&product).Error; err != nil {
			t.Fatal(err)
		}
		reservation := model.ProductReservation{ProductID: id, OrderID: id, BuyerID: 3, Status: model.ReservationActive, ExpiresAt: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now}
		if err := db.WithContext(ctx).Create(&reservation).Error; err != nil {
			t.Fatal(err)
		}
	}
	seed(1)
	got, err := client.GetReservation(rpcCtx, &productv1.GetReservationRequest{OrderId: 1})
	if err != nil || got.Reservation.Status != productv1.ReservationStatus_RESERVATION_STATUS_ACTIVE {
		t.Fatal("到期记录查询失败", err)
	}
	if _, err := client.GetReservation(rpcCtx, &productv1.GetReservationRequest{OrderId: 999}); status.Code(err) != codes.NotFound {
		t.Fatal("不存在记录错误映射不正确", err)
	}
	if _, err := client.GetReservation(rpcCtx, &productv1.GetReservationRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatal("参数错误映射不正确", err)
	}
	seed(2)
	if _, err := client.ReleaseReservation(rpcCtx, &productv1.ReleaseReservationRequest{OrderId: 1, ProductId: 2}); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("错误商品未拒绝", err)
	}
	first, err := client.ReleaseReservation(rpcCtx, &productv1.ReleaseReservationRequest{OrderId: 1, ProductId: 1})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := client.ReleaseReservation(rpcCtx, &productv1.ReleaseReservationRequest{OrderId: 1, ProductId: 1})
	if err != nil || !first.Reservation.ReleasedAt.AsTime().Equal(duplicate.Reservation.ReleasedAt.AsTime()) {
		t.Fatal("重复释放未保留结果", err)
	}
	if _, err := client.ConfirmReservation(rpcCtx, &productv1.ConfirmReservationRequest{OrderId: 1, ProductId: 1}); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("已释放预占被确认", err)
	}
	if _, err := client.ConfirmReservation(rpcCtx, &productv1.ConfirmReservationRequest{OrderId: 2, ProductId: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ConfirmReservation(rpcCtx, &productv1.ConfirmReservationRequest{OrderId: 2, ProductId: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReleaseReservation(rpcCtx, &productv1.ReleaseReservationRequest{OrderId: 2, ProductId: 2}); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("已成交预占被释放", err)
	}
	seed(3)
	// 并发调用共享商品锁；一类决定获胜，其重复调用成功，另一类始终冲突。
	var wg sync.WaitGroup
	results := make(chan struct {
		confirm bool
		err     error
	}, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(confirm bool) {
			defer wg.Done()
			var err error
			if confirm {
				_, err = client.ConfirmReservation(rpcCtx, &productv1.ConfirmReservationRequest{OrderId: 3, ProductId: 3})
			} else {
				_, err = client.ReleaseReservation(rpcCtx, &productv1.ReleaseReservationRequest{OrderId: 3, ProductId: 3})
			}
			results <- struct {
				confirm bool
				err     error
			}{confirm, err}
		}(i%2 == 0)
	}
	wg.Wait()
	close(results)
	var winner *bool
	for result := range results {
		if result.err == nil {
			if winner != nil && *winner != result.confirm {
				t.Fatal("确认与释放同时成功")
			}
			v := result.confirm
			winner = &v
		} else if status.Code(result.err) != codes.FailedPrecondition {
			t.Fatal(result.err)
		}
	}
	if winner == nil {
		t.Fatal("并发操作没有成功者")
	}
	var product model.Product
	if err := db.WithContext(ctx).First(&product, 3).Error; err != nil {
		t.Fatal(err)
	}
	if product.AggregateVersion != 3 || (*winner && product.Status != model.StatusSold) || (!*winner && product.Status != model.StatusOnSale) {
		t.Fatal("并发结果与商品不一致")
	}
	var count int64
	if err := db.WithContext(ctx).Model(&model.OutboxEvent{}).Where("aggregate_id = ?", 3).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("并发事件重复", err)
	}
	seed(4)
	if _, err := pool.ExecContext(ctx, "CREATE TRIGGER fail_reservation_event BEFORE INSERT ON outbox_events FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='test failure'"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReleaseReservation(rpcCtx, &productv1.ReleaseReservationRequest{OrderId: 4, ProductId: 4}); status.Code(err) != codes.Internal {
		t.Fatal("事件故障映射不正确", err)
	}
	var unchanged model.ProductReservation
	if err := db.WithContext(ctx).Where("order_id = ?", 4).Take(&unchanged).Error; err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != model.ReservationActive || unchanged.ReleasedAt != nil {
		t.Fatal("预占未回滚")
	}
	product = model.Product{}
	if err := db.WithContext(ctx).First(&product, 4).Error; err != nil {
		t.Fatal(err)
	}
	if product.Status != model.StatusReserved || product.AggregateVersion != 2 {
		t.Fatal("商品未回滚")
	}
}
