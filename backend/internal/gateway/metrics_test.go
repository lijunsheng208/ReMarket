package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	chatv1 "remarket/backend/gen/chat/v1"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/observability/metrics"
)

type observedChats struct{ chatv1.ChatServiceClient }

// SendMessage 为指标测试提供固定 RPC 结果，客户端重试计入尝试次数，不声称真实落库。
func (c observedChats) SendMessage(ctx context.Context, r *chatv1.SendMessageRequest, _ ...grpc.CallOption) (*chatv1.SendMessageResponse, error) {
	if r.Content == "fail" {
		return nil, status.Error(codes.Unavailable, "暂不可用")
	}
	return &chatv1.SendMessageResponse{Message: &chatv1.Message{Id: 9007199254740993, ConversationId: r.ConversationId, SenderId: 1, ClientMsgId: r.ClientMsgId, Seq: 1, Content: r.Content}, ParticipantIds: []int64{1, 2}}, nil
}

// TestGatewayLiveMetrics 验证真实 Gin/WS 连接生命周期指标，长连接不计入普通 HTTP 延迟。
func TestGatewayLiveMetrics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	monitor, e := metrics.New("gateway")
	if e != nil {
		t.Fatal(e)
	}
	tokens, e := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "metrics-test", Audience: "test", AccessTTL: time.Hour})
	if e != nil {
		t.Fatal(e)
	}
	router := NewRouter(nil, time.Second, monitor.HTTPMiddleware())
	router.GET("/metrics", gin.WrapH(monitor.Handler()))
	hub := AddChatRealtimeRoutes(router, observedChats{}, realtimeUsers{}, tokens, nil, time.Second, monitor)
	server := httptest.NewServer(router)
	defer server.Close()
	defer hub.Close()
	raw, _, e := tokens.IssueAccessToken(1, "USER")
	if e != nil {
		t.Fatal(e)
	}
	ticket, _, e := hub.Issue(raw)
	if e != nil {
		t.Fatal(e)
	}
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/chat/ws?ticket=" + ticket
	conn, _, e := websocket.DefaultDialer.Dial(endpoint, http.Header{"Origin": []string{server.URL}})
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	// exchange 通过实际 WS 协议等待 Node 处理，确保 Gauge 不是测试直接调用造成的。
	exchange := func(value any, kind string) {
		t.Helper()
		if e := conn.WriteJSON(value); e != nil {
			t.Fatal(e)
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		var frame struct{ Type string }
		if e := conn.ReadJSON(&frame); e != nil || frame.Type != kind {
			t.Fatal("WS 事件错误", frame.Type, e)
		}
	}
	exchange(map[string]string{"type": "ping", "request_id": "ready"}, "pong")
	// scrape 从真实管理路由读取文本，并及时关闭采集连接。
	scrape := func() string {
		t.Helper()
		response, e := http.Get(server.URL + "/metrics")
		if e != nil {
			t.Fatal(e)
		}
		data, e := io.ReadAll(response.Body)
		response.Body.Close()
		if e != nil || response.StatusCode != 200 {
			t.Fatal("采集失败", e)
		}
		return string(data)
	}
	if !strings.Contains(scrape(), `websocket_connections{service="gateway"} 1`) {
		t.Fatal("真实在线连接未计入指标")
	}
	for _, content := range []string{"hello", "hello", "fail"} {
		kind := "chat.ack"
		if content == "fail" {
			kind = "chat.error"
		}
		exchange(map[string]any{"type": "chat.send", "request_id": "send-1", "data": map[string]string{"conversation_id": "10", "client_msg_id": "stable-key", "content": content}}, kind)
	}
	_, rejected, e := websocket.DefaultDialer.Dial(endpoint, http.Header{"Origin": []string{"https://evil.example"}})
	if e == nil || rejected == nil || rejected.StatusCode != 403 {
		t.Fatal("未拒绝错误来源")
	}
	hub.Close()
	body := scrape()
	for _, sample := range []string{
		`websocket_connections{service="gateway"} 0`,
		`websocket_handshakes_total{result="accepted",service="gateway"} 1`,
		`websocket_handshakes_total{result="rejected",service="gateway"} 1`,
		`chat_send_rpc_total{code="OK",service="gateway"} 2`,
		`chat_send_rpc_total{code="Unavailable",service="gateway"} 1`,
	} {
		if !strings.Contains(body, sample) {
			t.Fatal("指标缺失", sample)
		}
	}
	for _, forbidden := range []string{raw, ticket, "stable-key", "9007199254740993", `route="/api/v1/chat/ws"`, `route="/metrics"`} {
		if strings.Contains(body, forbidden) {
			t.Fatal("指标污染敏感字段或长期连接 HTTP 延迟")
		}
	}
}
