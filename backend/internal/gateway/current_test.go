package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	driver "github.com/go-sql-driver/mysql"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	userv1 "remarket/backend/gen/user/v1"
	userhandler "remarket/backend/internal/user/handler"
	"remarket/backend/internal/user/repository"
	"remarket/backend/internal/user/service"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/database"
	"remarket/backend/pkg/grpcx"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type currentProbe struct{ calls atomic.Int32 }

// GetCurrentUser 记录是否访问下游，确认无效凭据在 Gateway 被拦截。
func (p *currentProbe) GetCurrentUser(ctx context.Context, req *userv1.GetCurrentUserRequest, opts ...grpc.CallOption) (*userv1.UserResponse, error) {
	p.calls.Add(1)
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("可信身份丢失")
	}
	return &userv1.UserResponse{User: &userv1.User{Id: identity.UserID}}, nil
}

// TestGatewayBearer 验证本地验签、重复凭据拒绝及身份头不能绕过认证。
func TestGatewayBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokens, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "remarket", Audience: "api", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := tokens.IssueAccessToken(42, "USER")
	if err != nil {
		t.Fatal(err)
	}
	probe := &currentProbe{}
	router := NewAuthenticatedRouter(nil, probe, tokens, time.Second)
	for _, values := range [][]string{nil, {"Bearer invalid"}, {"Basic " + token}, {"Bearer " + token, "Bearer " + token}} {
		req := httptest.NewRequest("GET", "/api/v1/users/me", nil)
		for _, v := range values {
			req.Header.Add("Authorization", v)
		}
		req.Header.Set("X-User-ID", "42")
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Code != 401 || res.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatal("认证未拒绝", res.Code)
		}
	}
	if probe.calls.Load() != 0 {
		t.Fatal("无效凭据进入下游")
	}
	req := httptest.NewRequest("GET", "/api/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-User-ID", "999")
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"42"`) {
		t.Fatal("身份不正确", res.Body.String())
	}
}

// TestAuthFlowMySQL 验证真实 HTTP、gRPC、User 业务、JWT、Repository 和 MySQL 完整链路。
func TestAuthFlowMySQL(t *testing.T) {
	dsn := os.Getenv("REMARKET_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未配置测试 MySQL DSN")
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
	name := fmt.Sprintf("remarket_auth_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name + " CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	// 只清理本次新建临时库，不操作业务库。
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
	var lines []string
	for _, line := range strings.Split(string(migration), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
		if strings.TrimSpace(statement) != "" {
			if _, err := pool.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	config := auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "remarket", Audience: "api", AccessTTL: time.Hour}
	userTokens, err := auth.NewJWTManager(config)
	if err != nil {
		t.Fatal(err)
	}
	gatewayTokens, err := auth.NewJWTManager(config)
	if err != nil {
		t.Fatal(err)
	}
	users := repository.NewUserRepository(db)
	login, err := service.NewLogin(users, userTokens)
	if err != nil {
		t.Fatal(err)
	}
	server := grpcx.NewServer(nil, grpcx.AuthInterceptor(userTokens, userv1.UserService_Register_FullMethodName, userv1.UserService_Login_FullMethodName))
	userv1.RegisterUserServiceServer(server, userhandler.NewAuthenticated(service.New(users), login, service.NewCurrentUser(users)))
	listener := bufconn.Listen(1 << 20)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///auth", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithUnaryInterceptor(grpcx.ClientInterceptor(3*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	gin.SetMode(gin.TestMode)
	client := userv1.NewUserServiceClient(conn)
	httpServer := httptest.NewServer(NewAuthenticatedRouter(client, client, gatewayTokens, 3*time.Second))
	defer httpServer.Close()
	call := func(method, path, body, token string) (int, map[string]json.RawMessage) {
		t.Helper()
		req, err := http.NewRequest(method, httpServer.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-ID", "e2e-review")
		req.Header.Set("X-User-ID", "999")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := httpServer.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result map[string]json.RawMessage
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if response.Header.Get("X-Request-ID") != "e2e-review" {
			t.Fatal("请求标识丢失")
		}
		return response.StatusCode, result
	}
	code, _ := call("POST", "/api/v1/auth/register", `{"username":" ALICE ","password":"password123","nickname":"测试用户"}`, "")
	if code != 201 {
		t.Fatal("注册失败", code)
	}
	code, result := call("POST", "/api/v1/auth/login", `{"username":"ALICE","password":"password123"}`, "")
	if code != 200 {
		t.Fatal("登录失败", code)
	}
	var token string
	if err := json.Unmarshal(result["access_token"], &token); err != nil {
		t.Fatal(err)
	}
	code, result = call("GET", "/api/v1/users/me", "", token)
	if code != 200 {
		t.Fatal("当前用户失败", code)
	}
	// HTTP 的 int64 ID 为字符串，通过普通映射核查，避免标准 JSON 解码为 int64。
	var public map[string]any
	if err := json.Unmarshal(result["user"], &public); err != nil {
		t.Fatal(err)
	}
	if public["username"] != "alice" || public["id"] == "999" {
		t.Fatal("身份错误")
	}
	if code, _ := call("GET", "/api/v1/users/me", "", token+"tampered"); code != 401 {
		t.Fatal("篡改令牌未拒绝")
	}
	if _, err := pool.Exec("UPDATE users SET status = 'DISABLED' WHERE username = 'alice'"); err != nil {
		t.Fatal(err)
	}
	if code, _ := call("GET", "/api/v1/users/me", "", token); code != 403 {
		t.Fatal("禁用未即时生效", code)
	}
	if code, _ := call("POST", "/api/v1/auth/login", `{"username":"alice","password":"password123"}`, ""); code != 401 {
		t.Fatal("禁用仍可登录")
	}
}
