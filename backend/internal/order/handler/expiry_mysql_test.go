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
	"google.golang.org/protobuf/types/known/timestamppb"
	"net"
	"os"
	"sync"

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

// TestOrderExpiryMySQL 验证过期决定、三方竞争和释放恢复，不修改业务库，不修改业务库。
func TestOrderExpiryMySQL(t *testing.T) {
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
	name := fmt.Sprintf("remarket_order_expiry_test_%d", time.Now().UnixNano())
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
	buyer, seller := user(1), user(2)
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

	expiry := service.NewExpiry(orders)
	// 仅修改测试夹具的订单与模拟远程绝对期限，避免真实等待三十分钟；生产不允许改写期限。
	due := func(id int64) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, "UPDATE orders SET created_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 2 HOUR),expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 HOUR) WHERE id=?", id); err != nil {
			t.Fatal(err)
		}
		stored, err := orders.GetForParticipant(ctx, id, 1)
		if err != nil {
			t.Fatal(err)
		}
		remote.mu.Lock()
		if r := remote.records[id]; r != nil {
			r.ExpiresAt = timestamppb.New(stored.ExpiresAt)
		}
		remote.mu.Unlock()
	}
	first := seed("expire")
	due(first.ID)
	// 同时扫描、确认、取消，截止后只有过期决定可成功。
	results := make(chan error, 2)
	go func() { _, err := decisions.Confirm(seller, first.ID); results <- err }()
	go func() { _, err := decisions.Cancel(buyer, first.ID); results <- err }()
	changed, err := expiry.ExpireOne(ctx)
	if err != nil || !changed {
		t.Fatal("过期决定失败", err)
	}
	for i := 0; i < 2; i++ {
		if err := <-results; status.Code(grpcx.Error(err)) != codes.FailedPrecondition {
			t.Fatal("到期后确认或取消成功", err)
		}
	}
	if changed, err := expiry.ExpireOne(ctx); err != nil || changed {
		t.Fatal("重复扫描重复创建决定", err)
	}
	stored, err := orders.GetForParticipant(ctx, first.ID, 1)
	if err != nil || stored.Status != model.StatusExpiring || stored.ClosedAt != nil {
		t.Fatal("释放前过早终结订单", err)
	}
	remote.unavailable.Store(true)
	if _, err := worker.RecoverOne(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err = orders.GetForParticipant(ctx, first.ID, 1)
	if err != nil || stored.Status != model.StatusExpiring {
		t.Fatal("远端不可用时误关订单", err)
	}
	remote.unavailable.Store(false)
	if _, err := pool.ExecContext(ctx, "UPDATE order_operations SET next_retry_at=UTC_TIMESTAMP(6) WHERE order_id=? AND operation_type='RELEASE'", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RecoverOne(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err = orders.GetForParticipant(ctx, first.ID, 1)
	if err != nil || stored.Status != model.StatusExpired || stored.ClosedAt == nil {
		t.Fatal("释放后未关闭为 EXPIRED", err)
	}
	// 成交决定在截止前已提交，即使之后到期也不被扫描覆盖或释放。
	confirming := seed("confirming")
	if _, err := decisions.Confirm(buyer, confirming.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := decisions.Confirm(seller, confirming.ID); err != nil {
		t.Fatal(err)
	}
	due(confirming.ID)
	if changed, err := expiry.ExpireOne(ctx); err != nil || changed {
		t.Fatal("过期覆盖了成交决定", err)
	}
	if _, err := worker.RecoverOne(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err = orders.GetForParticipant(ctx, confirming.ID, 1)
	if err != nil || stored.Status != model.StatusCompleted {
		t.Fatal("成交决定未保持", err)
	}
	var count int64
	if err := db.WithContext(ctx).Model(&model.OrderOperation{}).Where("order_id=? AND operation_type='RELEASE'", confirming.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("成交订单产生释放", err)
	}
	cancelling := seed("cancelling")
	if _, err := decisions.Cancel(buyer, cancelling.ID); err != nil {
		t.Fatal(err)
	}
	due(cancelling.ID)
	if changed, err := expiry.ExpireOne(ctx); err != nil || changed {
		t.Fatal("过期覆盖了主动取消", err)
	}
	if _, err := worker.RecoverOne(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err = orders.GetForParticipant(ctx, cancelling.ID, 1)
	if err != nil || stored.Status != model.StatusCancelled {
		t.Fatal("主动取消被改为过期", err)
	}
	concurrent := seed("multi-scanner")
	due(concurrent.ID)
	var wg sync.WaitGroup
	scans := make(chan bool, 10)
	failures := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			changed, err := expiry.ExpireOne(ctx)
			scans <- changed
			if err != nil {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(scans)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	wins := 0
	for changed := range scans {
		if changed {
			wins++
		}
	}
	if wins != 1 {
		t.Fatal("多扫描器过期决定数错误", wins)
	}
	if err := db.WithContext(ctx).Model(&model.OrderOperation{}).Where("order_id=? AND operation_type='RELEASE'", concurrent.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("重复释放操作", err)
	}
	if _, err := worker.RecoverOne(ctx); err != nil {
		t.Fatal(err)
	}
	rollback := seed("rollback-expiry")
	due(rollback.ID)
	if _, err := pool.ExecContext(ctx, "CREATE TRIGGER fail_expiry_event BEFORE INSERT ON outbox_events FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='test failure'"); err != nil {
		t.Fatal(err)
	}
	if _, err := expiry.ExpireOne(ctx); err == nil {
		t.Fatal("过期事件故障未传播")
	}
	stored, err = orders.GetForParticipant(ctx, rollback.ID, 1)
	if err != nil || stored.Status != model.StatusReserved {
		t.Fatal("过期决定未回滚", err)
	}
	if err := db.WithContext(ctx).Model(&model.OrderOperation{}).Where("order_id=? AND operation_type='RELEASE'", rollback.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("回滚后残留释放操作", err)
	}
	if _, err := pool.ExecContext(ctx, "DROP TRIGGER fail_expiry_event"); err != nil {
		t.Fatal(err)
	}
	if _, err := expiry.ExpireOne(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RecoverOne(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := create.Create(buyer, 1, "pending-expired")
	if err != nil {
		t.Fatal(err)
	}
	due(pending.ID)
	if changed, err := expiry.ExpireOne(ctx); err != nil || changed {
		t.Fatal("PENDING 结果未知被直接过期", err)
	}
	stored, err = orders.GetForParticipant(ctx, pending.ID, 1)
	if err != nil || stored.Status != model.StatusPending {
		t.Fatal("PENDING 被误关闭", err)
	}
	// PENDING 夹具只验证扫描边界，随后标记其操作为未来重试，避免影响其他夹具的预占执行。
	if _, err := pool.ExecContext(ctx, "UPDATE order_operations SET next_retry_at=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR) WHERE order_id=?", pending.ID); err != nil {
		t.Fatal(err)
	}
	// 后台扫描器启动即处理，未到期订单保持 RESERVED；可正常取消退出。
	future := seed("future")
	if changed, err := expiry.ExpireOne(ctx); err != nil || changed {
		t.Fatal("未到期订单被关闭", err)
	}
	due(future.ID)
	scanCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); expiry.Run(scanCtx) }()
	defer func() { stop(); <-done }()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("后台扫描未推进过期")
		case <-ticker.C:
			stored, err := orders.GetForParticipant(ctx, future.ID, 1)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status == model.StatusExpiring {
				return
			}
		}
	}
}
