package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/gin-gonic/gin"
	driver "github.com/go-sql-driver/mysql"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	chatv1 "remarket/backend/gen/chat/v1"
	"remarket/backend/internal/chat/handler"
	"remarket/backend/internal/chat/model"
	"remarket/backend/internal/chat/repository"
	"remarket/backend/internal/chat/service"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/database"
	"remarket/backend/pkg/grpcx"
	"strings"
	"testing"
	"time"
)

// TestChatReadHTTPMySQL 验证真实 HTTP→鉴权 gRPC→MySQL 的补拉、未读和已读闭环。
func TestChatReadHTTPMySQL(t *testing.T) {
	gin.SetMode(gin.TestMode)
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
	name := fmt.Sprintf("remarket_chat_read_http_test_%d", time.Now().UnixNano())
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
	for i, user := range []int64{1, 2, 1} {
		if _, e := store.Send(ctx, user, conversation.ID, fmt.Sprintf("initial-%d", i), "hello"); e != nil {
			t.Fatal(e)
		}
	}
	tokens, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "test", Audience: "test", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	rpcServer := grpcx.NewServer(nil, grpcx.AuthInterceptor(tokens))
	chatv1.RegisterChatServiceServer(rpcServer, handler.New(service.New(store, realtimeUsers{}, nil)))
	listener := bufconn.Listen(1 << 20)
	go func() { _ = rpcServer.Serve(listener) }()
	defer rpcServer.Stop()
	defer listener.Close()
	conn, err := grpc.NewClient("passthrough:///chat-read-test", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(grpcx.ClientInterceptor(3*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := chatv1.NewChatServiceClient(conn)
	router := NewRouter(nil, time.Second)
	hub := AddChatRealtimeRoutes(router, client, realtimeUsers{}, tokens, nil, time.Second)
	AddChatRoutes(router, client, tokens, time.Second, hub)
	server := httptest.NewServer(router)
	defer server.Close()
	defer hub.Close()
	path := fmt.Sprintf("/api/v1/conversations/%d", conversation.ID)
	// request 使用真实 JWT 和 HTTP 编解码；所有 int64 响应按 Proto JSON 检查。
	request := func(method, url, body string, user int64, status int, out proto.Message) {
		t.Helper()
		req, e := http.NewRequest(method, server.URL+url, strings.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Content-Type", "application/json")
		if user > 0 {
			raw, _, e := tokens.IssueAccessToken(user, "USER")
			if e != nil {
				t.Fatal(e)
			}
			req.Header.Set("Authorization", "Bearer "+raw)
		}
		response, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		data, e := io.ReadAll(response.Body)
		response.Body.Close()
		if e != nil {
			t.Fatal(e)
		}
		if response.StatusCode != status {
			t.Fatalf("%s status=%d want=%d body=%s", url, response.StatusCode, status, data)
		}
		if out != nil && protojson.Unmarshal(data, out) != nil {
			t.Fatal("Proto JSON 响应无效", string(data))
		}
		if status == 500 && strings.Contains(string(data), "secret") {
			t.Fatal("内部错误泄漏")
		}
	}
	var list chatv1.ListConversationsResponse
	request("GET", "/api/v1/conversations", "", 2, 200, &list)
	if len(list.Conversations) != 1 || list.Conversations[0].UnreadCount != 2 || list.Conversations[0].LastReadSeq != 0 {
		t.Fatal("列表未读统计错误", list.String())
	}
	var first chatv1.ListMessagesResponse
	request("GET", path+"/messages?after_seq=0&page_size=1", "", 2, 200, &first)
	if len(first.Messages) != 1 || first.Messages[0].Seq != 1 || first.SyncUntilSeq != 3 || first.NextSeq != 1 || !first.HasMore {
		t.Fatal("首次补拉错误", first.String())
	}
	if _, e := store.Send(ctx, 1, conversation.ID, "during-sync", "new during pagination"); e != nil {
		t.Fatal(e)
	}
	var second, third, fresh, empty, history chatv1.ListMessagesResponse
	request("GET", path+"/messages?after_seq=1&sync_until_seq=3&page_size=1", "", 2, 200, &second)
	request("GET", path+"/messages?after_seq=2&sync_until_seq=3&page_size=1", "", 2, 200, &third)
	if second.Messages[0].Seq != 2 || !second.HasMore || third.Messages[0].Seq != 3 || third.HasMore {
		t.Fatal("固定上界分页错误")
	}
	request("GET", path+"/messages?after_seq=3", "", 2, 200, &fresh)
	if len(fresh.Messages) != 1 || fresh.Messages[0].Seq != 4 || fresh.SyncUntilSeq != 4 {
		t.Fatal("未恢复新增消息")
	}
	request("GET", path+"/messages?after_seq=4&sync_until_seq=4", "", 2, 200, &empty)
	if len(empty.Messages) != 0 || empty.NextSeq != 4 || empty.HasMore {
		t.Fatal("空页回退游标")
	}
	request("GET", path+"/messages?page_size=1", "", 2, 200, &history)
	if history.Messages[0].Seq != 4 {
		t.Fatal("未传 after_seq 应为倒序历史")
	}
	// 先建立 WebSocket，再通过 HTTP 已读接口检查通知，接收方只能看到公开游标。
	raw, _, err := tokens.IssueAccessToken(1, "USER")
	if err != nil {
		t.Fatal(err)
	}
	ticket, _, err := hub.Issue(raw)
	if err != nil {
		t.Fatal(err)
	}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/chat/ws?ticket="+ticket, http.Header{"Origin": []string{server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err := ws.WriteJSON(map[string]string{"type": "ping", "request_id": "ready"}); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, e := ws.ReadMessage(); e != nil {
		t.Fatal(e)
	}
	readNotification := func() {
		t.Helper()
		_ = ws.SetReadDeadline(time.Now().Add(time.Second))
		_, data, e := ws.ReadMessage()
		if e != nil || !strings.Contains(string(data), `"type":"chat.read"`) || strings.Contains(string(data), "unread_count") {
			t.Fatal("HTTP 已读通知错误", e, string(data))
		}
	}
	var state chatv1.ReadState
	request("POST", path+"/read", `{"last_read_seq":"1"}`, 2, 200, &state)
	readNotification()
	if state.LastReadSeq != 1 || state.UnreadCount != 2 || state.UserId != 2 {
		t.Fatal("已读统计错误", state.String())
	}
	request("POST", path+"/read", `{"last_read_seq":"0"}`, 2, 200, &state)
	readNotification()
	if state.LastReadSeq != 1 {
		t.Fatal("游标回退")
	}
	request("POST", path+"/read", `{"last_read_seq":"4"}`, 2, 200, &state)
	readNotification()
	if state.LastReadSeq != 4 || state.UnreadCount != 0 {
		t.Fatal("全部已读后仍未读")
	}
	request("GET", "/api/v1/conversations", "", 2, 200, &list)
	if list.Conversations[0].UnreadCount != 0 || list.Conversations[0].LastReadSeq != 4 {
		t.Fatal("列表未更新已读")
	}
	request("GET", path+"/messages?after_seq=0", "", 3, 403, nil)
	request("POST", path+"/read", `{"last_read_seq":"1"}`, 3, 403, nil)
	request("POST", path+"/read", `{"last_read_seq":"1"}`, 0, 401, nil)
	request("POST", path+"/read", `{"last_read_seq":"5"}`, 2, 400, nil)
	for _, body := range []string{`{}`, `{"last_read_seq":null}`, `{"last_read_seq":1}`, `{"last_read_seq":"-1"}`, `{"last_read_seq":"9223372036854775808"}`, `{"last_read_seq":"1","user_id":"1"}`} {
		request("POST", path+"/read", body, 2, 400, nil)
	}
	for _, query := range []string{"after_seq=0&before_seq=0", "after_seq=0&after_seq=1", "after_seq=-1", "sync_until_seq=2", "after_seq=0&sync_until_seq=99"} {
		request("GET", path+"/messages?"+query, "", 2, 400, nil)
	}
	if _, e := store.Send(ctx, 1, conversation.ID, "read-failure", "failure"); e != nil {
		t.Fatal(e)
	}
	if _, e := pool.ExecContext(ctx, "CREATE TRIGGER reject_http_read BEFORE UPDATE ON conversation_members FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'secret read failure'"); e != nil {
		t.Fatal(e)
	}
	request("POST", path+"/read", `{"last_read_seq":"5"}`, 2, 500, nil)
	var persisted model.Member
	if e := db.Where("conversation_id = ? AND user_id = ?", conversation.ID, 2).Take(&persisted).Error; e != nil || persisted.LastReadSeq != 4 {
		t.Fatal("失败已读未回滚", e)
	}
}
