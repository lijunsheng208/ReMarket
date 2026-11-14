package gateway

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"os"
	"remarket/backend/pkg/auth"
	rl "remarket/backend/pkg/ratelimit"
	rx "remarket/backend/pkg/redis"
	"strconv"
	"strings"
	"testing"
	"time"
)

type rejectingBucket struct {
	err  error
	keys []string
}

// Allow 记录计费身份并模拟拒绝或 Redis 故障。
func (b *rejectingBucket) Allow(_ context.Context, key string, _ rl.Rule) (rl.Decision, error) {
	b.keys = append(b.keys, key)
	return rl.Decision{Allowed: false, RetryAfter: 1100 * time.Millisecond}, b.err
}

// rateTestConfig 取得隔离环境后的默认规则。
func rateTestConfig(t *testing.T) RateConfig {
	t.Helper()
	for _, k := range []string{"GATEWAY_RATE_LIMIT_ENABLED", "GATEWAY_RATE_LIMIT_TIMEOUT", "GATEWAY_RATE_LIMIT_USER", "GATEWAY_RATE_LIMIT_IP", "GATEWAY_RATE_LIMIT_ROUTE", "GATEWAY_RATE_LIMIT_ROUTES"} {
		t.Setenv(k, "")
	}
	c, err := RateConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	c.Enabled = true
	return c
}

// rateTokens 创建仅供测试的验签器。
func rateTokens(t *testing.T) *auth.JWTManager {
	t.Helper()
	m, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "test", Audience: "test", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// rateRouter 在注册路由之前安装限流中间件，不触发业务 RPC。
func rateRouter(t *testing.T, b BucketChecker, cfg RateConfig, m *auth.JWTManager) *gin.Engine {
	t.Helper()
	l, err := NewGatewayLimiter(b, m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter(nil, time.Second, l.Middleware())
	r.GET("/api/test", func(c *gin.Context) { c.Status(204) })
	return r
}

// TestRateHTTP 验证统一 429、Retry-After、IP 防伪造与故障时的敏感接口保守拒绝。
func TestRateHTTP(t *testing.T) {
	cfg := rateTestConfig(t)
	m := rateTokens(t)
	bucket := &rejectingBucket{}
	router := rateRouter(t, bucket, cfg, m)
	request := httptest.NewRequest("GET", "/api/test", nil)
	request.RemoteAddr = "192.0.2.1:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.1")
	request.Header.Set("X-User-ID", "1")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, request)
	if w.Code != 429 || w.Header().Get("Retry-After") != "2" || !strings.Contains(w.Body.String(), "request_id") {
		t.Fatal("429 响应错误")
	}
	if bucket.keys[0] != limitKey("subject", "ip:192.0.2.1") {
		t.Fatal("信任了伪造身份或代理 IP")
	}
	bucket.err = errors.New("offline")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader("{}")))
	if w.Code != 503 {
		t.Fatal("敏感接口故障放行")
	}
	for i := 0; i < 21; i++ {
		w = httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/test", nil))
		if i < 20 && w.Code != 204 {
			t.Fatal("读取降级失败")
		}
		if i == 20 && w.Code != 429 {
			t.Fatal("本地降级无限放行")
		}
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 {
		t.Fatal("健康检查被限流")
	}
}

// TestRateConfig 验证规则 JSON 和接口阈值覆盖，以及非法配置拒绝。
func TestRateConfig(t *testing.T) {
	rateTestConfig(t)
	t.Setenv("GATEWAY_RATE_LIMIT_ROUTES", `{"POST /api/v1/auth/login":{"capacity":1,"tokens":1,"interval":"1m"}}`)
	c, err := RateConfigFromEnv()
	if err != nil || c.Routes["POST /api/v1/auth/login"].Capacity != 1 {
		t.Fatal("覆盖失败")
	}
	t.Setenv("GATEWAY_RATE_LIMIT_USER", `{"capacity":0,"tokens":1,"interval":"1s"}`)
	if _, err := RateConfigFromEnv(); err == nil {
		t.Fatal("非法规则被接受")
	}
}

// TestRateMultiGateway 验证两个真实 HTTP 服务和独立 Redis 连接共享用户接口额度。
func TestRateMultiGateway(t *testing.T) {
	if os.Getenv("REDIS_INTEGRATION") != "1" {
		t.Skip("设置 REDIS_INTEGRATION=1 运行真实多 Gateway 验证")
	}
	cfg := rateTestConfig(t)
	cfg.User = LimitRule{100, 100, "1s"}
	cfg.Routes["GET /api/test"] = LimitRule{2, 1, "1h"}
	m := rateTokens(t)
	id := time.Now().UnixNano()
	token, _, err := m.IssueAccessToken(id, "USER")
	if err != nil {
		t.Fatal(err)
	}
	rc, err := rx.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := rx.Open(ctx, rc)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := rx.Open(ctx, rc)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	subject := "user:" + strconv.FormatInt(id, 10)
	defer a.Del(ctx, limitKey("subject", subject), limitKey("route", subject+"|GET /api/test"))
	one := httptest.NewServer(rateRouter(t, rl.New(a), cfg, m))
	defer one.Close()
	two := httptest.NewServer(rateRouter(t, rl.New(b), cfg, m))
	defer two.Close()
	for i, url := range []string{one.URL, two.URL, one.URL, two.URL} {
		request, err := http.NewRequestWithContext(ctx, "GET", url+"/api/test", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		expected := 204
		if i >= 2 {
			expected = 429
		}
		if response.StatusCode != expected {
			t.Fatalf("第 %d 次状态=%d", i, response.StatusCode)
		}
	}
}

// TestLoginAlwaysUsesIP 验证有效 Token 不能绕过登录 IP 桶，普通接口仍按可信用户计费。
func TestLoginAlwaysUsesIP(t *testing.T) {
	cfg := rateTestConfig(t)
	m := rateTokens(t)
	raw, _, err := m.IssueAccessToken(123, "USER")
	if err != nil {
		t.Fatal(err)
	}
	b := &rejectingBucket{}
	r := rateRouter(t, b, cfg, m)
	for _, path := range []string{"/api/v1/auth/login", "/api/test"} {
		method := "GET"
		if strings.Contains(path, "login") {
			method = "POST"
		}
		request := httptest.NewRequest(method, path, nil)
		request.RemoteAddr = "192.0.2.10:1234"
		request.Header.Set("Authorization", "Bearer "+raw)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, request)
	}
	if b.keys[0] != limitKey("subject", "ip:192.0.2.10") || b.keys[1] != limitKey("subject", "user:123") {
		t.Fatal("身份计费错误")
	}
}
