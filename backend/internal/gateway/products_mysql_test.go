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
	productv1 "remarket/backend/gen/product/v1"
	userv1 "remarket/backend/gen/user/v1"
	productclient "remarket/backend/internal/product/client"
	ph "remarket/backend/internal/product/handler"
	pr "remarket/backend/internal/product/repository"
	ps "remarket/backend/internal/product/service"
	uh "remarket/backend/internal/user/handler"
	ur "remarket/backend/internal/user/repository"
	us "remarket/backend/internal/user/service"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/database"
	"remarket/backend/pkg/grpcx"
	"strings"
	"testing"
	"time"
)

// TestProductHTTPMySQL 验证注册登录、商品生命周期、双服务鉴权及 Outbox 的完整 HTTP 链路。
func TestProductHTTPMySQL(t *testing.T) {
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
	name := fmt.Sprintf("remarket_product_http_user_%d", time.Now().UnixNano())
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

	productDB := fmt.Sprintf("remarket_product_http_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + productDB + " CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec("DROP DATABASE " + productDB); err != nil {
			t.Error(err)
		}
	}()
	productGorm, productPool, err := database.OpenMySQL(context.Background(), database.MySQLConfig{Address: cfg.Addr, Username: cfg.User, Password: cfg.Passwd, Database: productDB})
	if err != nil {
		t.Fatal(err)
	}
	defer productPool.Close()
	for _, path := range []string{"../../../migrations/product/000001_core.up.sql", "../../../migrations/product/000002_seed_categories.up.sql"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				lines = append(lines, line)
			}
		}
		for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
			if strings.TrimSpace(statement) != "" {
				if _, err := productPool.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	config := auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "remarket", Audience: "api", AccessTTL: time.Hour}
	tokens, err := auth.NewJWTManager(config)
	if err != nil {
		t.Fatal(err)
	}
	// 每个服务独立验签，Product 的管理员核实实际调用 User gRPC。
	userServer := grpcx.NewServer(nil, grpcx.AuthInterceptor(tokens, userv1.UserService_Register_FullMethodName, userv1.UserService_Login_FullMethodName))
	users := ur.NewUserRepository(db)
	login, err := us.NewLogin(users, tokens)
	if err != nil {
		t.Fatal(err)
	}
	userv1.RegisterUserServiceServer(userServer, uh.NewAuthenticated(us.New(users), login, us.NewCurrentUser(users)))
	userListener := bufconn.Listen(1 << 20)
	go userServer.Serve(userListener)
	defer userServer.Stop()
	userConn, err := grpc.NewClient("passthrough:///user-http", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return userListener.Dial() }), grpc.WithUnaryInterceptor(grpcx.ClientInterceptor(3*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	defer userConn.Close()
	userClient := userv1.NewUserServiceClient(userConn)
	products := pr.NewProductRepository(productGorm)
	admins := productclient.NewUserVerifier(userClient)
	productServer := grpcx.NewServer(nil, grpcx.OptionalAuthInterceptor(tokens, []string{productv1.ProductService_GetProduct_FullMethodName, productv1.ProductService_ListProducts_FullMethodName, productv1.ProductService_ListCategories_FullMethodName}))
	productv1.RegisterProductServiceServer(productServer, ph.NewComplete(ps.New(products), ps.NewPublisher(products), ps.NewQuery(products, admins), ps.NewMutation(products, admins)).WithCategories(ps.NewCategories(products)).WithOwnList(ps.NewQuery(products, admins)))
	productListener := bufconn.Listen(1 << 20)
	go productServer.Serve(productListener)
	defer productServer.Stop()
	productConn, err := grpc.NewClient("passthrough:///product-http", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return productListener.Dial() }), grpc.WithUnaryInterceptor(grpcx.ClientInterceptor(3*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	defer productConn.Close()
	gin.SetMode(gin.TestMode)
	router := NewAuthenticatedRouter(userClient, userClient, tokens, 3*time.Second)
	AddProductRoutes(router, productv1.NewProductServiceClient(productConn), tokens, 3*time.Second)
	httpServer := httptest.NewServer(router)
	defer httpServer.Close()
	call := func(method, path, body, token string, want int) map[string]json.RawMessage {
		t.Helper()
		req, err := http.NewRequest(method, httpServer.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-ID", "product-e2e")
		req.Header.Set("X-User-ID", "999")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := httpServer.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var result map[string]json.RawMessage
		if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, result["error"])
		}
		if res.Header.Get("X-Request-ID") != "product-e2e" {
			t.Fatal("请求标识丢失")
		}
		return result
	}
	call("POST", "/api/v1/auth/register", `{"username":"seller","password":"password123","nickname":"卖家"}`, "", 201)
	loginResult := call("POST", "/api/v1/auth/login", `{"username":"seller","password":"password123"}`, "", 200)
	var sellerToken string
	json.Unmarshal(loginResult["access_token"], &sellerToken)
	call("POST", "/api/v1/auth/register", `{"username":"manager","password":"password123","nickname":"管理员"}`, "", 201)
	if _, err := pool.Exec("UPDATE users SET role='ADMIN' WHERE username='manager'"); err != nil {
		t.Fatal(err)
	}
	loginResult = call("POST", "/api/v1/auth/login", `{"username":"manager","password":"password123"}`, "", 200)
	var adminToken string
	json.Unmarshal(loginResult["access_token"], &adminToken)
	call("GET", "/api/v1/categories", "", "", 200)
	input := `{"category_id":"1","title":"键盘","price_cent":"15000"}`
	call("POST", "/api/v1/products", input, "", 401)
	created := call("POST", "/api/v1/products", input, sellerToken, 201)
	var product map[string]any
	json.Unmarshal(created["product"], &product)
	id := product["id"].(string)
	path := "/api/v1/products/" + id
	call("GET", "/api/v1/products/mine", "", "", 401)
	mine := call("GET", "/api/v1/products/mine?page=1&page_size=1", "", sellerToken, 200)
	var total string
	json.Unmarshal(mine["total"], &total)
	if total != "1" {
		t.Fatal("草稿未在我的商品中")
	}
	another := call("GET", "/api/v1/products/mine?seller_id=999", "", adminToken, 200)
	var others []json.RawMessage
	json.Unmarshal(another["products"], &others)
	if len(others) != 0 {
		t.Fatal("管理员越权读取其他卖家列表")
	}
	call("POST", "/api/v1/products", input, sellerToken, 201)
	mine = call("GET", "/api/v1/products/mine?page=2&page_size=1", "", sellerToken, 200)
	var items []json.RawMessage
	json.Unmarshal(mine["products"], &items)
	json.Unmarshal(mine["total"], &total)
	if total != "2" || len(items) != 1 {
		t.Fatal("我的商品分页总数错误")
	}
	call("GET", "/api/v1/products/mine?page_size=101", "", sellerToken, 400)

	call("GET", path, "", "", 404)
	call("GET", path, "", sellerToken, 200)
	call("POST", path+"/publish", `{"expected_version":"1"}`, sellerToken, 200)
	call("GET", "/api/v1/products?page=1&page_size=1&category_id=1", "", "", 200)
	call("GET", path, "", "", 200)
	call("PATCH", path, `{"expected_version":"2","price_cent":"16000"}`, sellerToken, 200)
	call("PATCH", path, `{"expected_version":"2","price_cent":"17000"}`, sellerToken, 409)
	call("POST", path+"/off-shelf", `{"expected_version":"3"}`, adminToken, 200)
	call("GET", path, "", "", 404)
	call("POST", path+"/publish", `{"expected_version":"4"}`, sellerToken, 200)
	// 管理员降权后旧 Token 不能授权下架；Product 实际向 User 查询当前角色。
	if _, err := pool.Exec("UPDATE users SET role='USER' WHERE username='manager'"); err != nil {
		t.Fatal(err)
	}
	call("POST", path+"/off-shelf", `{"expected_version":"5"}`, adminToken, 403)
	call("POST", path+"/off-shelf", `{"expected_version":"5"}`, sellerToken, 200)
	call("GET", path, "", "bad-token", 401)
	call("GET", "/api/v1/products?page=abc", "", "", 400)
	call("GET", "/api/v1/products?page=1&page=2", "", "", 400)
	call("GET", "/api/v1/products/not-id", "", "", 400)
	call("PATCH", path, `{"expected_version":"6","seller_id":"999"}`, sellerToken, 400)
	var count int
	if err := productPool.QueryRow("SELECT COUNT(*) FROM outbox_events").Scan(&count); err != nil || count != 5 {
		t.Fatalf("事件计数错误: %d %v", count, err)
	}
}
