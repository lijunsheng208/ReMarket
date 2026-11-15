package node

import (
	"context"
	"database/sql"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"net"
	"net/http/httptest"
	"os"
	chatv1 "remarket/backend/gen/chat/v1"
	"remarket/backend/internal/chat/handler"
	"remarket/backend/internal/chat/model"
	"remarket/backend/internal/chat/repository"
	"remarket/backend/internal/chat/service"
	"remarket/backend/pkg/database"
	"remarket/backend/pkg/grpcx"
	"strings"
	"testing"
	"time"
)

// TestWebSocketMySQL 验证真实 WebSocket、独立验签 Chat RPC 和临时 MySQL 的发送恢复闭环。
func TestWebSocketMySQL(t *testing.T) {
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
	name := fmt.Sprintf("remarket_chat_ws_test_%d", time.Now().UnixNano())
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
	data, err := os.ReadFile("../../../../migrations/chat/000001_chat.up.sql")
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
	conversation, e := store.Create(ctx, 10, 1, 2)
	if e != nil {
		t.Fatal(e)
	}
	tokens := testTokens(t)
	rpcServer := grpcx.NewServer(nil, grpcx.AuthInterceptor(tokens))
	chatv1.RegisterChatServiceServer(rpcServer, handler.New(service.New(store, testUsers{}, nil)))
	listener := bufconn.Listen(1 << 20)
	go func() { _ = rpcServer.Serve(listener) }()
	defer rpcServer.Stop()
	defer listener.Close()
	// 使用真实 gRPC 编解码和服务端验签；User 状态 RPC 在本测试中模拟。
	conn, e := grpc.NewClient("passthrough:///chat-test", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(grpcx.ClientInterceptor(3*time.Second)))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	n := New(chatv1.NewChatServiceClient(conn), testUsers{}, tokens, nil)
	server := httptest.NewServer(n)
	defer server.Close()
	defer n.Close()
	sender, _ := dialPeer(t, n, server.URL, tokens, 1)
	receiver, _ := dialPeer(t, n, server.URL, tokens, 2)
	intruder, _ := dialPeer(t, n, server.URL, tokens, 3)
	// send 发送固定幂等键，重连时仍复用原键和正文。
	send := func(c *websocket.Conn, key, content string) {
		t.Helper()
		if e := c.WriteJSON(map[string]any{"type": "chat.send", "request_id": "req-1", "data": map[string]string{"conversation_id": fmt.Sprint(conversation.ID), "client_msg_id": key, "content": content}}); e != nil {
			t.Fatal(e)
		}
	}
	send(sender, "persistent-key", "hello")
	ack := readFrame(t, sender)
	if ack.Type != "chat.ack" {
		t.Fatal("未返回 ACK", ack)
	}
	push := readFrame(t, receiver)
	if push.Type != "chat.message" {
		t.Fatal("接收方未收到消息")
	}
	var count int64
	if e := db.Model(&model.Message{}).Count(&count).Error; e != nil || count != 1 {
		t.Fatal("ACK 前未持久化", e, count)
	}
	sender.Close()
	sender, _ = dialPeer(t, n, server.URL, tokens, 1)
	send(sender, "persistent-key", "hello")
	retry := readFrame(t, sender)
	if string(ack.Data) != string(retry.Data) {
		t.Fatal("重连重试结果变化")
	}
	_ = readFrame(t, receiver)
	send(sender, "persistent-key", "different")
	if f := readFrame(t, sender); f.Type != "chat.error" || !strings.Contains(string(f.Data), "AlreadyExists") {
		t.Fatal("参数冲突未拒绝", f)
	}
	send(intruder, "intruder-key", "hello")
	if f := readFrame(t, intruder); f.Type != "chat.error" || !strings.Contains(string(f.Data), "PermissionDenied") {
		t.Fatal("非成员未拒绝", f)
	}
	// 不读取 ACK 就断开；通过接收方推送确认提交后重连重试。
	send(sender, "ack-lost", "saved without reading ACK")
	_ = readFrame(t, receiver)
	sender.Close()
	sender, _ = dialPeer(t, n, server.URL, tokens, 1)
	send(sender, "ack-lost", "saved without reading ACK")
	if f := readFrame(t, sender); f.Type != "chat.ack" || !strings.Contains(string(f.Data), `"seq":"2"`) {
		t.Fatal("ACK 丢失重试错误", f)
	}
	_ = readFrame(t, receiver)
	receiver.Close()
	send(sender, "offline", "offline message")
	if f := readFrame(t, sender); f.Type != "chat.ack" {
		t.Fatal("离线发送未成功")
	}
	messages, e := store.Messages(ctx, 2, conversation.ID, 0, 2, 20)
	if e != nil || len(messages) != 1 || messages[0].Content != "offline message" {
		t.Fatal("离线消息不可查询", e)
	}
	if e := db.Model(&model.Message{}).Count(&count).Error; e != nil || count != 3 {
		t.Fatal("重试重复入库", e, count)
	}
	// 持久化失败只返回 Internal，不得发 ACK 或泄漏 SQL。
	if _, e := pool.ExecContext(ctx, "CREATE TRIGGER reject_ws_update BEFORE UPDATE ON conversations FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'secret sql failure'"); e != nil {
		t.Fatal(e)
	}
	send(sender, "fail-key", "must rollback")
	failure := readFrame(t, sender)
	if failure.Type != "chat.error" || !strings.Contains(string(failure.Data), "Internal") || strings.Contains(string(failure.Data), "secret") {
		t.Fatal("错误映射或 ACK 边界错误", failure)
	}
	if e := db.Model(&model.Message{}).Count(&count).Error; e != nil || count != 3 {
		t.Fatal("失败消息未回滚", e, count)
	}

}
