package gateway

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	driver "github.com/go-sql-driver/mysql"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	chatv1 "remarket/backend/gen/chat/v1"
	"remarket/backend/internal/chat/handler"
	"remarket/backend/internal/chat/model"
	chatnode "remarket/backend/internal/chat/node"
	"remarket/backend/internal/chat/repository"
	"remarket/backend/internal/chat/service"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/database"
	"remarket/backend/pkg/grpcx"
	rx "remarket/backend/pkg/redis"
)

// TestChatBrowserLive 启动隔离双 Node、真实聊天 RPC/存储和 Vite，运行真实浏览器，不触碰业务库。
func TestChatBrowserLive(t *testing.T) {
	if os.Getenv("CHAT_BROWSER_CHECK") != "1" {
		t.Skip("需要 CHAT_BROWSER_CHECK=1 和临时 Playwright 环境")
	}
	gin.SetMode(gin.TestMode)
	python := os.Getenv("CHAT_BROWSER_PYTHON")
	if python == "" {
		t.Fatal("需要 CHAT_BROWSER_PYTHON 指向安装 Playwright 的 Python")
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	name := fmt.Sprintf("remarket_chat_browser_test_%d", time.Now().UnixNano())
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
	data, err := os.ReadFile("../../../migrations/chat/000001_chat.up.sql")
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

	store := repository.New(db)
	conversation, err := store.Create(ctx, 10, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "chat-browser-test", Audience: "test", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	rpcServer := grpcx.NewServer(nil, grpcx.AuthInterceptor(tokens))
	chatv1.RegisterChatServiceServer(rpcServer, handler.New(service.New(store, realtimeUsers{}, nil)))
	rpcListener := bufconn.Listen(1 << 20)
	go func() { _ = rpcServer.Serve(rpcListener) }()
	defer rpcServer.Stop()
	defer rpcListener.Close()
	conn, err := grpc.NewClient("passthrough:///chat-browser-test", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return rpcListener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(grpcx.ClientInterceptor(3*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := chatv1.NewChatServiceClient(conn)
	redisConfig, err := rx.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	redis, err := rx.Open(ctx, redisConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer redis.Close()
	prefix := fmt.Sprintf("chat:browser:%d:", time.Now().UnixNano())
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var cursor uint64
		for {
			keys, next, e := redis.Scan(cleanup, cursor, prefix+"*", 100).Result()
			if e != nil {
				t.Error("清理测试 Redis 失败")
				return
			}
			if len(keys) > 0 {
				if e := redis.Del(cleanup, keys...).Err(); e != nil {
					t.Error("清理测试 Redis 失败")
					return
				}
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
	}()
	origin := "http://127.0.0.1:15174"
	router := NewRouter(nil, 3*time.Second)
	hub := AddChatRealtimeRoutes(router, client, realtimeUsers{}, tokens, []string{origin}, 3*time.Second)
	peerRouter := NewRouter(nil, 3*time.Second)
	peerHub := AddChatRealtimeRoutes(peerRouter, client, realtimeUsers{}, tokens, nil, 3*time.Second)
	relayConfig := chatnode.RedisConfig{Prefix: prefix, LeaseTTL: 3 * time.Second, RenewInterval: time.Second}
	if err := hub.EnableRedis(ctx, redis, relayConfig); err != nil {
		t.Fatal(err)
	}
	if err := peerHub.EnableRedis(ctx, redis, relayConfig); err != nil {
		t.Fatal(err)
	}
	AddChatRoutes(router, client, tokens, 3*time.Second, hub)
	product := gin.H{"id": "10", "seller_id": "2", "category_id": "1", "title": "隔离验收机械键盘", "price_cent": "24900", "status": "PRODUCT_STATUS_ON_SALE", "aggregate_version": "1"}
	// 辅助账号与商品只提供固定展示夹具；聊天路径使用真实鉴权 RPC、MySQL 与 Redis。
	router.GET("/api/v1/users/me", bearerAuth(tokens), func(c *gin.Context) {
		c.JSON(200, gin.H{"user": gin.H{"id": "1", "username": "browser", "nickname": "浏览器验收", "role": "USER", "status": "ACTIVE"}})
	})
	router.GET("/api/v1/categories", func(c *gin.Context) {
		c.JSON(200, gin.H{"categories": []gin.H{{"id": "1", "name": "数码电子", "code": "digital"}}})
	})
	router.GET("/api/v1/users/:id/profile", func(c *gin.Context) { c.JSON(200, gin.H{"id": c.Param("id"), "nickname": "测试卖家"}) })
	router.GET("/api/v1/feed/home", func(c *gin.Context) { c.JSON(200, gin.H{"products": []gin.H{product}}) })
	router.GET("/api/v1/products/:id", func(c *gin.Context) { c.JSON(200, gin.H{"product": product}) })
	router.GET("/api/v1/products/:id/images", func(c *gin.Context) { c.JSON(200, gin.H{}) })
	router.POST("/api/v1/__chat_browser_fixture/message", bearerAuth(tokens), func(c *gin.Context) {
		// 直接持久化但不发布通知，验证浏览器持续在线时仍能通过补拉恢复。
		if _, e := store.Send(c.Request.Context(), 2, conversation.ID, "missed-real-browser", "通知遗漏后通过补拉恢复"); e != nil {
			writeRPCError(c, e)
			return
		}
		c.JSON(200, gin.H{})
	})
	server := httptest.NewServer(router)
	peerServer := httptest.NewServer(peerRouter)
	defer server.Close()
	defer peerServer.Close()
	defer hub.Close()
	defer peerHub.Close()
	sellerToken, _, err := tokens.IssueAccessToken(2, "USER")
	if err != nil {
		t.Fatal(err)
	}
	ticket, _, err := peerHub.Issue(sellerToken)
	if err != nil {
		t.Fatal(err)
	}
	seller, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(peerServer.URL, "http")+"/api/v1/chat/ws?ticket="+ticket, http.Header{"Origin": []string{peerServer.URL}})
	if err != nil {
		t.Fatal("卖家连接失败")
	}
	sellerDone := make(chan struct{})
	go func() {
		defer close(sellerDone)
		for {
			var f struct {
				Type string
				Data struct {
					Content  string
					SenderID string `json:"sender_id"`
				}
			}
			if seller.ReadJSON(&f) != nil {
				return
			}
			if f.Type == "chat.message" && f.Data.SenderID == "1" && f.Data.Content == "真实浏览器跨节点消息" {
				_ = seller.SetWriteDeadline(time.Now().Add(3 * time.Second))
				if seller.WriteJSON(gin.H{"type": "chat.send", "request_id": "seller-reply", "data": gin.H{"conversation_id": fmt.Sprint(conversation.ID), "client_msg_id": "seller-real-reply", "content": "收到，明天下午三点见。"}}) != nil {
					return
				}
			}
		}
	}()
	defer func() { seller.Close(); <-sellerDone }()
	deadline := time.Now().Add(3 * time.Second)
	for redis.ZCard(ctx, prefix+"route:2").Val() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("卖家路由未就绪")
		}
		time.Sleep(10 * time.Millisecond)
	}
	frontend, err := filepath.Abs("../../../frontend")
	if err != nil {
		t.Fatal(err)
	}
	var viteLog bytes.Buffer
	vite := exec.CommandContext(ctx, filepath.Join(frontend, "node_modules", ".bin", "vite"), "--host", "127.0.0.1", "--port", "15174", "--strictPort")
	vite.Dir = frontend
	vite.Env = append(os.Environ(), "GATEWAY_PROXY_TARGET="+server.URL)
	vite.Stdout = &viteLog
	vite.Stderr = &viteLog
	if err := vite.Start(); err != nil {
		t.Fatal("启动 Vite 失败")
	}
	defer func() { _ = vite.Process.Kill(); _ = vite.Wait() }()
	// 轮询端口只检查准备状态，不打印环境中的凭据。
	ready := false
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		response, e := http.Get(origin)
		if e == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("Vite 未就绪")
	}
	buyerToken, _, err := tokens.IssueAccessToken(1, "USER")
	if err != nil {
		t.Fatal(err)
	}
	browser := exec.CommandContext(ctx, python, "tests/chat-live-browser.py")
	browser.Dir = frontend
	browser.Env = append(os.Environ(), "CHAT_WEB_ORIGIN="+origin, "CHAT_BROWSER_TOKEN="+buyerToken)
	output, err := browser.CombinedOutput()
	if err != nil {
		t.Fatalf("浏览器验收失败: %s", output)
	}
	t.Log(strings.TrimSpace(string(output)))
	for _, content := range []string{"真实浏览器跨节点消息", "ACK丢失的真实消息", "通知遗漏后通过补拉恢复"} {
		var count int64
		if e := db.Model(&model.Message{}).Where("content = ?", content).Count(&count).Error; e != nil || count != 1 {
			t.Fatal("浏览器恢复产生重复消息", e)
		}
	}
}
