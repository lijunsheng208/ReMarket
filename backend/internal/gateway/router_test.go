package gateway

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/pkg/grpcx"
	"strings"
	"testing"
	"time"
)

type rpcUser struct {
	userv1.UnimplementedUserServiceServer
	observed chan metadata.MD
}

// Register 模拟上游服务，验证请求编号、追踪字段及身份头的隔离。
func (s *rpcUser) Register(ctx context.Context, req *userv1.RegisterRequest) (*userv1.UserResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.observed <- md
	if _, ok := ctx.Deadline(); !ok {
		return nil, status.Error(codes.Internal, "deadline missing")
	}
	if req.Username == "duplicate" {
		return nil, status.Error(codes.AlreadyExists, "用户名已存在")
	}
	return &userv1.UserResponse{User: &userv1.User{Id: 9007199254740993, Username: req.Username}}, nil
}

// Login 模拟成功、凭据错误、超时与服务内部故障。
func (s *rpcUser) Login(ctx context.Context, req *userv1.LoginRequest) (*userv1.LoginResponse, error) {
	switch req.Username {
	case "timeout":
		<-ctx.Done()
		return nil, ctx.Err()
	case "wrong":
		return nil, status.Error(codes.Unauthenticated, "登录失败")
	case "internal":
		return nil, status.Error(codes.Internal, "sensitive sql")
	}
	return &userv1.LoginResponse{AccessToken: "test-token"}, nil
}

// TestGatewayRPC 验证 HTTP 到真实 gRPC 的适配、超时、错误及安全响应。
func TestGatewayRPC(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &rpcUser{observed: make(chan metadata.MD, 10)}
	server := grpc.NewServer()
	userv1.RegisterUserServiceServer(server, upstream)
	listener := bufconn.Listen(1 << 20)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///gateway", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithUnaryInterceptor(grpcx.ClientInterceptor(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	router := NewRouter(userv1.NewUserServiceClient(conn), 50*time.Millisecond)
	request := httptest.NewRequest("POST", "/api/v1/auth/register", strings.NewReader(`{"username":"alice","password":"password123","nickname":"用户"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "review-123")
	request.Header.Set("X-User-ID", "999")
	request.Header.Set("Authorization", "Bearer untrusted")
	request.Header.Set("traceparent", "trace-value")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 201 || response.Header().Get("X-Request-ID") != "review-123" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(response.Code, response.Body.String())
	}
	var body map[string]map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["user"]["id"] != "9007199254740993" {
		t.Fatal("ID 精度丢失")
	}
	md := <-upstream.observed
	if md.Get("x-request-id")[0] != "review-123" || md.Get("traceparent")[0] != "trace-value" || len(md.Get("authorization")) > 0 || len(md.Get("x-user-id")) > 0 {
		t.Fatal("metadata 传递错误")
	}
	tests := []struct {
		path, body, content string
		code                int
	}{
		{"register", `{"username":"duplicate"}`, "application/json", 409},
		{"login", `{"username":"alice","password":"password123"}`, "application/json", 200},
		{"login", `{"username":"wrong"}`, "application/json", 401},
		{"login", `{"username":"internal"}`, "application/json", 500},
		{"login", `{"username":"timeout"}`, "application/json", 504},
		{"login", `{`, "application/json", 400},
		{"login", `{} {}`, "application/json", 400},
		{"login", `{"role":"ADMIN"}`, "application/json", 400},
		{"login", `{}`, "text/plain", 415},
		{"login", `{"password":"` + strings.Repeat("a", 17000) + `"}`, "application/json", 413},
	}
	for _, tc := range tests {
		req := httptest.NewRequest("POST", "/api/v1/auth/"+tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", tc.content)
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.body[:min(len(tc.body), 50)], res.Code, res.Body.String())
		}
		if strings.Contains(res.Body.String(), "sensitive sql") {
			t.Fatal("内部错误泄漏")
		}
	}
	// HTTP 的取消信号必须到达 gRPC，而不是被 Background 替换。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	if res.Code != 499 {
		t.Fatal("HTTP 取消未传递", res.Code)
	}
}

// TestHTTPErrorMapping 验证连接故障、限流及业务状态映射不会泄漏内部信息。
func TestHTTPErrorMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for code, want := range map[codes.Code]int{codes.InvalidArgument: 400, codes.PermissionDenied: 403, codes.NotFound: 404, codes.FailedPrecondition: 409, codes.ResourceExhausted: 429, codes.Unavailable: 503, codes.Unimplemented: 501} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		writeRPCError(c, status.Error(code, "test"))
		if recorder.Code != want {
			t.Fatal(code, recorder.Code)
		}
	}
	// recovery 不将 panic 信息输出给客户端。
	router := gin.New()
	router.Use(requestContext(), recoverRequest())
	router.GET("/panic", func(c *gin.Context) { panic("secret") })
	r := httptest.NewRecorder()
	router.ServeHTTP(r, httptest.NewRequest("GET", "/panic", nil))
	data, _ := io.ReadAll(r.Result().Body)
	if r.Code != 500 || strings.Contains(string(data), "secret") {
		t.Fatal("panic 未安全处理")
	}
}
