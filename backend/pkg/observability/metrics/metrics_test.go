package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/internal/gateway"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/grpcx"
)

// metricValue 从真实 Registry 查找指定标签，缺失视为零，支持 Counter/Gauge/Histogram。
func metricValue(t *testing.T, m *Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	sum := float64(0)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, sample := range family.Metric {
			found := map[string]string{}
			for _, pair := range sample.Label {
				found[pair.GetName()] = pair.GetValue()
			}
			match := true
			for k, v := range labels {
				if found[k] != v {
					match = false
				}
			}
			if match {
				switch {
				case sample.Counter != nil:
					sum += sample.Counter.GetValue()
				case sample.Gauge != nil:
					sum += sample.Gauge.GetValue()
				case sample.Histogram != nil:
					sum += float64(sample.Histogram.GetSampleCount())
				}
			}
		}
	}
	return sum
}

// exposition 读取真实 HTTP 指标导出，不依赖实现内部名称之外的结构。
func exposition(t *testing.T, m *Metrics) string {
	t.Helper()
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Fatal("采集失败", w.Code)
	}
	return w.Body.String()
}

// TestHTTPMetrics 验证动态 ID/查询不成为标签，401/429/panic 统计且采集不污染业务请求数。
func TestHTTPMetrics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m, err := New("gateway")
	if err != nil {
		t.Fatal(err)
	}
	router := gateway.NewRouter(nil, time.Second, m.HTTPMiddleware())
	router.GET("/api/v1/products/:id", func(c *gin.Context) { c.Status(200) })
	router.GET("/denied", func(c *gin.Context) { c.AbortWithStatus(401) })
	router.GET("/limited", func(c *gin.Context) { c.AbortWithStatus(429) })
	router.GET("/panic", func(c *gin.Context) { panic("secret database failure") })
	router.GET("/metrics", gin.WrapH(m.Handler()))
	for _, tc := range []struct {
		path string
		code int
	}{{"/api/v1/products/9007199254740993?token=private", 200}, {"/api/v1/products/22", 200}, {"/denied", 401}, {"/limited", 429}, {"/panic", 500}} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.code {
			t.Fatal(tc.path, w.Code)
		}
	}
	for i := 0; i < 20; i++ {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/unknown/"+strconv.Itoa(i), nil))
	}
	before := metricValue(t, m, "http_requests_total", nil)
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil))
	if metricValue(t, m, "http_requests_total", nil) != before {
		t.Fatal("采集请求混入业务 QPS")
	}
	if metricValue(t, m, "http_requests_total", map[string]string{"route": "/api/v1/products/:id", "status": "200"}) != 2 {
		t.Fatal("动态路由未合并")
	}
	if metricValue(t, m, "http_requests_total", map[string]string{"route": "unmatched", "status": "404"}) != 20 {
		t.Fatal("未知路径未合并")
	}
	for path, code := range map[string]string{"/denied": "401", "/limited": "429", "/panic": "500"} {
		if metricValue(t, m, "http_requests_total", map[string]string{"route": path, "status": code}) != 1 {
			t.Fatal("错误响应统计错误", path)
		}
	}
	if metricValue(t, m, "http_requests_in_flight", nil) != 0 {
		t.Fatal("HTTP 活跃计数未回收")
	}
	body := exposition(t, m)
	for _, private := range []string{"9007199254740993", "private", "secret database", "/unknown/1"} {
		if strings.Contains(body, private) {
			t.Fatal("指标泄漏动态字段", private)
		}
	}
	isolated, err := New("gateway")
	if err != nil {
		t.Fatal(err)
	}
	if metricValue(t, isolated, "http_requests_total", nil) != 0 {
		t.Fatal("Registry 跨实例污染")
	}
}

type demoServer struct {
	userv1.UnimplementedUserServiceServer
	started chan struct{}
}

// Login 提供不同真实错误路径，指标必须观察对外映射后的结果。
func (s *demoServer) Login(ctx context.Context, r *userv1.LoginRequest) (*userv1.LoginResponse, error) {
	switch r.Username {
	case "invalid":
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "参数无效"}
	case "unknown":
		return nil, errors.New("secret SQL")
	case "panic":
		panic("secret panic")
	case "cancel":
		close(s.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &userv1.LoginResponse{}, nil
}

// TestGRPCMetrics 验证真实 gRPC 的认证失败、业务错误、未知错误、panic 和取消均准确计数。
func TestGRPCMetrics(t *testing.T) {
	m, err := New("user")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "test", Audience: "test", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	server := grpcx.NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), m.UnaryServerInterceptor(grpcx.Error), grpcx.AuthInterceptor(tokens, userv1.UserService_Login_FullMethodName))
	demo := &demoServer{started: make(chan struct{})}
	userv1.RegisterUserServiceServer(server, demo)
	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	defer listener.Close()
	conn, err := grpc.NewClient("passthrough:///metrics-test", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := userv1.NewUserServiceClient(conn)
	for _, tc := range []struct {
		name string
		code codes.Code
	}{{"ok", codes.OK}, {"invalid", codes.InvalidArgument}, {"unknown", codes.Internal}, {"panic", codes.Internal}} {
		_, e := client.Login(context.Background(), &userv1.LoginRequest{Username: tc.name, Password: "private-password"})
		if status.Code(e) != tc.code {
			t.Fatal(tc.name, e)
		}
	}
	forged := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-user-id", "999", "x-role", "ADMIN"))
	if _, e := client.GetCurrentUser(forged, &userv1.GetCurrentUserRequest{}); status.Code(e) != codes.Unauthenticated {
		t.Fatal(e)
	}
	callCtx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { _, e := client.Login(callCtx, &userv1.LoginRequest{Username: "cancel"}); finished <- e }()
	select {
	case <-demo.started:
	case <-time.After(time.Second):
		t.Fatal("RPC 未开始")
	}
	cancel()
	if e := <-finished; status.Code(e) != codes.Canceled {
		t.Fatal(e)
	}
	deadline := time.Now().Add(time.Second)
	for metricValue(t, m, "grpc_server_requests_in_flight", nil) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	for code, count := range map[string]float64{"OK": 1, "InvalidArgument": 1, "Internal": 2, "Canceled": 1} {
		if got := metricValue(t, m, "grpc_server_handled_total", map[string]string{"grpc_method": "Login", "code": code}); got != count {
			t.Fatal(code, got, count)
		}
	}
	if metricValue(t, m, "grpc_server_handled_total", map[string]string{"grpc_method": "GetCurrentUser", "code": "Unauthenticated"}) != 1 {
		t.Fatal("认证失败未记录")
	}
	if metricValue(t, m, "grpc_server_handling_seconds", nil) != 6 || metricValue(t, m, "grpc_server_requests_in_flight", nil) != 0 {
		t.Fatal("RPC 直方图或活跃计数不一致")
	}
	// 未注册方法不经过一元拦截器，不能把任意传入方法路径变成标签。
	for i := 0; i < 5; i++ {
		_ = conn.Invoke(context.Background(), "/arbitrary."+strconv.Itoa(i)+"/ID", &userv1.GetCurrentUserRequest{}, &userv1.UserResponse{})
	}
	if metricValue(t, m, "grpc_server_handled_total", nil) != 6 {
		t.Fatal("未注册方法污染标签")
	}
	body := exposition(t, m)
	for _, private := range []string{"private-password", "secret SQL", "secret panic", "arbitrary."} {
		if strings.Contains(body, private) {
			t.Fatal("RPC 指标泄漏请求数据")
		}
	}
}

// TestRuntimeLifecycle 验证指标显式开关、配置错误、端口冲突及停止后监听资源释放。
func TestRuntimeLifecycle(t *testing.T) {
	t.Setenv("METRICS_ENABLED", "false")
	m, err := FromEnv("user")
	if err != nil || m != nil {
		t.Fatal("关闭时仍创建指标")
	}
	t.Setenv("METRICS_ENABLED", "not-bool")
	if _, e := FromEnv("user"); e == nil {
		t.Fatal("接受无效开关")
	}
	t.Setenv("METRICS_ENABLED", "true")
	t.Setenv("USER_METRICS_ADDR", "127.0.0.1:99999")
	if _, e := FromEnv("user"); e == nil {
		t.Fatal("接受无效地址")
	}
	t.Setenv("USER_METRICS_ADDR", "127.0.0.1:9101")
	m, err = FromEnv("user")
	if err != nil {
		t.Fatal(err)
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := m.serve(ctx, listener)
	defer stop()
	response, e := http.Get("http://" + listener.Addr().String() + "/metrics")
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	m.address = listener.Addr().String()
	if _, e := Start(ctx, m); e == nil {
		t.Fatal("端口冲突未报错")
	}
	cancel()
	stop()
	c, e := net.DialTimeout("tcp", listener.Addr().String(), 100*time.Millisecond)
	if e == nil {
		c.Close()
		t.Fatal("停止后端口仍可连接")
	}
}

// TestWebSocketCounters 验证关闭钩子幂等、握手与发送结果指标不包含业务身份。
func TestWebSocketCounters(t *testing.T) {
	m, e := New("gateway")
	if e != nil {
		t.Fatal(e)
	}
	m.HandshakeFinished(true, 20*time.Millisecond)
	m.HandshakeFinished(false, 5*time.Millisecond)
	finish := m.ConnectionOpened()
	if metricValue(t, m, "websocket_connections", nil) != 1 {
		t.Fatal("在线数未增加")
	}
	finish()
	finish()
	if metricValue(t, m, "websocket_connections", nil) != 0 || metricValue(t, m, "websocket_connection_duration_seconds", nil) != 1 {
		t.Fatal("关闭未幂等回收")
	}
	m.SendFinished(codes.OK)
	m.SendFinished(codes.Unavailable)
	if metricValue(t, m, "websocket_handshakes_total", nil) != 2 || metricValue(t, m, "chat_send_rpc_total", nil) != 2 {
		t.Fatal("握手或发送结果计数错误")
	}
}
