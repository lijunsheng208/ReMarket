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

// TestMutationMySQL 验证编辑、下架、重新上架的权限、版本及事件原子性。
func TestMutationMySQL(t *testing.T) {
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
	name := fmt.Sprintf("remarket_mutation_test_%d", time.Now().UnixNano())
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
	verifier := &adminCheck{allowed: true}
	server := grpcx.NewServer(nil, grpcx.AuthInterceptor(tokens))
	productv1.RegisterProductServiceServer(server, NewComplete(service.New(repo), service.NewPublisher(repo), service.NewQuery(repo, verifier), service.NewMutation(repo, verifier)))
	listener := bufconn.Listen(1 << 20)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///mutation", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
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
	draft, err := client.CreateProduct(ctx, &productv1.CreateProductRequest{CategoryId: 1, Title: "键盘", PriceCent: 100})
	if err != nil {
		t.Fatal(err)
	}
	id := draft.Product.Id
	title := " 新键盘 "
	edited, err := client.UpdateProduct(ctx, &productv1.UpdateProductRequest{Id: id, ExpectedVersion: 1, Title: &title})
	if err != nil || edited.Product.AggregateVersion != 2 || edited.Product.Title != "新键盘" {
		t.Fatal("草稿编辑失败", err)
	}
	var count int
	pool.QueryRow("SELECT COUNT(*) FROM outbox_events").Scan(&count)
	if count != 0 {
		t.Fatal("草稿编辑发事件")
	}
	first, err := client.PublishProduct(ctx, &productv1.ChangeProductStateRequest{Id: id, ExpectedVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	// optional 空字符串可清空描述；在售编辑必须生成 updated 快照。
	description := ""
	price := int64(200)
	edited, err = client.UpdateProduct(ctx, &productv1.UpdateProductRequest{Id: id, ExpectedVersion: 3, Description: &description, PriceCent: &price})
	if err != nil || edited.Product.AggregateVersion != 4 {
		t.Fatal("在售编辑失败", err)
	}
	unchanged, err := client.UpdateProduct(ctx, &productv1.UpdateProductRequest{Id: id, ExpectedVersion: 4, PriceCent: &price})
	if err != nil || unchanged.Product.AggregateVersion != 4 {
		t.Fatal("无变化增加版本", err)
	}
	if _, err := client.UpdateProduct(ctx, &productv1.UpdateProductRequest{Id: id, ExpectedVersion: 4}); status.Code(err) != codes.InvalidArgument {
		t.Fatal("空编辑未拒绝")
	}
	// 相同版本并发编辑，只允许一个写入及一个新事件。
	var workers sync.WaitGroup
	results := make(chan error, 2)
	for i := range 2 {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			value := int64(300 + i)
			_, err := client.UpdateProduct(ctx, &productv1.UpdateProductRequest{Id: id, ExpectedVersion: 4, PriceCent: &value})
			results <- err
		}(i)
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
		t.Fatal("并发编辑覆盖")
	}
	off, err := client.OffShelfProduct(ctx, &productv1.ChangeProductStateRequest{Id: id, ExpectedVersion: 5})
	if err != nil || off.Product.Status != productv1.ProductStatus_PRODUCT_STATUS_OFF_SHELF {
		t.Fatal("下架失败", err)
	}
	if _, err := client.OffShelfProduct(ctx, &productv1.ChangeProductStateRequest{Id: id, ExpectedVersion: 6}); err != nil {
		t.Fatal("重复下架失败", err)
	}
	relisted, err := client.PublishProduct(ctx, &productv1.ChangeProductStateRequest{Id: id, ExpectedVersion: 6})
	if err != nil || relisted.Product.AggregateVersion != 7 || !relisted.Product.FirstPublishedAt.AsTime().Equal(first.Product.FirstPublishedAt.AsTime()) {
		t.Fatal("重新上架覆盖首次时间", err)
	}
	if err := pool.QueryRow("SELECT COUNT(*) FROM outbox_events WHERE event_type='product.published.v1'").Scan(&count); err != nil || count != 1 {
		t.Fatal("首次发布重复")
	}
	var payload []byte
	if err := pool.QueryRow("SELECT payload FROM outbox_events WHERE event_type='product.off_shelf.v1'").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	json.Unmarshal(payload, &data)
	if data["status"] != "OFF_SHELF" {
		t.Fatal("下架快照状态错误")
	}
	adminToken, _, err := tokens.IssueAccessToken(8, "ADMIN")
	if err != nil {
		t.Fatal(err)
	}
	administrator := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+adminToken))
	if _, err := client.UpdateProduct(administrator, &productv1.UpdateProductRequest{Id: id, ExpectedVersion: 7, PriceCent: &price}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("管理员代编辑")
	}
	verifier.allowed = false
	if _, err := client.OffShelfProduct(administrator, &productv1.ChangeProductStateRequest{Id: id, ExpectedVersion: 7}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("旧管理员权限未拒绝")
	}
	verifier.allowed = true
	if _, err := client.OffShelfProduct(administrator, &productv1.ChangeProductStateRequest{Id: id, ExpectedVersion: 7}); err != nil {
		t.Fatal("管理员下架失败", err)
	}
	if _, err := client.PublishProduct(ctx, &productv1.ChangeProductStateRequest{Id: id, ExpectedVersion: 8}); err != nil {
		t.Fatal(err)
	}
	before, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec("CREATE TRIGGER fail_mutation BEFORE INSERT ON outbox_events FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected failure'"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UpdateProduct(ctx, &productv1.UpdateProductRequest{Id: id, ExpectedVersion: 9, PriceCent: &price}); status.Code(err) != codes.Internal {
		t.Fatal("编辑故障未传播")
	}
	if _, err := client.OffShelfProduct(ctx, &productv1.ChangeProductStateRequest{Id: id, ExpectedVersion: 9}); status.Code(err) != codes.Internal {
		t.Fatal("下架故障未传播")
	}
	after, err := repo.GetByID(ctx, id)
	if err != nil || before.PriceCent != after.PriceCent || before.Status != after.Status || before.AggregateVersion != after.AggregateVersion {
		t.Fatal("事件故障未回滚商品")
	}
	if _, err := pool.Exec("DROP TRIGGER fail_mutation"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"RESERVED", "SOLD"} {
		if _, err := pool.Exec("UPDATE products SET status=? WHERE id=?", state, id); err != nil {
			t.Fatal(err)
		}
		if _, err := client.UpdateProduct(ctx, &productv1.UpdateProductRequest{Id: id, ExpectedVersion: 9, PriceCent: &price}); status.Code(err) != codes.FailedPrecondition {
			t.Fatal("交易状态可编辑")
		}
		if _, err := client.OffShelfProduct(ctx, &productv1.ChangeProductStateRequest{Id: id, ExpectedVersion: 9}); status.Code(err) != codes.FailedPrecondition {
			t.Fatal("交易状态可下架")
		}
	}
}
