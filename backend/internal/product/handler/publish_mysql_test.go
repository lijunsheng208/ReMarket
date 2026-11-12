package handler

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"sync"
	"testing"
	"time"
)

// TestPublicationMySQL 使用真实 MySQL 和 gRPC 验证发布原子性、并发及权限。
func TestPublicationMySQL(t *testing.T) {
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
	name := fmt.Sprintf("remarket_publish_test_%d", time.Now().UnixNano())
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
	server := grpcx.NewServer(nil, grpcx.AuthInterceptor(tokens))
	productv1.RegisterProductServiceServer(server, NewWithPublisher(service.New(repo), service.NewPublisher(repo)))
	listener := bufconn.Listen(1 << 20)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///publish", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := productv1.NewProductServiceClient(conn)
	token, _, err := tokens.IssueAccessToken(7, "USER")
	if err != nil {
		t.Fatal(err)
	}
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
	draft, err := client.CreateProduct(ctx, &productv1.CreateProductRequest{CategoryId: 1, Title: "键盘", PriceCent: 15000})
	if err != nil {
		t.Fatal(err)
	}
	req := &productv1.ChangeProductStateRequest{Id: draft.Product.Id, ExpectedVersion: 1}
	_, err = client.PublishProduct(context.Background(), req)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatal("匿名发布未拒绝")
	}
	other, _, err := tokens.IssueAccessToken(8, "ADMIN")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.PublishProduct(metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+other)), req)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatal("其他用户发布未拒绝")
	}
	// 两个客户端同时携带版本 1 发布：锁内再次比较版本，只允许一个提交。
	var workers sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		workers.Add(1)
		go func() { defer workers.Done(); _, err := client.PublishProduct(ctx, req); results <- err }()
	}
	workers.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if status.Code(err) == codes.FailedPrecondition {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("并发版本校验失败")
	}
	stored, err := repo.GetByID(ctx, req.Id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "ON_SALE" || stored.AggregateVersion != 2 || stored.FirstPublishedAt == nil {
		t.Fatal("发布状态错误")
	}
	var payload []byte
	var version int64
	var eventType, publishStatus string
	if err := pool.QueryRow("SELECT payload,aggregate_version,event_type,publish_status FROM outbox_events WHERE aggregate_id=?", req.Id).Scan(&payload, &version, &eventType, &publishStatus); err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		t.Fatal(err)
	}
	if version != 2 || eventType != "product.published.v1" || publishStatus != "PENDING" || snapshot["status"] != "ON_SALE" {
		t.Fatal("事件快照错误")
	}
	repeated, err := client.PublishProduct(ctx, &productv1.ChangeProductStateRequest{Id: req.Id, ExpectedVersion: 2})
	if err != nil || repeated.Product.AggregateVersion != 2 {
		t.Fatal("同版本重复发布失败", err)
	}
	var count int
	if err := pool.QueryRow("SELECT COUNT(*) FROM outbox_events WHERE aggregate_id=?", req.Id).Scan(&count); err != nil || count != 1 {
		t.Fatal("重复事件")
	}
	rollbackDraft, err := client.CreateProduct(ctx, &productv1.CreateProductRequest{CategoryId: 1, Title: "回滚商品", PriceCent: 100})
	if err != nil {
		t.Fatal(err)
	}
	// 仅在临时库注入 Outbox 写入故障，证明商品 UPDATE 后仍会完整回滚。
	if _, err := pool.Exec("CREATE TRIGGER fail_outbox BEFORE INSERT ON outbox_events FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected failure'"); err != nil {
		t.Fatal(err)
	}
	_, err = client.PublishProduct(ctx, &productv1.ChangeProductStateRequest{Id: rollbackDraft.Product.Id, ExpectedVersion: 1})
	if status.Code(err) != codes.Internal {
		t.Fatal("事件故障未传播", err)
	}
	after, err := repo.GetByID(ctx, rollbackDraft.Product.Id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "DRAFT" || after.AggregateVersion != 1 || after.FirstPublishedAt != nil {
		t.Fatal("商品未回滚")
	}
	if _, err := pool.Exec("DROP TRIGGER fail_outbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec("UPDATE categories SET enabled=false WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	_, err = client.PublishProduct(ctx, &productv1.ChangeProductStateRequest{Id: rollbackDraft.Product.Id, ExpectedVersion: 1})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatal("禁用分类未拒绝")
	}
}
