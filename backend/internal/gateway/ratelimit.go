package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"remarket/backend/pkg/auth"
	rl "remarket/backend/pkg/ratelimit"
)

// LimitRule 使用可读时长表达环境 JSON 中的令牌桶规则。
type LimitRule struct {
	Capacity int64  `json:"capacity"`
	Tokens   int64  `json:"tokens"`
	Interval string `json:"interval"`
}

// rule 验证配置并转换为通用令牌桶参数。
func (r LimitRule) rule() (rl.Rule, error) {
	d, err := time.ParseDuration(r.Interval)
	if err != nil {
		return rl.Rule{}, fmt.Errorf("限流 interval 必须为时长")
	}
	v := rl.Rule{Capacity: r.Capacity, Tokens: r.Tokens, Interval: d}
	return v, v.Validate()
}

// RateConfig 定义总额度和逐接口额度；接口 Key 使用 METHOD 加 Gin 路由模板。
type RateConfig struct {
	Enabled         bool
	Timeout         time.Duration
	User, IP, Route LimitRule
	Routes          map[string]LimitRule
}

// RateConfigFromEnv 加载限流开关、超时和规则 JSON，不回显环境内容。
func RateConfigFromEnv() (RateConfig, error) {
	c := RateConfig{Timeout: 100 * time.Millisecond, User: LimitRule{20, 20, "1s"}, IP: LimitRule{40, 40, "1s"}, Route: LimitRule{20, 20, "1s"}, Routes: map[string]LimitRule{"POST /api/v1/auth/login": {5, 5, "1m"}, "POST /api/v1/auth/register": {5, 5, "1m"}, "POST /api/v1/orders": {2, 2, "1s"}}}
	var err error
	if raw := os.Getenv("GATEWAY_RATE_LIMIT_ENABLED"); raw != "" {
		c.Enabled, err = strconv.ParseBool(raw)
		if err != nil {
			return c, fmt.Errorf("限流开关必须为布尔值")
		}
	}
	if raw := os.Getenv("GATEWAY_RATE_LIMIT_TIMEOUT"); raw != "" {
		c.Timeout, err = time.ParseDuration(raw)
		if err != nil {
			return c, fmt.Errorf("限流超时无效")
		}
	}
	for key, target := range map[string]*LimitRule{"GATEWAY_RATE_LIMIT_USER": &c.User, "GATEWAY_RATE_LIMIT_IP": &c.IP, "GATEWAY_RATE_LIMIT_ROUTE": &c.Route} {
		if raw := os.Getenv(key); raw != "" {
			if err := json.Unmarshal([]byte(raw), target); err != nil {
				return c, fmt.Errorf("%s JSON 无效", key)
			}
		}
	}
	if raw := os.Getenv("GATEWAY_RATE_LIMIT_ROUTES"); raw != "" {
		var overrides map[string]LimitRule
		if err := json.Unmarshal([]byte(raw), &overrides); err != nil {
			return c, fmt.Errorf("接口限流 JSON 无效")
		}
		for key, value := range overrides {
			c.Routes[key] = value
		}
	}
	return c, c.validate()
}

// validate 拒绝不合理超时或规则；开关关闭也校验，避免错误在上线时才出现。
func (c RateConfig) validate() error {
	if c.Timeout <= 0 || c.Timeout > time.Second {
		return fmt.Errorf("限流超时必须大于零且不超过一秒")
	}
	for _, r := range []LimitRule{c.User, c.IP, c.Route} {
		if _, err := r.rule(); err != nil {
			return err
		}
	}
	for path, r := range c.Routes {
		if !strings.Contains(path, " /api/") {
			return fmt.Errorf("接口限流必须使用 METHOD /api/路径模板")
		}
		if _, err := r.rule(); err != nil {
			return err
		}
	}
	return nil
}

// BucketChecker 允许测试和 HTTP 适配器独立于 Redis 客户端。
type BucketChecker interface {
	Allow(context.Context, string, rl.Rule) (rl.Decision, error)
}

// GatewayLimiter 在 Redis 故障时对普通读取使用有界的实例总桶，其余接口保守拒绝。
type GatewayLimiter struct {
	checker  BucketChecker
	cfg      RateConfig
	verifier auth.TokenVerifier
	mu       sync.Mutex
	tokens   float64
	at       time.Time
}

// NewGatewayLimiter 校验并复制接口配置，启动后不允许并发修改规则。
func NewGatewayLimiter(checker BucketChecker, verifier auth.TokenVerifier, cfg RateConfig) (*GatewayLimiter, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if checker == nil || verifier == nil {
		return nil, fmt.Errorf("限流依赖缺失")
	}
	routes := map[string]LimitRule{}
	for k, v := range cfg.Routes {
		routes[k] = v
	}
	cfg.Routes = routes
	return &GatewayLimiter{checker: checker, cfg: cfg, verifier: verifier, tokens: 20, at: time.Now()}, nil
}

// localRead 使用固定容量而非按用户无限增长的 map，故障时每实例最多 20/s、突发 20。
func (l *GatewayLimiter) localRead() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.tokens = min(20, l.tokens+now.Sub(l.at).Seconds()*20)
	l.at = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// limitKey 对身份和接口散列，避免原始 IP、Token 或路径参数进入 Key。
func limitKey(scope, subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return "ratelimit:v1:" + scope + ":" + hex.EncodeToString(sum[:])
}

// Middleware 在路由鉴权前只使用独立验签的身份计费；无效凭据仍由原鉴权入口拒绝。
func (l *GatewayLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !l.cfg.Enabled || c.FullPath() == "" || !strings.HasPrefix(c.FullPath(), "/api/") {
			c.Next()
			return
		}
		subject := "ip:" + c.ClientIP()
		general := l.cfg.IP
		values := c.Request.Header.Values("Authorization")
		if len(values) == 1 && c.FullPath() != "/api/v1/auth/login" && c.FullPath() != "/api/v1/auth/register" {
			parts := strings.Fields(values[0])
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
				verified, err := auth.Authenticate(c.Request.Context(), parts[1], l.verifier)
				if err == nil {
					if identity, ok := auth.IdentityFromContext(verified); ok {
						subject = "user:" + strconv.FormatInt(identity.UserID, 10)
						general = l.cfg.User
					}
				}
			}
		}
		route := c.Request.Method + " " + c.FullPath()
		policy := l.cfg.Route
		if v, ok := l.cfg.Routes[route]; ok {
			policy = v
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), l.cfg.Timeout)
		defer cancel()
		// 两个单 Key 脚本依次扣费；后续规则拒绝时不退还前面额度，明确选择保守计费。
		for _, item := range []struct {
			scope, key string
			rule       LimitRule
		}{{"subject", subject, general}, {"route", subject + "|" + route, policy}} {
			rule, _ := item.rule.rule()
			decision, err := l.checker.Allow(ctx, limitKey(item.scope, item.key), rule)
			if err != nil {
				if c.Request.Context().Err() != nil {
					c.Abort()
					writeError(c, 499, "Canceled", "请求已取消")
					return
				}
				if c.Request.Method == http.MethodGet && l.localRead() {
					c.Header("X-RateLimit-Mode", "local")
					c.Next()
					return
				}
				c.Abort()
				if c.Request.Method == http.MethodGet {
					c.Header("Retry-After", "1")
					writeError(c, 429, "ResourceExhausted", "请求过于频繁")
				} else {
					writeError(c, 503, "Unavailable", "限流服务暂不可用，请稍后重试")
				}
				return
			}
			if !decision.Allowed {
				seconds := max(int64(1), int64((decision.RetryAfter+time.Second-1)/time.Second))
				c.Header("Retry-After", strconv.FormatInt(seconds, 10))
				c.Abort()
				writeError(c, 429, "ResourceExhausted", "请求过于频繁")
				return
			}
		}
		c.Next()
	}
}
