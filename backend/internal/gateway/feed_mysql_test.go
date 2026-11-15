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
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	feedv1 "remarket/backend/gen/feed/v1"
	productv1 "remarket/backend/gen/product/v1"
	userv1 "remarket/backend/gen/user/v1"
	fh "remarket/backend/internal/feed/handler"
	fs "remarket/backend/internal/feed/service"
	"remarket/backend/internal/feed/timeline"
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
	"sync/atomic"
	"testing"
	"time"
)

// TestFeedHTTPMySQL 验证三服务隔离、同时间游标、实时关注和状态过滤，使用并清理独立临时库。
func TestFeedHTTPMySQL(t *testing.T) {
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
	name := fmt.Sprintf("remarket_feed_user_%d", time.Now().UnixNano())
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

	productDB := fmt.Sprintf("remarket_feed_%d", time.Now().UnixNano())
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

	tokens, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "test", Audience: "test", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// connect 启动真实 gRPC 服务并保留客户端超时和 Request ID 拦截器。
	connect := func(server *grpc.Server) *grpc.ClientConn {
		l := bufconn.Listen(1 << 20)
		go server.Serve(l)
		t.Cleanup(server.Stop)
		conn, e := grpc.NewClient("passthrough:///feed-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return l.Dial() }), grpc.WithUnaryInterceptor(grpcx.ClientInterceptor(3*time.Second)))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	users := ur.NewUserRepository(db)
	ids := make([]int64, 3)
	for i := range ids {
		u, e := users.Create(context.Background(), ur.CreateUserParams{Username: fmt.Sprintf("feed%d", i), PasswordHash: "test", Nickname: "测试"})
		if e != nil {
			t.Fatal(e)
		}
		ids[i] = u.ID
	}
	userServer := grpcx.NewServer(nil, grpcx.OptionalAuthInterceptor(tokens, []string{userv1.UserService_GetPublicProfile_FullMethodName}))
	userv1.RegisterUserServiceServer(userServer, uh.NewAuthenticated(nil, nil, us.NewCurrentUser(users)).WithFollow(us.NewFollow(users)).WithProfile(us.NewProfile(users)))
	uc := userv1.NewUserServiceClient(connect(userServer))
	productAuth, err := ph.NewAuthInterceptor(tokens, "", strings.Repeat("s", 32))
	if err != nil {
		t.Fatal(err)
	}
	var candidateCalls atomic.Int64
	productServer := grpcx.NewServer(nil, productAuth, func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if r, ok := req.(*productv1.ListFeedProductsRequest); ok && len(r.CandidateIds) > 0 {
			candidateCalls.Add(1)
		}
		return h(ctx, req)
	})
	productv1.RegisterProductServiceServer(productServer, ph.New(nil).WithFeed(ps.NewFeedQuery(pr.NewProductRepository(productGorm))))
	pc := productv1.NewProductServiceClient(connect(productServer))
	feedServer := grpcx.NewServer(nil, grpcx.OptionalAuthInterceptor(tokens, []string{feedv1.FeedService_Home_FullMethodName}))
	query := fs.New(uc, pc)
	var cacheStore *timeline.Store
	var breakCache func()
	if os.Getenv("FEED_INTEGRATION") == "1" {
		cfg, e := timeline.LoadConfig()
		if e != nil {
			t.Fatal(e)
		}
		cfg.Prefix = fmt.Sprintf("feed:test:http:%d:", time.Now().UnixNano())
		store, client, e := timeline.OpenLazy(cfg)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			keys, e := client.Keys(context.Background(), cfg.Prefix+"*").Result()
			if e != nil {
				t.Error(e)
			} else if len(keys) > 0 {
				if e := client.Del(context.Background(), keys...).Err(); e != nil {
					t.Error(e)
				}
			}
			client.Close()
		})
		cacheStore = store
		breakCache = func() {
			dead, closedClient, e := timeline.OpenLazy(cfg)
			if e != nil {
				t.Fatal(e)
			}
			closedClient.Close()
			query.WithTimeline(dead)
		}
		query.WithTimeline(store)
	}
	feedv1.RegisterFeedServiceServer(feedServer, fh.New(query))
	fc := feedv1.NewFeedServiceClient(connect(feedServer))
	router := NewRouter(uc, time.Second)
	AddFeedRoutes(router, fc, tokens, 3*time.Second)
	AddFollowRoutes(router, uc, tokens, time.Second)
	AddProfileRoutes(router, uc, pc, tokens, time.Second)
	raw, _, err := tokens.IssueAccessToken(ids[0], "USER")
	if err != nil {
		t.Fatal(err)
	}
	// call 使用 HTTP 路由发起请求，令牌仅用于测试，不写入日志。
	call := func(method, path, token string, want int) map[string]any {
		req := httptest.NewRequest(method, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("x-user-id", fmt.Sprint(ids[1]))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s status=%d want=%d body=%s", method, path, w.Code, want, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// 同一微秒内插入多条商品，主键必须稳定打破时间平局；非在售状态均不得展示。
	for i, state := range []string{"ON_SALE", "ON_SALE", "ON_SALE", "DRAFT", "OFF_SHELF", "SOLD", "RESERVED"} {
		seller := ids[1]
		if i == 2 {
			seller = ids[2]
		}
		e := productGorm.Exec("INSERT INTO products (seller_id,category_id,title,description,price_cent,status,first_published_at,created_at) VALUES (?,1,?,'test',100,?,'2026-01-01','2026-01-01')", seller, fmt.Sprint(i), state).Error
		if e != nil {
			t.Fatal(e)
		}
	}
	home := call("GET", "/api/v1/feed/home", "", 200)
	if len(home["products"].([]any)) != 3 {
		t.Fatal("首页状态过滤错误")
	}
	call("GET", "/api/v1/feed/following", "", 401)
	empty := call("GET", "/api/v1/feed/following", raw, 200)
	if len(empty) != 0 {
		t.Fatal("未关注应为空")
	}
	followPath := fmt.Sprintf("/api/v1/users/%d/follow", ids[1])
	call("POST", followPath, raw, 200)
	anonymous := call("GET", fmt.Sprintf("/api/v1/users/%d/profile", ids[1]), "", 200)
	if anonymous["follower_count"] != "1" || anonymous["is_following"] == true || anonymous["username"] != nil {
		t.Fatal("公开资料或匿名关系错误")
	}
	followed := call("GET", fmt.Sprintf("/api/v1/users/%d/follow", ids[1]), raw, 200)
	if followed["is_following"] != true {
		t.Fatal("关注状态未同步")
	}
	sellerPage := call("GET", fmt.Sprintf("/api/v1/users/%d/products", ids[1]), "", 200)
	if len(sellerPage["products"].([]any)) != 2 {
		t.Fatal("卖家公开商品范围错误")
	}
	for _, item := range sellerPage["products"].([]any) {
		if item.(map[string]any)["seller_id"] != fmt.Sprint(ids[1]) {
			t.Fatal("卖家主页混入他人商品")
		}
	}
	first := call("GET", "/api/v1/feed/following?page_size=1", raw, 200)
	if os.Getenv("FEED_INTEGRATION") == "1" {
		warm := call("GET", "/api/v1/feed/following?page_size=1", raw, 200)
		if len(warm["products"].([]any)) != 1 || candidateCalls.Load() == 0 {
			t.Fatal("未进入 Redis 候选复核路径")
		}
	}
	if breakCache != nil {
		breakCache()
		fallback := call("GET", "/api/v1/feed/following?page_size=1", raw, 200)
		if fallback["products"].([]any)[0].(map[string]any)["id"] != first["products"].([]any)[0].(map[string]any)["id"] {
			t.Fatal("Redis 不可用回退结果错误")
		}
		query.WithTimeline(cacheStore)
	}
	cursor := first["next_cursor"].(string)
	second := call("GET", "/api/v1/feed/following?page_size=1&cursor="+url.QueryEscape(cursor), raw, 200)
	a := first["products"].([]any)[0].(map[string]any)
	b := second["products"].([]any)[0].(map[string]any)
	if a["id"] == b["id"] || a["seller_id"] != fmt.Sprint(ids[1]) || b["seller_id"] != fmt.Sprint(ids[1]) {
		t.Fatal("关注分页错误")
	}
	call("GET", "/api/v1/feed/home?cursor=invalid", "", 400)
	call("GET", "/api/v1/feed/home?page_size=101", "", 400)
	call("DELETE", followPath, raw, 200)
	unfollowed := call("GET", fmt.Sprintf("/api/v1/users/%d/follow", ids[1]), raw, 200)
	if unfollowed["is_following"] == true || unfollowed["follower_count"] != nil {
		t.Fatal("取消后的状态或粉丝数错误")
	}

	empty = call("GET", "/api/v1/feed/following", raw, 200)
	if len(empty) != 0 {
		t.Fatal("取消后仍显示")
	}
	if e := productGorm.Exec("UPDATE products SET status='OFF_SHELF' WHERE status='ON_SALE'").Error; e != nil {
		t.Fatal(e)
	}
	empty = call("GET", "/api/v1/feed/home", "", 200)
	if len(empty) != 0 {
		t.Fatal("下架后仍显示")
	}
}
