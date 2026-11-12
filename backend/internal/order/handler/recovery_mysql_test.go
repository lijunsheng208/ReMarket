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

	"google.golang.org/protobuf/types/known/timestamppb"
	productv1 "remarket/backend/gen/product/v1"
	orderclient "remarket/backend/internal/order/client"
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

// TestRecoveryMySQL 验证真实 Product RPC 的超时结果查询、持久化任务恢复与租约仲裁，不修改业务库，不修改业务库。
func TestRecoveryMySQL(t *testing.T) {
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
	name := fmt.Sprintf("remarket_order_recovery_test_%d", time.Now().UnixNano())
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
	identity, err := auth.Authenticate(ctx, token, tokens)
	if err != nil {
		t.Fatal(err)
	}
	orders := repository.New(db)
	create := service.NewCreation(orders, &creationProduct{})
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
	order, err := create.Create(identity, 1, "timeout")
	if err != nil {
		t.Fatal(err)
	}
	// 远程预占成功但 RPC 返回超时；随后查询应恢复 RESERVED。
	if worked, err := worker.RecoverOne(ctx); err != nil || !worked {
		t.Fatal("恢复执行失败", err)
	}
	got, err := orders.GetForParticipant(ctx, order.ID, 1)
	if err != nil || got.Status != model.StatusReserved || got.AggregateVersion != 2 {
		t.Fatal("超时成功结果未恢复", err)
	}
	var operation model.OrderOperation
	if err := db.WithContext(ctx).Where("order_id = ?", order.ID).Take(&operation).Error; err != nil {
		t.Fatal(err)
	}
	if operation.Status != model.OperationSucceeded || operation.FinishedAt == nil || operation.LockToken != nil {
		t.Fatal("操作成功状态不完整")
	}
	// 旧工作者崩溃，租约过期后由新实例接管；旧令牌不得写入失败。
	second, err := create.Create(identity, 2, "lease")
	if err != nil {
		t.Fatal(err)
	}
	old, err := orders.ClaimOperation(ctx)
	if err != nil || old == nil {
		t.Fatal(err)
	}
	if extra, err := orders.ClaimOperation(ctx); err != nil || extra != nil {
		t.Fatal("有效租约被重复领取", err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE order_operations SET locked_until=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE id=?", old.Operation.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := orders.ClaimOperation(ctx)
	if err != nil || fresh == nil || *fresh.Operation.LockToken == *old.Operation.LockToken {
		t.Fatal("过期租约未接管", err)
	}
	if err := orders.FinishOperation(ctx, old, service.RecoveryResult{Status: model.OperationFailed, ErrorCode: "STALE"}); err != nil {
		t.Fatal(err)
	}
	got, err = orders.GetForParticipant(ctx, second.ID, 1)
	if err != nil || got.Status != model.StatusPending {
		t.Fatal("旧令牌覆盖新租约", err)
	}
	// 模拟新实例也退出，强制租约到期，后续全新执行器读取持久化任务恢复。
	if _, err := pool.ExecContext(ctx, "UPDATE order_operations SET locked_until=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE id=?", fresh.Operation.ID); err != nil {
		t.Fatal(err)
	}
	restarted := service.NewRecovery(repository.New(db), products)
	if worked, err := restarted.RecoverOne(ctx); err != nil || !worked {
		t.Fatal("重建执行器恢复失败", err)
	}
	got, err = orders.GetForParticipant(ctx, second.ID, 1)
	if err != nil || got.Status != model.StatusReserved {
		t.Fatal("持久化操作未恢复", err)
	}
	third, err := create.Create(identity, 3, "unknown")
	if err != nil {
		t.Fatal(err)
	}
	remote.unavailable.Store(true)
	if _, err := worker.RecoverOne(ctx); err != nil {
		t.Fatal(err)
	}
	operation = model.OrderOperation{}
	if err := db.WithContext(ctx).Where("order_id = ?", third.ID).Take(&operation).Error; err != nil {
		t.Fatal(err)
	}
	if operation.Status != model.OperationRetry || operation.RetryCount != 1 || operation.LastError == nil || *operation.LastError != "REMOTE_UNKNOWN" {
		t.Fatal("未知结果未持久化重试")
	}
	got, err = orders.GetForParticipant(ctx, third.ID, 1)
	if err != nil || got.Status != model.StatusPending {
		t.Fatal("未知结果被当作失败", err)
	}
	remote.unavailable.Store(false)
	remote.rejected.Store(true)
	if _, err := pool.ExecContext(ctx, "UPDATE order_operations SET next_retry_at=UTC_TIMESTAMP(6) WHERE id=?", operation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RecoverOne(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = orders.GetForParticipant(ctx, third.ID, 1)
	if err != nil || got.Status != model.StatusFailed || got.ClosedAt == nil {
		t.Fatal("明确拒绝未关闭订单", err)
	}
	// 状态落地事件失败时订单和操作一并回滚，保留租约等待恢复。
	remote.rejected.Store(false)
	rollback, err := create.Create(identity, 5, "result-rollback")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "CREATE TRIGGER fail_recovery_event BEFORE INSERT ON outbox_events FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='test failure'"); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RecoverOne(ctx); err == nil {
		t.Fatal("结果事件故障未传播")
	}
	got, err = orders.GetForParticipant(ctx, rollback.ID, 1)
	if err != nil || got.Status != model.StatusPending {
		t.Fatal("结果失败时订单未回滚", err)
	}
	operation = model.OrderOperation{}
	if err := db.WithContext(ctx).Where("order_id = ?", rollback.ID).Take(&operation).Error; err != nil {
		t.Fatal(err)
	}
	if operation.Status != model.OperationProcessing || operation.FinishedAt != nil {
		t.Fatal("结果失败时操作未回滚")
	}
	if _, err := pool.ExecContext(ctx, "DROP TRIGGER fail_recovery_event"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE order_operations SET locked_until=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE id=?", operation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.RecoverOne(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = orders.GetForParticipant(ctx, rollback.ID, 1)
	if err != nil || got.Status != model.StatusReserved {
		t.Fatal("事件故障后的操作未恢复", err)
	}
	// Run 启动即扫描，随后按定时器扫描；取消后可等待退出。
	fourth, err := create.Create(identity, 4, "timer")
	if err != nil {
		t.Fatal(err)
	}
	remote.rejected.Store(false)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); restarted.Run(runCtx) }()
	defer func() { stop(); <-done }()
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("定时执行器未处理任务")
		case <-tick.C:
			stored, err := orders.GetForParticipant(ctx, fourth.ID, 1)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status == model.StatusReserved {
				return
			}
		}
	}
}

type recoveryProduct struct {
	productv1.UnimplementedProductServiceServer
	mu                    sync.Mutex
	records               map[int64]*productv1.Reservation
	unavailable, rejected atomic.Bool
}

// GetReservation 模拟真实远程记录查询，并验证用户凭据没有替代独立服务凭据。
func (p *recoveryProduct) GetReservation(ctx context.Context, req *productv1.GetReservationRequest) (*productv1.ReservationResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if values := md.Get("authorization"); len(values) != 1 || values[0] != "Bearer "+strings.Repeat("s", 32) {
		return nil, status.Error(codes.Unauthenticated, "bad service auth")
	}
	if p.unavailable.Load() {
		return nil, status.Error(codes.Unavailable, "sensitive remote detail")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	result := p.records[req.OrderId]
	if result == nil {
		return nil, status.Error(codes.NotFound, "missing")
	}
	return &productv1.ReservationResponse{Reservation: result}, nil
}

// ReserveProduct 模拟提交成功但调用超时，也支持确定拒绝；不把这些模拟当作真实 Product 事务验收。
func (p *recoveryProduct) ReserveProduct(ctx context.Context, req *productv1.ReserveProductRequest) (*productv1.ReservationResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if values := md.Get("authorization"); len(values) != 1 || values[0] != "Bearer "+strings.Repeat("s", 32) {
		return nil, status.Error(codes.Unauthenticated, "bad service auth")
	}
	if p.rejected.Load() {
		return nil, status.Error(codes.FailedPrecondition, "not for sale")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records[req.OrderId] = &productv1.Reservation{OrderId: req.OrderId, ProductId: req.ProductId, BuyerId: req.BuyerId, Status: productv1.ReservationStatus_RESERVATION_STATUS_ACTIVE, ExpiresAt: timestamppb.New(req.ExpiresAt.AsTime())}
	return nil, status.Error(codes.DeadlineExceeded, "response lost")
}

// ReleaseReservation 模拟释放提交成功但响应丢失，执行器须通过查询完成本地状态。
func (p *recoveryProduct) ReleaseReservation(ctx context.Context, req *productv1.ReleaseReservationRequest) (*productv1.ReservationResponse, error) {
	return p.finish(ctx, req.OrderId, req.ProductId, productv1.ReservationStatus_RESERVATION_STATUS_RELEASED)
}

// ConfirmReservation 模拟成交提交成功但响应丢失。
func (p *recoveryProduct) ConfirmReservation(ctx context.Context, req *productv1.ConfirmReservationRequest) (*productv1.ReservationResponse, error) {
	return p.finish(ctx, req.OrderId, req.ProductId, productv1.ReservationStatus_RESERVATION_STATUS_CONFIRMED)
}

// finish 校验独立凭据和终态竞争，仅用于恢复协议测试。
func (p *recoveryProduct) finish(ctx context.Context, orderID, productID int64, target productv1.ReservationStatus) (*productv1.ReservationResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if values := md.Get("authorization"); len(values) != 1 || values[0] != "Bearer "+strings.Repeat("s", 32) {
		return nil, status.Error(codes.Unauthenticated, "bad auth")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	record := p.records[orderID]
	if record == nil {
		return nil, status.Error(codes.NotFound, "missing")
	}
	if record.ProductId != productID || (record.Status != productv1.ReservationStatus_RESERVATION_STATUS_ACTIVE && record.Status != target) {
		return nil, status.Error(codes.FailedPrecondition, "conflict")
	}
	record.Status = target
	return nil, status.Error(codes.DeadlineExceeded, "response lost")
}
