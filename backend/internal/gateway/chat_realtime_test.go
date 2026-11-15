package gateway

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"net/http"
	"net/http/httptest"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/pkg/auth"
	"strings"
	"testing"
	"time"
)

type realtimeUsers struct{ userv1.UserServiceClient }

// GetCurrentUser 检查 HTTP 票据与 WebSocket 握手都传递原凭据。
func (u realtimeUsers) GetCurrentUser(ctx context.Context, _ *userv1.GetCurrentUserRequest, _ ...grpc.CallOption) (*userv1.UserResponse, error) {
	md, _ := metadata.FromOutgoingContext(ctx)
	if len(md.Get("authorization")) != 1 {
		return nil, status.Error(codes.Unauthenticated, "missing credentials")
	}
	return &userv1.UserResponse{}, nil
}

// TestChatTicketsHTTP 验证浏览器票据入口的认证、来源和真实 WebSocket 升级。
func TestChatTicketsHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokens, e := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "test", Audience: "test", AccessTTL: time.Hour})
	if e != nil {
		t.Fatal(e)
	}
	raw, _, e := tokens.IssueAccessToken(1, "USER")
	if e != nil {
		t.Fatal(e)
	}
	router := gin.New()
	hub := AddChatRealtimeRoutes(router, &chatTestClient{}, realtimeUsers{}, tokens, nil, time.Second)
	server := httptest.NewServer(router)
	defer server.Close()
	defer hub.Close()
	for _, tc := range []struct {
		auth   bool
		origin string
		status int
	}{
		{false, server.URL, 401}, {true, "", 403}, {true, "https://evil.example", 403}, {true, server.URL, 200},
	} {
		req, e := http.NewRequest("POST", server.URL+"/api/v1/chat/tickets", nil)
		if e != nil {
			t.Fatal(e)
		}
		if tc.auth {
			req.Header.Set("Authorization", "Bearer "+raw)
		}
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		response, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		if response.StatusCode != tc.status {
			response.Body.Close()
			t.Fatal("票据状态错误", response.StatusCode, tc.status)
		}
		if tc.status != 200 {
			response.Body.Close()
			continue
		}
		if response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("票据允许缓存")
		}
		var body struct {
			Ticket string `json:"ticket"`
		}
		e = json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if e != nil {
			t.Fatal(e)
		}
		conn, _, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/chat/ws?ticket="+body.Ticket, http.Header{"Origin": []string{server.URL}})
		if e != nil {
			t.Fatal(e)
		}
		if e := conn.WriteJSON(map[string]string{"type": "ping", "request_id": "ready"}); e != nil {
			t.Fatal(e)
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		var pong struct {
			Type string `json:"type"`
		}
		if e := conn.ReadJSON(&pong); e != nil || pong.Type != "pong" {
			t.Fatal("WebSocket 未就绪", e)
		}
		conn.Close()
	}
}
