package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"remarket/backend/pkg/auth"
)

// TestNginxEdge 使用真实 Nginx 容器验证双 Gin Node 亲和、代理边界、WS 与票据脱敏，不连接业务库。
func TestNginxEdge(t *testing.T) {
	if os.Getenv("NGINX_EDGE_INTEGRATION") != "1" {
		t.Skip("需要 NGINX_EDGE_INTEGRATION=1 和本机 Docker")
	}
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// docker 仅执行本测试创建的容器/网络操作，错误不输出完整环境或认证信息。
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("Docker 验收命令失败: %s", out)
		}
		return strings.TrimSpace(string(out))
	}
	tokens, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "edge-test", Audience: "edge-test", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var ports []string
	var writeAttempts atomic.Int64
	for _, name := range []string{"a", "b"} {
		router := NewRouter(nil, 3*time.Second)
		// echo 仅提供测试响应，验证入口不把客户端伪造身份头传给业务服务。
		router.GET("/api/v1/edge-test", func(c *gin.Context) {
			c.JSON(200, gin.H{"node": name, "host": c.Request.Host, "upgrade": c.GetHeader("Upgrade"), "connection": c.GetHeader("Connection"), "user": c.GetHeader("X-User-ID"), "role": c.GetHeader("X-Role"), "forwarded": c.GetHeader("X-Forwarded-For")})
		})
		// 模拟写请求已被上游接收后连接中断，入口不得向另一 Node 重放。
		router.POST("/api/v1/edge-fail", func(c *gin.Context) {
			writeAttempts.Add(1)
			connection, _, err := c.Writer.Hijack()
			if err != nil {
				return
			}
			_ = connection.Close()
			c.Abort()
		})
		hub := AddChatRealtimeRoutes(router, &chatTestClient{}, realtimeUsers{}, tokens, nil, 3*time.Second)
		server := httptest.NewServer(router)
		t.Cleanup(func() { hub.Close(); server.Close() })
		_, port, e := net.SplitHostPort(server.Listener.Addr().String())
		if e != nil {
			t.Fatal(e)
		}
		ports = append(ports, port)
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("remarket-edge-test-%d", time.Now().UnixNano())
	network := name + "-net"
	docker("network", "create", network)
	owned := []string{}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		for _, container := range owned {
			if out, e := exec.CommandContext(cleanup, "docker", "rm", "-f", container).CombinedOutput(); e != nil {
				t.Errorf("清理测试容器失败: %s", out)
			}
		}
		if out, e := exec.CommandContext(cleanup, "docker", "network", "rm", network).CombinedOutput(); e != nil {
			t.Errorf("清理测试网络失败: %s", out)
		}
	})
	image := "nginx:1.28-alpine"
	docker("run", "-d", "--read-only", "--tmpfs", "/etc/nginx/conf.d", "--tmpfs", "/var/cache/nginx", "--tmpfs", "/var/run", "--name", name, "--network", network, "--network-alias", "edge", "--add-host", "host.docker.internal:host-gateway",
		"-p", "127.0.0.1::80", "-e", "EDGE_GATEWAY_HOST=host.docker.internal", "-e", "EDGE_GATEWAY_PRIMARY_PORT="+ports[0], "-e", "EDGE_GATEWAY_SECONDARY_PORT="+ports[1], "-e", "NGINX_ENVSUBST_FILTER=^EDGE_",
		"--mount", "type=bind,src="+filepath.Join(root, "deployments/nginx/19-resolve-gateway.envsh")+",dst=/docker-entrypoint.d/19-resolve-gateway.envsh,readonly",
		"--mount", "type=bind,src="+filepath.Join(root, "deployments/nginx/remarket.conf.template")+",dst=/etc/nginx/templates/remarket.conf.template,readonly",
		"--mount", "type=bind,src="+filepath.Join(root, "deployments/nginx/remarket-proxy.conf")+",dst=/etc/nginx/snippets/remarket-proxy.conf,readonly",
		"--mount", "type=bind,src="+filepath.Join(root, "frontend/dist")+",dst=/usr/share/nginx/html,readonly", image)
	owned = append(owned, name)
	port := docker("port", name, "80/tcp")
	origin := "http://" + strings.TrimSpace(strings.Split(port, "\n")[0])
	client := &http.Client{Timeout: 3 * time.Second}
	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		response, e := client.Get(origin + "/edge-healthz")
		if e == nil {
			response.Body.Close()
			if response.StatusCode == 204 {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("Nginx 未就绪: %s", docker("logs", name))
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", origin+"/api/v1/edge-test?ticket=redaction-sentinel", nil)
	req.Header.Set("X-User-ID", "999")
	req.Header.Set("X-Role", "ADMIN")
	req.Header.Set("X-Forwarded-For", "198.51.100.99")
	req.Header.Set("X-Request-ID", "forged")
	req.Header.Set("Upgrade", "websocket")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var echo map[string]string
	if e := json.NewDecoder(response.Body).Decode(&echo); e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 200 || echo["user"] != "" || echo["role"] != "" || echo["upgrade"] != "" || echo["connection"] != "" || echo["forwarded"] == "198.51.100.99" {
		t.Fatal("普通 HTTP 代理边界错误", echo)
	}
	if echo["host"] != strings.TrimPrefix(origin, "http://") || response.Header.Get("X-Request-ID") == "forged" || response.Header.Get("X-Request-ID") == "" {
		t.Fatal("Host/Request ID 传递错误")
	}
	// 每个独立 Docker 客户端有自己的真实源 IP，不能靠伪造 XFF 演示负载均衡。
	reached := map[string]bool{echo["node"]: true}
	for i := 0; i < 16 && len(reached) < 2; i++ {
		container := name + "-client-" + strconv.Itoa(i)
		docker("run", "-d", "--name", container, "--network", network, "--entrypoint", "sleep", image, "90")
		owned = append(owned, container)
		raw := docker("exec", container, "wget", "-q", "-O", "-", "http://edge/api/v1/edge-test")
		var result map[string]string
		if json.Unmarshal([]byte(raw), &result) != nil {
			t.Fatal("负载均衡响应无效")
		}
		reached[result["node"]] = true
	}
	if !reached["a"] || !reached["b"] {
		t.Fatal("没有覆盖两个上游节点")
	}
	raw, _, err := tokens.IssueAccessToken(1, "USER")
	if err != nil {
		t.Fatal(err)
	}
	ticketReq, _ := http.NewRequestWithContext(ctx, "POST", origin+"/api/v1/chat/tickets", nil)
	ticketReq.Header.Set("Authorization", "Bearer "+raw)
	ticketReq.Header.Set("Origin", origin)
	ticketResponse, err := client.Do(ticketReq)
	if err != nil {
		t.Fatal(err)
	}
	if ticketResponse.StatusCode != 200 {
		ticketResponse.Body.Close()
		t.Fatalf("票据 HTTP 状态 %d，入口日志: %s", ticketResponse.StatusCode, docker("logs", name))
	}
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	if e := json.NewDecoder(ticketResponse.Body).Decode(&ticket); e != nil {
		t.Fatal(e)
	}
	ticketResponse.Body.Close()
	if ticketResponse.StatusCode != 200 || ticket.Ticket == "" {
		t.Fatal("代理票据请求失败")
	}
	wsURL := "ws" + strings.TrimPrefix(origin, "http") + "/api/v1/chat/ws?" + url.Values{"ticket": []string{ticket.Ticket}, "probe": []string{"ws-redaction-sentinel"}}.Encode()
	conn, handshake, err := websocket.DefaultDialer.DialContext(ctx, wsURL, http.Header{"Origin": []string{origin}})
	if err != nil {
		t.Fatal("票据与 WS 未落同一上游，或升级失败")
	}
	defer conn.Close()
	if handshake.StatusCode != 101 {
		t.Fatal("WS 未升级")
	}
	if e := conn.WriteJSON(map[string]string{"type": "ping", "request_id": "edge-ping"}); e != nil {
		t.Fatal(e)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var pong struct{ Type string }
	if e := conn.ReadJSON(&pong); e != nil || pong.Type != "pong" {
		t.Fatal("代理 WS 心跳失败", e)
	}
	conn.Close()
	failedRequest, _ := http.NewRequestWithContext(ctx, "POST", origin+"/api/v1/edge-fail", strings.NewReader("write"))
	failedResponse, e := client.Do(failedRequest)
	if e != nil {
		t.Fatal(e)
	}
	failedResponse.Body.Close()
	if failedResponse.StatusCode != 502 || writeAttempts.Load() != 1 {
		t.Fatal("入口重放已接收写请求", writeAttempts.Load())
	}
	for _, path := range []string{"/", "/metrics"} {
		response, e := client.Get(origin + path)
		if e != nil {
			t.Fatal(e)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if path == "/" && (response.StatusCode != 200 || !strings.Contains(string(body), "<!doctype html>")) {
			t.Fatal("前端入口不可用")
		}
		if path == "/metrics" && response.StatusCode != 404 {
			t.Fatal("指标入口对外暴露")
		}
	}
	logs := docker("logs", name)
	for deadline := time.Now().Add(2 * time.Second); !strings.Contains(logs, `"status":101`) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		logs = docker("logs", name)
	}
	if !strings.Contains(logs, `"status":101`) {
		t.Fatal("未记录 WS 握手完成日志")
	}
	if strings.Contains(logs, ticket.Ticket) || strings.Contains(logs, raw) || strings.Contains(logs, "redaction-sentinel") {
		t.Fatal("Nginx 日志泄露查询参数或认证信息")
	}
	t.Log("真实 Nginx 验证双 Node 负载、来源亲和、HTTP 头边界、WS 101/pong、前端和访问日志脱敏通过")
}
