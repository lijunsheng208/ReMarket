package handler

import (
	"context"
	"database/sql"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"net"
	"os"

	productv1 "remarket/backend/gen/product/v1"
	orderclient "remarket/backend/internal/order/client"
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

// TestOrderDecisionsMySQL 验证订单确认与取消竞争、幂等、权限及终结恢复，不修改业务库，不修改业务库。
func TestOrderDecisionsMySQL(t *testing.T) {
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
	name := fmt.Sprintf("remarket_order_decision_test_%d", time.Now().UnixNano())
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
	user := func(id int64) context.Context {
		t.Helper()
		token, _, err := tokens.IssueAccessToken(id, "USER")
		if err != nil {
			t.Fatal(err)
		}
		identity, err := auth.Authenticate(ctx, token, tokens)
		if err != nil {
			t.Fatal(err)
		}
		return identity
	}
	buyer, seller, stranger := user(1), user(2), user(3)
	orders := repository.New(db)
	create := service.NewCreation(orders, &creationProduct{})
	decisions := service.NewDecision(orders)
	remote := &recoveryProduct{records: make(map[int64]*productv1.Reservation)}
	listener := bufconn.Listen(1 << 20)
	server := grpcx.NewServer(nil)
	productv1.RegisterProductServiceServer(server, remote)
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()
	conn, err := grpc.NewClient("passthrough:///test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	products, err := orderclient.NewReservations(productv1.NewProductServiceClient(conn), strings.Repeat("s", 32), strings.Repeat("j", 32))
	if err != nil {
		t.Fatal(err)
	}
	worker := service.NewRecovery(orders, products)
	seed := func(key string) *model.Order {
		t.Helper()
		order, err := create.Create(buyer, 1, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := worker.RecoverOne(ctx); err != nil {
			t.Fatal(err)
		}
		order, err = orders.GetForParticipant(ctx, order.ID, 1)
		if err != nil || order.Status != model.StatusReserved {
			t.Fatal("预占夹具失败", err)
		}
		return order
	}
	first := seed("complete")
	if _, err := decisions.Confirm(stranger, first.ID); status.Code(grpcx.Error(err)) != codes.NotFound {
		t.Fatal("越权确认未拒绝", err)
	}
	one, err := decisions.Confirm(buyer, first.ID)
	if err != nil || !one.BuyerConfirmed || one.SellerConfirmed || one.Status != model.StatusReserved {
		t.Fatal("首方确认错误", err)
	}
	repeat, err := decisions.Confirm(buyer, first.ID)
	if err != nil || repeat.AggregateVersion != one.AggregateVersion {
		t.Fatal("重复确认不幂等", err)
	}
	two, err := decisions.Confirm(seller, first.ID)
	if err != nil || two.Status != model.StatusConfirming {
		t.Fatal("双方确认未持久化决定", err)
	}
	if _, err := decisions.Cancel(buyer, first.ID); status.Code(grpcx.Error(err)) != codes.FailedPrecondition {
		t.Fatal("成交决定后可取消", err)
	}
	if _, err := worker.RecoverOne(ctx); err != nil {
		t.Fatal(err)
	}
	completed, err := orders.GetForParticipant(ctx, first.ID, 1)
	if err != nil || completed.Status != model.StatusCompleted || completed.ClosedAt == nil {
		t.Fatal("成交恢复失败", err)
	}
	cancelled := seed("cancel")
	if _, err := decisions.Confirm(buyer, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	closing, err := decisions.Cancel(seller, cancelled.ID)
	if err != nil || closing.Status != model.StatusCancelling {
		t.Fatal("单方确认后取消错误", err)
	}
	again, err := decisions.Cancel(buyer, cancelled.ID)
	if err != nil || again.AggregateVersion != closing.AggregateVersion {
		t.Fatal("重复取消不幂等", err)
	}
	if _, err := worker.RecoverOne(ctx); err != nil {
		t.Fatal(err)
	}
	closed, err := orders.GetForParticipant(ctx, cancelled.ID, 1)
	if err != nil || closed.Status != model.StatusCancelled {
		t.Fatal("释放恢复未完成", err)
	}
	// 第一方已经确认，第二方确认与取消竞争同一个订单锁，只允许一项决定成功。
	race := seed("race")
	if _, err := decisions.Confirm(buyer, race.ID); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() { _, err := decisions.Confirm(seller, race.ID); results <- err }()
	go func() { _, err := decisions.Cancel(buyer, race.ID); results <- err }()
	successes, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			successes++
		} else if status.Code(grpcx.Error(err)) == codes.FailedPrecondition {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal("确认与取消同时胜出")
	}
	if _, err := worker.RecoverOne(ctx); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.WithContext(ctx).Model(&model.OrderOperation{}).Where("order_id = ? AND operation_type IN ?", race.ID, []string{"RELEASE", "CONFIRM"}).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("竞争产生相反操作", err)
	}
	expired := seed("expired")
	if _, err := pool.ExecContext(ctx, "UPDATE orders SET expires_at=DATE_ADD(created_at,INTERVAL 1 MICROSECOND) WHERE id=?", expired.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := decisions.Confirm(buyer, expired.ID); status.Code(grpcx.Error(err)) != codes.FailedPrecondition {
		t.Fatal("过期确认未拒绝", err)
	}
	if _, err := decisions.Cancel(seller, expired.ID); status.Code(grpcx.Error(err)) != codes.FailedPrecondition {
		t.Fatal("过期主动取消未拒绝", err)
	}
	rollback := seed("rollback")
	if _, err := pool.ExecContext(ctx, "CREATE TRIGGER fail_decision_event BEFORE INSERT ON outbox_events FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='test failure'"); err != nil {
		t.Fatal(err)
	}
	if _, err := decisions.Cancel(buyer, rollback.ID); err == nil {
		t.Fatal("事件故障未传播")
	}
	unchanged, err := orders.GetForParticipant(ctx, rollback.ID, 1)
	if err != nil || unchanged.Status != model.StatusReserved {
		t.Fatal("决定未回滚", err)
	}
	if err := db.WithContext(ctx).Model(&model.OrderOperation{}).Where("order_id = ? AND operation_type = ?", rollback.ID, model.OperationRelease).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("回滚后残留释放操作", err)
	}
}
