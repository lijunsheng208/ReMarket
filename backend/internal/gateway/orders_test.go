package gateway

import (
	"context"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	orderv1 "remarket/backend/gen/order/v1"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/grpcx"
)

type orderProbe struct {
	orderv1.UnimplementedOrderServiceServer
	calls    atomic.Int32
	observed chan *orderv1.CreateOrderRequest
	metadata chan metadata.MD
}

// CreateOrder 检查 Order 独立验签后的身份，模拟结果与故障；不替代真实订单持久化测试。
func (p *orderProbe) CreateOrder(ctx context.Context, req *orderv1.CreateOrderRequest) (*orderv1.OrderResponse, error) {
	p.calls.Add(1)
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok || identity.UserID != 42 {
		return nil, status.Error(codes.PermissionDenied, "identity missing")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, status.Error(codes.Internal, "deadline missing")
	}
	switch req.ProductId {
	case 2:
		return nil, status.Error(codes.AlreadyExists, "幂等键已用于不同请求")
	case 3:
		return nil, status.Error(codes.Internal, "sensitive sql")
	case 4:
		<-ctx.Done()
		return nil, ctx.Err()
	case 5:
		return nil, status.Error(codes.Unavailable, "sensitive address")
	}
	md, _ := metadata.FromIncomingContext(ctx)
	p.metadata <- md
	p.observed <- req
	state := orderv1.OrderStatus_ORDER_STATUS_PENDING
	if req.ProductId == 6 {
		state = orderv1.OrderStatus_ORDER_STATUS_RESERVED
	}
	return &orderv1.OrderResponse{Order: &orderv1.Order{Id: 9007199254740993, ProductId: req.ProductId, BuyerId: identity.UserID, Status: state}}, nil
}

// GetOrder 模拟详情与不存在订单，确认路径大整数按 int64 无损传递。
func (p *orderProbe) GetOrder(ctx context.Context, req *orderv1.GetOrderRequest) (*orderv1.OrderResponse, error) {
	p.calls.Add(1)
	if req.Id == 1 {
		return nil, status.Error(codes.NotFound, "订单不存在")
	}
	return &orderv1.OrderResponse{Order: &orderv1.Order{Id: req.Id, Status: orderv1.OrderStatus_ORDER_STATUS_RESERVED}}, nil
}

// ListOrders 回显分页总数及角色用于验证 HTTP 到 Proto 转换。
func (p *orderProbe) ListOrders(ctx context.Context, req *orderv1.ListOrdersRequest) (*orderv1.ListOrdersResponse, error) {
	p.calls.Add(1)
	return &orderv1.ListOrdersResponse{Total: int64(req.Page), Orders: []*orderv1.Order{{Id: int64(req.PageSize), BuyerId: int64(req.Role)}}}, nil
}

// CancelOrder 模拟取消决定，校验 HTTP 按真实状态返回处理中。
func (p *orderProbe) CancelOrder(ctx context.Context, req *orderv1.CancelOrderRequest) (*orderv1.OrderResponse, error) {
	p.calls.Add(1)
	return &orderv1.OrderResponse{Order: &orderv1.Order{Id: req.Id, Status: orderv1.OrderStatus_ORDER_STATUS_CANCELLING}}, nil
}

// ConfirmOrder 模拟单方确认或成交决定，验证 HTTP 不把单方确认伪装为成交。
func (p *orderProbe) ConfirmOrder(ctx context.Context, req *orderv1.ConfirmOrderRequest) (*orderv1.OrderResponse, error) {
	p.calls.Add(1)
	state := orderv1.OrderStatus_ORDER_STATUS_RESERVED
	if req.Id == 2 {
		state = orderv1.OrderStatus_ORDER_STATUS_CONFIRMING
	}
	return &orderv1.OrderResponse{Order: &orderv1.Order{Id: req.Id, Status: state, BuyerConfirmed: true}}, nil
}

// TestOrderHTTPRPC 验证真实 HTTP 适配到 gRPC 的幂等键、认证、超时、错误及参数边界。
func TestOrderHTTPRPC(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokens, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("j", 32), Issuer: "test", Audience: "test", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := tokens.IssueAccessToken(42, "USER")
	if err != nil {
		t.Fatal(err)
	}
	upstream := &orderProbe{observed: make(chan *orderv1.CreateOrderRequest, 10), metadata: make(chan metadata.MD, 10)}
	listener := bufconn.Listen(1 << 20)
	server := grpcx.NewServer(nil, grpcx.AuthInterceptor(tokens))
	orderv1.RegisterOrderServiceServer(server, upstream)
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()
	conn, err := grpc.NewClient("passthrough:///test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithUnaryInterceptor(grpcx.ClientInterceptor(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	router := NewRouter(nil, time.Second)
	AddOrderRoutes(router, orderv1.NewOrderServiceClient(conn), tokens, 100*time.Millisecond)
	// 每个请求均伪造身份头，确认实际授权仍来源于验签 Token。
	send := func(method, path, body string, keys []string, authorization string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-ID", "order-http-review")
		req.Header.Set("X-User-ID", "999")
		req.Header.Set("X-Role", "ADMIN")
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		for _, key := range keys {
			req.Header.Add("Idempotency-Key", key)
		}
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res
	}
	for i := 0; i < 2; i++ {
		res := send("POST", "/api/v1/orders", `{"product_id":"9007199254740993"}`, []string{"Case-Key"}, "Bearer "+token)
		if res.Code != 202 || res.Header().Get("Location") != "/api/v1/orders/9007199254740993" || res.Header().Get("Cache-Control") != "no-store" || !strings.Contains(res.Body.String(), `"id":"9007199254740993"`) {
			t.Fatal("创建响应语义错误", res.Code, res.Body.String())
		}
		observed := <-upstream.observed
		md := <-upstream.metadata
		if observed.ProductId != 9007199254740993 || observed.IdempotencyKey != "Case-Key" || len(md.Get("authorization")) != 1 || md.Get("authorization")[0] != "Bearer "+token || md.Get("x-request-id")[0] != "order-http-review" || len(md.Get("x-user-id")) != 0 || len(md.Get("x-role")) != 0 {
			t.Fatal("请求转换或凭据传递错误")
		}
	}
	before := upstream.calls.Load()
	invalid := []struct {
		body string
		keys []string
	}{
		{`{"product_id":"1"}`, nil},
		{`{"product_id":"1"}`, []string{""}},
		{`{"product_id":"1"}`, []string{"a", "b"}},
		{`{"product_id":"1"}`, []string{"a b"}},
		{`{"product_id":"1"}`, []string{strings.Repeat("a", 129)}},
		{`{"product_id":1}`, []string{"valid"}},
		{`{"product_id":"0"}`, []string{"valid"}},
		{`{"product_id":"1","idempotency_key":"body-key"}`, []string{"valid"}},
		{`{"product_id":"1","buyer_id":"999"}`, []string{"valid"}},
	}
	for _, tc := range invalid {
		res := send("POST", "/api/v1/orders", tc.body, tc.keys, "Bearer "+token)
		if res.Code != 400 {
			t.Fatal("非法创建未拒绝", res.Code, res.Body.String())
		}
	}
	for _, authorization := range []string{"", "Bearer invalid"} {
		res := send("POST", "/api/v1/orders", `{"product_id":"1"}`, []string{"valid"}, authorization)
		if res.Code != 401 {
			t.Fatal("无效身份未拒绝", res.Code)
		}
	}
	for _, path := range []string{"/api/v1/orders", "/api/v1/orders?role=admin", "/api/v1/orders?role=buyer&role=seller", "/api/v1/orders?role=buyer&user_id=99", "/api/v1/orders?role=buyer&page=-1", "/api/v1/orders?role=buyer&page_size=101", "/api/v1/orders/9223372036854775808", "/api/v1/orders/0"} {
		if res := send("GET", path, "", nil, "Bearer "+token); res.Code != 400 {
			t.Fatal("非法查询未拒绝", path, res.Code)
		}
	}
	if upstream.calls.Load() != before {
		t.Fatal("非法请求访问了下游")
	}
	for _, tc := range []struct{ id, code int }{{2, 409}, {3, 500}, {4, 504}, {5, 503}, {6, 200}} {
		res := send("POST", "/api/v1/orders", `{"product_id":"`+strconv.Itoa(tc.id)+`"}`, []string{"valid"}, "Bearer "+token)
		if res.Code != tc.code || strings.Contains(res.Body.String(), "sensitive") {
			t.Fatal("错误或状态转换错误", res.Code, res.Body.String())
		}
	}
	if res := send("GET", "/api/v1/orders/9007199254740993", "", nil, "Bearer "+token); res.Code != 200 || !strings.Contains(res.Body.String(), `"9007199254740993"`) {
		t.Fatal("详情 ID 精度错误", res.Code)
	}
	if res := send("GET", "/api/v1/orders/1", "", nil, "Bearer "+token); res.Code != 404 {
		t.Fatal("NotFound 映射错误", res.Code)
	}
	if res := send("GET", "/api/v1/orders?role=seller&page=2&page_size=3", "", nil, "Bearer "+token); res.Code != 200 || !strings.Contains(res.Body.String(), `"total":"2"`) || !strings.Contains(res.Body.String(), `"id":"3"`) {
		t.Fatal("列表适配错误", res.Code, res.Body.String())
	}
	for _, tc := range []struct {
		path, body, authorization string
		code                      int
	}{
		{"/api/v1/orders/1/cancel", "", "Bearer " + token, 202},
		{"/api/v1/orders/1/confirm", "", "Bearer " + token, 200},
		{"/api/v1/orders/2/confirm", "", "Bearer " + token, 202},
		{"/api/v1/orders/1/cancel", `{"buyer_id":"999"}`, "Bearer " + token, 400},
		{"/api/v1/orders/0/cancel", "", "Bearer " + token, 400},
		{"/api/v1/orders/1/confirm", "", "", 401},
	} {
		res := send("POST", tc.path, tc.body, nil, tc.authorization)
		if res.Code != tc.code {
			t.Fatal("订单决定 HTTP 适配错误", tc.path, res.Code, res.Body.String())
		}
	}

}
