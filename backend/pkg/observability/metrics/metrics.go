// Package metrics 提供进程独立的 Prometheus 指标，不注册业务模型或访问业务数据。
package metrics

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var latencyBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}

// Metrics 每个实例独占 Registry，避免多实例测试或进程内服务器互相污染。
type Metrics struct {
	registry            *prometheus.Registry
	address             string
	httpRequests        *prometheus.CounterVec
	httpDuration        *prometheus.HistogramVec
	httpActive          prometheus.Gauge
	grpcRequests        *prometheus.CounterVec
	grpcDuration        *prometheus.HistogramVec
	grpcActive          prometheus.Gauge
	wsHandshakes        *prometheus.CounterVec
	wsHandshakeDuration *prometheus.HistogramVec
	wsActive            prometheus.Gauge
	wsDuration          prometheus.Histogram
	chatSends           *prometheus.CounterVec
}

// New 仅接受固定服务名作为常量标签，动态用户、实体、请求或来源不进入指标。
func New(service string) (*Metrics, error) {
	switch service {
	case "gateway", "user", "product", "order", "search", "feed", "chat":
	default:
		return nil, errors.New("指标服务名无效")
	}
	labels := prometheus.Labels{"service": service}
	m := &Metrics{registry: prometheus.NewRegistry()}
	m.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "http_requests_total", Help: "完成的普通 HTTP 请求，不含 metrics 与 WS 路径。", ConstLabels: labels}, []string{"method", "route", "status"})
	m.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "http_request_duration_seconds", Help: "普通 HTTP 请求耗时，不含长连接寿命。", ConstLabels: labels, Buckets: latencyBuckets}, []string{"method", "route"})
	m.httpActive = prometheus.NewGauge(prometheus.GaugeOpts{Name: "http_requests_in_flight", Help: "正在处理的普通 HTTP 请求数。", ConstLabels: labels})
	m.grpcRequests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "grpc_server_handled_total", Help: "已注册一元 gRPC 方法的完成次数。", ConstLabels: labels}, []string{"grpc_service", "grpc_method", "code"})
	m.grpcDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "grpc_server_handling_seconds", Help: "已注册一元 gRPC 方法的处理耗时。", ConstLabels: labels, Buckets: latencyBuckets}, []string{"grpc_service", "grpc_method"})
	m.grpcActive = prometheus.NewGauge(prometheus.GaugeOpts{Name: "grpc_server_requests_in_flight", Help: "正在处理的一元 gRPC 请求数。", ConstLabels: labels})
	m.wsHandshakes = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "websocket_handshakes_total", Help: "WS 升级成功或拒绝次数，不等于在线连接数。", ConstLabels: labels}, []string{"result"})
	m.wsHandshakeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "websocket_handshake_duration_seconds", Help: "WS 握手耗时，包含账号验证，不包含连接寿命。", ConstLabels: labels, Buckets: latencyBuckets}, []string{"result"})
	m.wsActive = prometheus.NewGauge(prometheus.GaugeOpts{Name: "websocket_connections", Help: "当前本 Node 已注册的 WS 连接数。", ConstLabels: labels})
	m.wsDuration = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "websocket_connection_duration_seconds", Help: "已关闭 WS 连接的生命周期。", ConstLabels: labels, Buckets: []float64{1, 5, 15, 60, 300, 900, 3600, 14400, 86400}})
	m.chatSends = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "chat_send_rpc_total", Help: "WS Node 发起的聊天发送 RPC 结果，含幂等重试，不是唯一消息数量。", ConstLabels: labels}, []string{"code"})
	m.registry.MustRegister(m.httpRequests, m.httpDuration, m.httpActive, m.grpcRequests, m.grpcDuration, m.grpcActive, m.wsHandshakes, m.wsHandshakeDuration, m.wsActive, m.wsDuration, m.chatSends)
	prometheus.WrapRegistererWith(labels, m.registry).MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m.wsHandshakes.WithLabelValues("accepted")
	m.wsHandshakes.WithLabelValues("rejected")
	return m, nil
}

// Handler 只暴露本实例 Registry，限制同时采集数及处理时间，不查询任何业务存储。
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{EnableOpenMetrics: true, MaxRequestsInFlight: 2, Timeout: 3 * time.Second})
}

// httpMethod 将客户端任意方法折叠为固定集合，避免不常见动词放大时序。
func httpMethod(method string) string {
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "CONNECT", "TRACE":
		return method
	default:
		return "OTHER"
	}
}

// HTTPMiddleware 使用注册路由模板；认证/限流失败也统计，panic 先记录再交外层统一恢复。
func (m *Metrics) HTTPMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if m == nil || c.FullPath() == "/metrics" || c.FullPath() == "/api/v1/chat/ws" {
			c.Next()
			return
		}
		start := time.Now()
		m.httpActive.Inc()
		// observe 在返回或 panic 时恰好执行一次，路由标签从不读取原始 URL/Query。
		observe := func(code int) {
			m.httpActive.Dec()
			route := c.FullPath()
			if route == "" {
				route = "unmatched"
			}
			label := "unknown"
			if code >= 100 && code <= 599 {
				label = strconv.Itoa(code)
			}
			method := httpMethod(c.Request.Method)
			m.httpRequests.WithLabelValues(method, route, label).Inc()
			m.httpDuration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
		}
		defer func() {
			if value := recover(); value != nil {
				observe(500)
				panic(value)
			}
			observe(c.Writer.Status())
		}()
		c.Next()
	}
}

// UnaryServerInterceptor 将业务错误映射成稳定 code 标签，重新抛出 panic 让 grpcx 统一隐藏内部信息。
func (m *Metrics) UnaryServerInterceptor(normalize func(error) error) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (response any, err error) {
		if m == nil {
			return next(ctx, req)
		}
		full := strings.TrimPrefix(info.FullMethod, "/")
		service, method, ok := strings.Cut(full, "/")
		if !ok {
			service = "unknown"
			method = "unknown"
		}
		start := time.Now()
		m.grpcActive.Inc()
		observe := func(code codes.Code) {
			m.grpcActive.Dec()
			m.grpcRequests.WithLabelValues(service, method, code.String()).Inc()
			m.grpcDuration.WithLabelValues(service, method).Observe(time.Since(start).Seconds())
		}
		defer func() {
			if value := recover(); value != nil {
				observe(codes.Internal)
				panic(value)
			}
			mapped := err
			if normalize != nil {
				mapped = normalize(err)
			}
			observe(status.Code(mapped))
		}()
		return next(ctx, req)
	}
}

// HandshakeFinished 只记录握手阶段，不把长期连接时长混入 HTTP 或握手分位数。
func (m *Metrics) HandshakeFinished(accepted bool, duration time.Duration) {
	if m == nil {
		return
	}
	result := "rejected"
	if accepted {
		result = "accepted"
	}
	m.wsHandshakes.WithLabelValues(result).Inc()
	m.wsHandshakeDuration.WithLabelValues(result).Observe(duration.Seconds())
}

// ConnectionOpened 返回幂等关闭钩子，客户端断开、超时和服务关闭都会回收在线 Gauge。
func (m *Metrics) ConnectionOpened() func() {
	if m == nil {
		return func() {}
	}
	start := time.Now()
	m.wsActive.Inc()
	var once sync.Once
	return func() { once.Do(func() { m.wsActive.Dec(); m.wsDuration.Observe(time.Since(start).Seconds()) }) }
}

// SendFinished 统计发送 RPC 尝试结果，不能误称已保存的唯一消息总数。
func (m *Metrics) SendFinished(code codes.Code) {
	if m == nil {
		return
	}
	if code > codes.Unauthenticated {
		code = codes.Unknown
	}
	m.chatSends.WithLabelValues(code.String()).Inc()
}
