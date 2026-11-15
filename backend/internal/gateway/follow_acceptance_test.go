package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"io"
	"net"
	"net/http/httptest"
	"os"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/internal/user/handler"
	"remarket/backend/internal/user/repository"
	"remarket/backend/internal/user/service"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/database"
	"remarket/backend/pkg/grpcx"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFollowAcceptanceMySQL 通过真实数据库及 HTTP→gRPC 链路验证关注闭环，所有数据限定于临时库。
func TestFollowAcceptanceMySQL(t *testing.T) {
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
	name := fmt.Sprintf("remarket_follow_test_%d", time.Now().UnixNano())
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
	migration, err := os.ReadFile("../../../migrations/user/000001_core.up.sql")
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

	repo := repository.NewUserRepository(db)
	ids := make([]int64, 3)
	for i := range ids {
		u, e := repo.Create(context.Background(), repository.CreateUserParams{Username: fmt.Sprintf("user%d", i), PasswordHash: "test-only", Nickname: "验收"})
		if e != nil {
			t.Fatal(e)
		}
		ids[i] = u.ID
	}
	tokens, e := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "test", Audience: "test", AccessTTL: time.Hour})
	if e != nil {
		t.Fatal(e)
	}
	server := grpcx.NewServer(nil, grpcx.AuthInterceptor(tokens))
	userv1.RegisterUserServiceServer(server, handler.NewAuthenticated(nil, nil, service.NewCurrentUser(repo)).WithFollow(service.NewFollow(repo)))
	listener := bufconn.Listen(1 << 20)
	go server.Serve(listener)
	defer server.Stop()
	conn, e := grpc.NewClient("passthrough:///follow", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	client := userv1.NewUserServiceClient(conn)
	router := NewRouter(client, time.Second)
	AddFollowRoutes(router, client, tokens, time.Second)
	raw, _, e := tokens.IssueAccessToken(ids[0], "USER")
	if e != nil {
		t.Fatal(e)
	}
	// call 使用真实路由和 RPC，不输出令牌或数据库凭据。
	call := func(method, path, token string, want int) map[string]any {
		req := httptest.NewRequest(method, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("x-user-id", fmt.Sprint(ids[1]))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("%s %s: got %d want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		var result map[string]any
		b, _ := io.ReadAll(w.Result().Body)
		_ = json.Unmarshal(b, &result)
		return result
	}
	path := fmt.Sprintf("/api/v1/users/%d/follow", ids[1])
	call("POST", path, "", 401)
	call("POST", fmt.Sprintf("/api/v1/users/%d/follow", ids[0]), raw, 400)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); call("POST", path, raw, 200) }()
	}
	wg.Wait()
	var count int64
	if e := db.Table("follows").Where("follower_id = ? AND followee_id = ?", ids[0], ids[1]).Count(&count).Error; e != nil || count != 1 {
		t.Fatalf("unique relation count=%d err=%v", count, e)
	}
	call("POST", fmt.Sprintf("/api/v1/users/%d/follow", ids[2]), raw, 200)
	first := call("GET", fmt.Sprintf("/api/v1/users/%d/following?page_size=1", ids[0]), raw, 200)
	cursor, ok := first["next_after_id"].(string)
	if !ok || cursor == "0" {
		t.Fatalf("missing cursor: %v", first)
	}
	second := call("GET", fmt.Sprintf("/api/v1/users/%d/following?page_size=1&after_id=%s", ids[0], cursor), raw, 200)
	a := first["items"].([]any)
	b := second["items"].([]any)
	if len(a) != 1 || len(b) != 1 || a[0].(map[string]any)["id"] == b[0].(map[string]any)["id"] {
		t.Fatal("分页重复或缺失")
	}
	otherToken, _, err := tokens.IssueAccessToken(ids[1], "USER")
	if err != nil {
		t.Fatal(err)
	}
	fans := call("GET", fmt.Sprintf("/api/v1/users/%d/followers", ids[1]), otherToken, 200)
	if len(fans["items"].([]any)) != 1 {
		t.Fatal("粉丝列表错误")
	}
	call("GET", fmt.Sprintf("/api/v1/users/%d/following?page_size=101", ids[0]), raw, 400)
	call("POST", "/api/v1/users/9223372036854775807/follow", raw, 404)
	call("DELETE", path, raw, 200)
	call("DELETE", path, raw, 200)
	if e := db.Table("follows").Where("followee_id = ?", ids[1]).Count(&count).Error; e != nil || count != 0 {
		t.Fatal("取消关注未落库")
	}
}
