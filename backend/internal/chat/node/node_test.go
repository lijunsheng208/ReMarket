package node

import (
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"net"
	"net/http"
	"net/http/httptest"
	chatv1 "remarket/backend/gen/chat/v1"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/pkg/auth"
	"strings"
	"sync"
	"testing"
	"time"
)

type testUsers struct {
	userv1.UserServiceClient
	disabled bool
}

// GetCurrentUser 模拟账号核实；禁用账号不能通过握手。
func (u testUsers) GetCurrentUser(context.Context, *userv1.GetCurrentUserRequest, ...grpc.CallOption) (*userv1.UserResponse, error) {
	if u.disabled {
		return nil, status.Error(codes.Unauthenticated, "disabled")
	}
	return &userv1.UserResponse{}, nil
}

type testChat struct {
	chatv1.ChatServiceClient
	mu    sync.Mutex
	calls int
}

// SendMessage 模拟已保存的幂等结果，验证 ACK 与多连接分发。
func (c *testChat) SendMessage(context.Context, *chatv1.SendMessageRequest, ...grpc.CallOption) (*chatv1.SendMessageResponse, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return &chatv1.SendMessageResponse{Message: &chatv1.Message{Id: 9007199254740993, ConversationId: 10, SenderId: 1, ClientMsgId: "key", Seq: 1, Content: "hello"}, ParticipantIds: []int64{1, 2}}, nil
}

// testTokens 创建用于真实 WebSocket 握手的签发与验签组件。
func testTokens(t *testing.T) *auth.JWTManager {
	t.Helper()
	tokens, e := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "test", Audience: "test", AccessTTL: time.Hour})
	if e != nil {
		t.Fatal(e)
	}
	return tokens
}

// dialPeer 使用一次性票据建立连接，并通过 ping 确认连接已注册。
func dialPeer(t *testing.T, n *Node, serverURL string, tokens *auth.JWTManager, user int64) (*websocket.Conn, string) {
	t.Helper()
	raw, _, e := tokens.IssueAccessToken(user, "USER")
	if e != nil {
		t.Fatal(e)
	}
	ticket, _, e := n.Issue(raw)
	if e != nil {
		t.Fatal(e)
	}
	headers := http.Header{"Origin": []string{serverURL}}
	conn, _, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(serverURL, "http")+"?ticket="+ticket, headers)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { conn.Close() })
	if e := conn.WriteJSON(map[string]any{"type": "ping", "request_id": "ready"}); e != nil {
		t.Fatal(e)
	}
	if f := readFrame(t, conn); f.Type != "pong" {
		t.Fatal("连接未就绪", f)
	}
	return conn, ticket
}

// readFrame 为测试读取设置上限，避免失败时永久等待。
func readFrame(t *testing.T, c *websocket.Conn) frame {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var f frame
	if e := c.ReadJSON(&f); e != nil {
		t.Fatal(e)
	}
	return f
}

// TestNodeRealtime 验证 ACK、多标签页推送、票据重放拒绝、错误格式与关闭释放。
func TestNodeRealtime(t *testing.T) {
	tokens := testTokens(t)
	client := &testChat{}
	n := New(client, testUsers{}, tokens, nil)
	server := httptest.NewServer(n)
	defer server.Close()
	defer n.Close()
	sender, ticket := dialPeer(t, n, server.URL, tokens, 1)
	other, _ := dialPeer(t, n, server.URL, tokens, 1)
	receiver, _ := dialPeer(t, n, server.URL, tokens, 2)
	intruder, _ := dialPeer(t, n, server.URL, tokens, 3)
	if c, response, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"?ticket="+ticket, http.Header{"Origin": []string{server.URL}}); e == nil {
		c.Close()
		t.Fatal("票据可重放")
	} else if response == nil || response.StatusCode != 401 {
		t.Fatal("票据重放状态错误")
	}
	if e := sender.WriteJSON(map[string]any{"type": "chat.send", "request_id": "send-1", "data": map[string]string{"conversation_id": "10", "client_msg_id": "key", "content": "hello"}}); e != nil {
		t.Fatal(e)
	}
	ack := readFrame(t, sender)
	if ack.Type != "chat.ack" || !strings.Contains(string(ack.Data), `"9007199254740993"`) {
		t.Fatal("ACK 或精度错误", ack)
	}
	for _, c := range []*websocket.Conn{other, receiver} {
		f := readFrame(t, c)
		if f.Type != "chat.message" || !strings.Contains(string(f.Data), `"message_id":"9007199254740993"`) {
			t.Fatal("在线推送错误", f)
		}
	}
	// 非参与者连接应只收到自己请求的 pong，不能收到聊天推送。
	if e := intruder.WriteJSON(map[string]string{"type": "ping", "request_id": "check"}); e != nil {
		t.Fatal(e)
	}
	if f := readFrame(t, intruder); f.Type != "pong" {
		t.Fatal("消息泄漏给非参与者")
	}
	if e := sender.WriteMessage(websocket.TextMessage, []byte(`{"type":"chat.send","request_id":"bad","data":{"conversation_id":"10","sender_id":"99"}}`)); e != nil {
		t.Fatal(e)
	}
	if f := readFrame(t, sender); f.Type != "chat.error" {
		t.Fatal("未知字段被接受")
	}
	n.Close()
	_ = sender.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, e := sender.ReadMessage(); e == nil {
		t.Fatal("关闭未释放连接")
	}
	if _, _, e := n.Issue("invalid"); e == nil {
		t.Fatal("无效凭据取得票据")
	}
}

// TestOriginAndTicketExpiry 验证来源白名单、过期票据和禁用账号握手拒绝。
func TestOriginAndTicketExpiry(t *testing.T) {
	tokens := testTokens(t)
	n := New(&testChat{}, testUsers{disabled: true}, tokens, []string{"https://frontend.example"})
	defer n.Close()
	for _, tc := range []struct {
		origin  string
		allowed bool
	}{{"", false}, {"null", false}, {"https://evil.example", false}, {"https://frontend.example", true}, {"http://node.example", true}, {"http://node.example/path", false}} {
		req := httptest.NewRequest("GET", "http://node.example/", nil)
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if n.OriginAllowed(req) != tc.allowed {
			t.Fatal("来源判断错误", tc.origin)
		}
	}
	raw, _, e := tokens.IssueAccessToken(1, "USER")
	if e != nil {
		t.Fatal(e)
	}
	value, _, e := n.Issue(raw)
	if e != nil {
		t.Fatal(e)
	}
	n.mu.Lock()
	for key, item := range n.tickets {
		item.expires = time.Now().Add(-time.Second)
		n.tickets[key] = item
	}
	n.mu.Unlock()
	if _, ok := n.take(value); ok {
		t.Fatal("过期票据被接受")
	}
	server := httptest.NewServer(n)
	defer server.Close()
	value, _, e = n.Issue(raw)
	if e != nil {
		t.Fatal(e)
	}
	_, response, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"?ticket="+value, http.Header{"Origin": []string{server.URL}})
	if e == nil || response == nil || response.StatusCode != 401 {
		t.Fatal("禁用账号通过握手")
	}
}

// TestQueueLimit 验证慢连接队列溢出会关闭连接而不会阻塞推送。
func TestQueueLimit(t *testing.T) {
	tokens := testTokens(t)
	n := New(&testChat{}, testUsers{}, tokens, nil)
	defer n.Close()
	server := httptest.NewServer(n)
	defer server.Close()
	conn, _ := dialPeer(t, n, server.URL, tokens, 1)
	p := &peer{ws: conn, queue: make(chan []byte, 1)}
	n.enqueue(p, []byte("one"))
	n.enqueue(p, []byte("two"))
	if e := conn.WriteMessage(websocket.TextMessage, []byte("{}")); e == nil {
		t.Fatal("队列溢出后仍可写")
	}
}

// TestStrictJSON 拒绝多段消息和未知字段。
func TestStrictJSON(t *testing.T) {
	var f frame
	for _, data := range []string{`{"type":"ping","extra":1}`, `{"type":"ping"}{}`, `[]`} {
		if strictJSON([]byte(data), &f) == nil {
			t.Fatal("非法 JSON 被接受")
		}
	}
	if e := json.Unmarshal([]byte(`{}`), &f); e != nil {
		t.Fatal(e)
	}
}

// TestHeartbeatTimeout 验证客户端不回应控制帧心跳时服务端主动释放连接。
func TestHeartbeatTimeout(t *testing.T) {
	tokens := testTokens(t)
	n := New(&testChat{}, testUsers{}, tokens, nil)
	n.pingInterval = 20 * time.Millisecond
	n.pongWait = 100 * time.Millisecond
	server := httptest.NewServer(n)
	defer server.Close()
	defer n.Close()
	conn, _ := dialPeer(t, n, server.URL, tokens, 1)
	conn.SetPingHandler(func(string) error { return nil })
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, e := conn.ReadMessage(); e == nil {
		t.Fatal("无心跳仍保留连接")
	} else if timeout, ok := e.(net.Error); ok && timeout.Timeout() {
		t.Fatal("服务端未因心跳超时关闭")
	}

}

// TestTokenExpiry 验证空闲长连接也会在 JWT 到期后关闭。
func TestTokenExpiry(t *testing.T) {
	tokens, e := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "test", Audience: "test", AccessTTL: 2 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	n := New(&testChat{}, testUsers{}, tokens, nil)
	server := httptest.NewServer(n)
	defer server.Close()
	defer n.Close()
	conn, _ := dialPeer(t, n, server.URL, tokens, 1)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, e := conn.ReadMessage(); e == nil {
		t.Fatal("JWT 到期后仍保留连接")
	} else if timeout, ok := e.(net.Error); ok && timeout.Timeout() {
		t.Fatal("服务端未在 JWT 到期时关闭")
	}

}
