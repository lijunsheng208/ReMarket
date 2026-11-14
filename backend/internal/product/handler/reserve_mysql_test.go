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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
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

// TestReserveProductMySQL 验证首次预占、完整幂等、百请求竞争与事务回滚，不修改业务库。
func TestReserveProductMySQL(t *testing.T) {
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
	name := fmt.Sprintf("remarket_reserve_test_%d", time.Now().UnixNano())
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

	now := time.Now().UTC().Truncate(time.Microsecond)
	seed := func(id int64) {
		t.Helper()
		product := model.Product{ID: id, SellerID: 2, CategoryID: 1, Title: "测试", Description: "测试", PriceCent: 100, Status: model.StatusOnSale, AggregateVersion: 2, FirstPublishedAt: &now, CreatedAt: now, UpdatedAt: now}
		if err := db.WithContext(ctx).Create(&product).Error; err != nil {
			t.Fatal(err)
		}
	}
	request := func(order, product int64) *productv1.ReserveProductRequest {
		return &productv1.ReserveProductRequest{OrderId: order, ProductId: product, BuyerId: 3, ExpectedSellerId: 2, ExpectedPriceCent: 100, ExpectedProductVersion: 2, ExpiresAt: timestamppb.New(now.Add(time.Hour))}
	}
	seed(1)
	req := request(1, 1)
	first, err := client.ReserveProduct(rpcCtx, req)
	if err != nil || first.Reservation.Status != productv1.ReservationStatus_RESERVATION_STATUS_ACTIVE {
		t.Fatal("首次预占失败", err)
	}
	duplicate, err := client.ReserveProduct(rpcCtx, req)
	if err != nil || !proto.Equal(first, duplicate) {
		t.Fatal("重复预占不幂等", err)
	}
	changed := proto.Clone(req).(*productv1.ReserveProductRequest)
	changed.ExpectedPriceCent++
	if _, err := client.ReserveProduct(rpcCtx, changed); status.Code(err) != codes.AlreadyExists {
		t.Fatal("同订单不同参数未拒绝", err)
	}
	if _, err := client.ReleaseReservation(rpcCtx, &productv1.ReleaseReservationRequest{OrderId: 1, ProductId: 1}); err != nil {
		t.Fatal(err)
	}
	terminal, err := client.ReserveProduct(rpcCtx, req)
	if err != nil || terminal.Reservation.Status != productv1.ReservationStatus_RESERVATION_STATUS_RELEASED {
		t.Fatal("终态预占被重新激活", err)
	}
	seed(2)
	for _, mutate := range []func(*productv1.ReserveProductRequest){
		func(r *productv1.ReserveProductRequest) { r.BuyerId = 2 },
		func(r *productv1.ReserveProductRequest) { r.ExpectedPriceCent = 101 },
		func(r *productv1.ReserveProductRequest) { r.ExpectedProductVersion = 1 },
		func(r *productv1.ReserveProductRequest) { r.ExpectedSellerId = 4 },
		func(r *productv1.ReserveProductRequest) { r.ExpiresAt = timestamppb.New(now.Add(-time.Hour)) },
	} {
		r := request(2, 2)
		mutate(r)
		if _, err := client.ReserveProduct(rpcCtx, r); status.Code(err) != codes.FailedPrecondition {
			t.Fatal("非法快照或自购未拒绝", err)
		}
	}
	invalid := request(2, 2)
	invalid.ExpiresAt.Nanos++
	if _, err := client.ReserveProduct(rpcCtx, invalid); status.Code(err) != codes.InvalidArgument {
		t.Fatal("非微秒精度未拒绝", err)
	}
	// 同商品百请求不同订单竞争库存，必须恰有一个赢家。
	var wg sync.WaitGroup
	results := make(chan error, 100)
	for i := int64(10); i < 110; i++ {
		wg.Add(1)
		go func(order int64) {
			defer wg.Done()
			r := request(order, 2)
			r.BuyerId = order
			_, err := client.ReserveProduct(rpcCtx, r)
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if status.Code(err) != codes.FailedPrecondition {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("预占成功数 %d", successes)
	}
	var count int64
	if err := db.WithContext(ctx).Model(&model.ProductReservation{}).Where("product_id = ?", 2).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("预占记录数不正确", err)
	}
	if err := db.WithContext(ctx).Model(&model.OutboxEvent{}).Where("aggregate_id = ?", 2).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("预占事件数不正确", err)
	}
	// 同订单竞争不同商品，唯一键冲突必须映射为参数冲突且失败商品完整回滚。
	seed(3)
	seed(4)
	cross := make(chan error, 2)
	for _, id := range []int64{3, 4} {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			_, err := client.ReserveProduct(rpcCtx, request(200, id))
			cross <- err
		}(id)
	}
	wg.Wait()
	close(cross)
	successes = 0
	conflicts := 0
	for err := range cross {
		if err == nil {
			successes++
		} else if status.Code(err) == codes.AlreadyExists {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal("跨商品同订单仲裁错误")
	}
	if err := db.WithContext(ctx).Model(&model.Product{}).Where("id IN (3,4) AND status = ?", model.StatusOnSale).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("竞争失败商品未回滚", err)
	}
	seed(5)
	if _, err := pool.ExecContext(ctx, "CREATE TRIGGER fail_reserve_event BEFORE INSERT ON outbox_events FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='test failure'"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReserveProduct(rpcCtx, request(300, 5)); status.Code(err) != codes.Internal {
		t.Fatal("Outbox 故障未传播", err)
	}
	var unchanged model.Product
	if err := db.WithContext(ctx).First(&unchanged, 5).Error; err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != model.StatusOnSale || unchanged.AggregateVersion != 2 {
		t.Fatal("商品未回滚")
	}
	if err := db.WithContext(ctx).Model(&model.ProductReservation{}).Where("order_id = ?", 300).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("预占未回滚", err)
	}
}
