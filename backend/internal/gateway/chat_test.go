package gateway

import (
	"context"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"net/http/httptest"
	chatv1 "remarket/backend/gen/chat/v1"
	"remarket/backend/pkg/auth"
	"strings"
	"testing"
	"time"
)

type chatTestClient struct {
	chatv1.ChatServiceClient
	calls int
}

// CreateConversation 模拟 RPC 返回大整数，验证 HTTP 不损失精度。
func (c *chatTestClient) CreateConversation(ctx context.Context, r *chatv1.CreateConversationRequest, _ ...grpc.CallOption) (*chatv1.Conversation, error) {
	c.calls++
	return &chatv1.Conversation{Id: 9007199254740993, ProductId: r.ProductId}, nil
}

// TestChatHTTP 验证鉴权、严格参数校验和大整数序列化。
func TestChatHTTP(t *testing.T) {
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
	client := &chatTestClient{}
	AddChatRoutes(router, client, tokens, time.Second)
	cases := []struct {
		method, url, body string
		authenticated     bool
		status            int
	}{
		{"POST", "/api/v1/conversations", `{"product_id":"10"}`, false, 401},
		{"POST", "/api/v1/conversations", `{"product_id":"10","buyer_id":"3"}`, true, 400},
		{"POST", "/api/v1/conversations", `{"product_id":10}`, true, 400},
		{"GET", "/api/v1/conversations?page_size=1&page_size=2", "", true, 400},
		{"GET", "/api/v1/conversations/1/messages?before_seq=overflow", "", true, 400},
		{"POST", "/api/v1/conversations", `{"product_id":"10"}`, true, 200},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.url, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		if tc.authenticated {
			req.Header.Set("Authorization", "Bearer "+raw)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("%s: status=%d body=%s", tc.url, w.Code, w.Body.String())
		}
		if w.Code == 200 && !strings.Contains(w.Body.String(), `"9007199254740993"`) {
			t.Fatal("大整数丢失精度")
		}
	}
	if client.calls != 1 {
		t.Fatal("非法请求进入 RPC")
	}
}
