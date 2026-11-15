package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	health "google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
	"remarket/backend/internal/gateway"
	"remarket/backend/pkg/grpcx"
)

// TestPrometheusScrape 使用真实 Prometheus 采集 HTTP/gRPC Exporter 并查询 up、计数与延迟分位数。
func TestPrometheusScrape(t *testing.T) {
	if os.Getenv("PROMETHEUS_INTEGRATION") != "1" {
		t.Skip("需要 PROMETHEUS_INTEGRATION=1 和已下载的 Prometheus 镜像")
	}
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	docker := func(args ...string) string {
		t.Helper()
		out, e := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if e != nil {
			t.Fatalf("Prometheus 验收命令失败: %s", out)
		}
		return strings.TrimSpace(string(out))
	}
	image := "prom/prometheus:v3.5.0"
	docker("image", "inspect", image, "--format", "{{.Id}}")
	grpcMetrics, e := New("user")
	if e != nil {
		t.Fatal(e)
	}
	gatewayMetrics, e := New("gateway")
	if e != nil {
		t.Fatal(e)
	}
	rpcServer := grpcx.NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), grpcMetrics.UnaryServerInterceptor(grpcx.Error))
	healthv1.RegisterHealthServer(rpcServer, health.NewServer())
	rpcListener := bufconn.Listen(1 << 20)
	go func() { _ = rpcServer.Serve(rpcListener) }()
	defer rpcServer.Stop()
	defer rpcListener.Close()
	conn, e := grpc.NewClient("passthrough:///prometheus-test", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return rpcListener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	rpc := healthv1.NewHealthClient(conn)
	router := gateway.NewRouter(nil, time.Second, gatewayMetrics.HTTPMiddleware())
	router.GET("/metrics", gin.WrapH(gatewayMetrics.Handler()))
	router.GET("/api/v1/check/:id", func(c *gin.Context) {
		if _, e := rpc.Check(c.Request.Context(), &healthv1.HealthCheckRequest{}); e != nil {
			c.Status(503)
			return
		}
		c.Status(200)
	})
	httpServer := httptest.NewServer(router)
	defer httpServer.Close()
	metricsListener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	stopMetrics := grpcMetrics.serve(ctx, metricsListener)
	defer stopMetrics()
	_, httpPort, _ := net.SplitHostPort(httpServer.Listener.Addr().String())
	_, grpcPort, _ := net.SplitHostPort(metricsListener.Addr().String())
	config := fmt.Sprintf("global:\n  scrape_interval: 1s\n  scrape_timeout: 900ms\nscrape_configs:\n  - job_name: gateway\n    static_configs:\n      - targets: ['host.docker.internal:%s']\n  - job_name: user\n    static_configs:\n      - targets: ['host.docker.internal:%s']\n", httpPort, grpcPort)
	path := filepath.Join(t.TempDir(), "prometheus.yml")
	if e := os.WriteFile(path, []byte(config), 0644); e != nil {
		t.Fatal(e)
	}
	name := fmt.Sprintf("remarket-prometheus-test-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if out, e := exec.CommandContext(cleanup, "docker", "rm", "-f", name).CombinedOutput(); e != nil {
			t.Errorf("清理 Prometheus 测试容器失败: %s", out)
		}
	})
	docker("run", "-d", "--read-only", "--tmpfs", "/tmp", "--tmpfs", "/prometheus:uid=65534,gid=65534", "--name", name, "--add-host", "host.docker.internal:host-gateway", "-p", "127.0.0.1::9090",
		"--mount", "type=bind,src="+path+",dst=/etc/prometheus/prometheus.yml,readonly", image, "--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/prometheus")
	address := strings.Split(docker("port", name, "9090/tcp"), "\n")[0]
	endpoint := "http://" + address
	client := &http.Client{Timeout: 2 * time.Second}
	// produce 调用实际 HTTP→gRPC 链路，动态 ID 只作为业务参数，不进入指标标签。
	produce := func(id string) {
		t.Helper()
		response, e := client.Get(httpServer.URL + "/api/v1/check/" + id)
		if e != nil {
			t.Fatal(e)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal(response.StatusCode)
		}
	}
	produce("9007199254740993")
	produce("42")
	// query 从 Prometheus HTTP API 读取真实抓取后的数值，不直接查看 Exporter 假装采集完成。
	query := func(expression string) ([]float64, error) {
		response, e := client.Get(endpoint + "/api/v1/query?" + url.Values{"query": []string{expression}}.Encode())
		if e != nil {
			return nil, e
		}
		defer response.Body.Close()
		var data struct {
			Status string
			Data   struct {
				Result []struct{ Value []json.RawMessage }
			}
		}
		if e := json.NewDecoder(response.Body).Decode(&data); e != nil {
			return nil, e
		}
		if response.StatusCode != 200 || data.Status != "success" {
			return nil, fmt.Errorf("查询失败")
		}
		var values []float64
		for _, sample := range data.Data.Result {
			if len(sample.Value) != 2 {
				return nil, fmt.Errorf("样本无效")
			}
			var raw string
			if e := json.Unmarshal(sample.Value[1], &raw); e != nil {
				return nil, e
			}
			value, e := strconv.ParseFloat(raw, 64)
			if e != nil {
				return nil, e
			}
			values = append(values, value)
		}
		return values, nil
	}
	// eventually 只允许有界等待，异常时输出无凭据的 Prometheus 容器诊断。
	eventually := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("等待 Prometheus 采集超时: %s", docker("logs", name))
	}
	eventually(func() bool {
		values, e := query(`up{job=~"gateway|user"}`)
		return e == nil && len(values) == 2 && values[0] == 1 && values[1] == 1
	})
	eventually(func() bool {
		values, e := query(`http_requests_total{job="gateway",route="/api/v1/check/:id",status="200"}`)
		return e == nil && len(values) == 1 && values[0] == 2
	})
	eventually(func() bool {
		values, e := query(`grpc_server_handled_total{job="user",grpc_method="Check",code="OK"}`)
		return e == nil && len(values) == 1 && values[0] == 2
	})
	produce("43")
	produce("44")
	eventually(func() bool {
		values, e := query(`histogram_quantile(0.95,sum by (le)(rate(http_request_duration_seconds_bucket{job="gateway"}[1m])))`)
		return e == nil && len(values) == 1 && !math.IsNaN(values[0]) && !math.IsInf(values[0], 0) && values[0] > 0
	})
	stopMetrics()
	eventually(func() bool {
		values, e := query(`up{job="user"}`)
		return e == nil && len(values) == 1 && values[0] == 0
	})
	t.Log("真实 Prometheus 抓取 HTTP/gRPC、up=1、请求计数、P95 及采集端口停止后 up=0 验证通过")
}
