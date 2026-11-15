package node

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	goredis "github.com/redis/go-redis/v9"
	rx "remarket/backend/pkg/redis"
)

// testRedis 建立真实 Redis 连接，仅清理本测试随机命名空间，不清空业务数据库。
func testRedis(t *testing.T) (*goredis.Client, RedisConfig) {
	t.Helper()
	if os.Getenv("CHAT_REDIS_INTEGRATION") != "1" {
		t.Skip("需要 CHAT_REDIS_INTEGRATION=1")
	}
	cfg, err := rx.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := rx.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("chat:test:%d:", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		var cursor uint64
		for {
			keys, next, e := client.Scan(cleanup, cursor, prefix+"*", 100).Result()
			if e != nil {
				t.Error("清理测试 Redis 失败")
				break
			}
			if len(keys) > 0 {
				if e := client.Del(cleanup, keys...).Err(); e != nil {
					t.Error("清理测试 Redis 失败")
					break
				}
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
		client.Close()
	})
	return client, RedisConfig{Prefix: prefix, LeaseTTL: time.Second, RenewInterval: 200 * time.Millisecond}
}

// eventually 在有界时间内等待异步路由变化，不依赖固定启动顺序。
func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等待 Redis 路由状态超时")
}

// routeNodes 读取经 Redis 服务器时间过滤的有效 Node，不信任本机时钟。
func routeNodes(t *testing.T, r *redisRelay, user int64) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	nodes, err := r.client.Eval(ctx, routesScript, []string{r.routeKey(user)}).StringSlice()
	if err != nil {
		t.Fatal(err)
	}
	return nodes
}

// TestRedisNodes 验证双 Node 互通、多设备路由、续期、最后连接清理及陈旧租约隔离。
func TestRedisNodes(t *testing.T) {
	client, cfg := testRedis(t)
	tokens := testTokens(t)
	chats := &testChat{}
	options := *client.Options()
	options.ClientName = cfg.Prefix + "node-b"
	remoteClient := goredis.NewClient(&options)
	defer remoteClient.Close()
	a := New(chats, testUsers{}, tokens, nil)
	b := New(chats, testUsers{}, tokens, nil)
	if err := a.EnableRedis(context.Background(), client, cfg); err != nil {
		t.Fatal(err)
	}
	if err := b.EnableRedis(context.Background(), remoteClient, cfg); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer b.Close()
	as := httptest.NewServer(a)
	bs := httptest.NewServer(b)
	defer as.Close()
	defer bs.Close()
	sender, _ := dialPeer(t, a, as.URL, tokens, 1)
	local, _ := dialPeer(t, a, as.URL, tokens, 2)
	remote, _ := dialPeer(t, b, bs.URL, tokens, 2)
	secondTab, _ := dialPeer(t, b, bs.URL, tokens, 2)
	senderOther, _ := dialPeer(t, b, bs.URL, tokens, 1)
	intruder, _ := dialPeer(t, b, bs.URL, tokens, 3)
	eventually(t, func() bool { return len(routeNodes(t, a.relay, 1)) == 2 && len(routeNodes(t, a.relay, 2)) == 2 })
	// sendAndRead 覆盖本地和远端分发，读取下一次 ping 可检测重复消息或误广播。
	sendAndRead := func() {
		t.Helper()
		if e := sender.WriteJSON(map[string]any{"type": "chat.send", "request_id": "cross", "data": map[string]string{"conversation_id": "10", "client_msg_id": "key", "content": "hello"}}); e != nil {
			t.Fatal(e)
		}
		if f := readFrame(t, sender); f.Type != "chat.ack" {
			t.Fatal("无 ACK", f)
		}
		for _, c := range []*websocket.Conn{remote, senderOther} {
			if f := readFrame(t, c); f.Type != "chat.message" {
				t.Fatal("跨节点未送达", f)
			}
		}
	}
	sendAndRead()
	if f := readFrame(t, local); f.Type != "chat.message" {
		t.Fatal("本地推送被 Redis 影响")
	}
	if f := readFrame(t, secondTab); f.Type != "chat.message" {
		t.Fatal("远端多标签未收到")
	}
	// 续期跨越原始 TTL，连接不应因路由自然过期而失联。
	until := time.Now().Add(1200 * time.Millisecond)
	eventually(t, func() bool { return time.Now().After(until) && len(routeNodes(t, a.relay, 2)) == 2 })
	secondTab.Close()
	eventually(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return len(b.peers[2]) == 1 })
	if len(routeNodes(t, a.relay, 2)) != 2 {
		t.Fatal("关闭一个标签页删除了整个用户路由")
	}
	local.Close()
	eventually(t, func() bool { nodes := routeNodes(t, a.relay, 2); return len(nodes) == 1 && nodes[0] == b.relay.id })
	sendAndRead()
	for _, c := range []*websocket.Conn{remote, senderOther, intruder} {
		if e := c.WriteJSON(map[string]string{"type": "ping", "request_id": "verify"}); e != nil {
			t.Fatal(e)
		}
		if f := readFrame(t, c); f.Type != "pong" {
			t.Fatal("重复推送或非成员泄漏", f)
		}
	}
	// 过期的旧进程租约由查询原子清理，不影响仍在线的其他节点。
	stale := strings.Repeat("a", 32)
	if err := client.ZAdd(context.Background(), a.relay.routeKey(2), goredis.Z{Score: 1, Member: stale}).Err(); err != nil {
		t.Fatal(err)
	}
	if nodes := routeNodes(t, a.relay, 2); len(nodes) != 1 || nodes[0] != b.relay.id {
		t.Fatal("旧路由未过滤")
	}
	// 模拟崩溃进程留下的租约；没有续期时按真实 Redis 时间自动失效。
	if err := client.Eval(context.Background(), leaseScript, []string{a.relay.routeKey(99)}, stale, 1000).Err(); err != nil {
		t.Fatal(err)
	}
	if len(routeNodes(t, a.relay, 99)) != 1 {
		t.Fatal("崩溃租约未建立")
	}
	eventually(t, func() bool { return len(routeNodes(t, a.relay, 99)) == 0 })
	// 精确终止本测试 Node B 的订阅连接，验证库重订阅而不重建 WebSocket。
	listing, err := client.ClientList(context.Background()).Result()
	if err != nil {
		t.Fatal(err)
	}
	killed := false
	for _, line := range strings.Split(listing, "\n") {
		fields := make(map[string]string)
		for _, field := range strings.Fields(line) {
			parts := strings.SplitN(field, "=", 2)
			if len(parts) == 2 {
				fields[parts[0]] = parts[1]
			}
		}
		if fields["name"] == options.ClientName && fields["cmd"] == "subscribe" {
			if err := client.ClientKillByFilter(context.Background(), "ID", fields["id"]).Err(); err != nil {
				t.Fatal(err)
			}
			killed = true
		}
	}
	if !killed {
		t.Fatal("未找到隔离的订阅连接")
	}
	eventually(t, func() bool {
		counts, err := client.PubSubNumSub(context.Background(), b.relay.channel(b.relay.id)).Result()
		return err == nil && counts[b.relay.channel(b.relay.id)] == 1
	})
	sendAndRead()
	b.Close()
	eventually(t, func() bool { return len(routeNodes(t, a.relay, 2)) == 0 && len(routeNodes(t, a.relay, 1)) == 1 })
}

// TestRedisConfig 拒绝不合理续期参数和无效显式开关。
func TestRedisConfig(t *testing.T) {
	t.Setenv("CHAT_REDIS_ENABLED", "not-bool")
	if _, _, e := LoadRedisConfig(); e == nil {
		t.Fatal("接受无效开关")
	}
	t.Setenv("CHAT_REDIS_ENABLED", "true")
	t.Setenv("CHAT_ROUTE_TTL", "2s")
	t.Setenv("CHAT_ROUTE_RENEW_INTERVAL", "1s")
	if _, _, e := LoadRedisConfig(); e == nil {
		t.Fatal("接受过慢续期")
	}
	t.Setenv("CHAT_ROUTE_RENEW_INTERVAL", "500ms")
	if enabled, _, e := LoadRedisConfig(); e != nil || !enabled {
		t.Fatal("合法配置被拒绝", e)
	}
}

type redisFaultHook struct{ fail atomic.Bool }

// DialHook 保留真实连接建立行为，故障只注入命令执行。
func (h *redisFaultHook) DialHook(next goredis.DialHook) goredis.DialHook { return next }

// ProcessHook 注入 Redis 命令失败，不影响 Chat gRPC 和 MySQL。
func (h *redisFaultHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if h.fail.Load() {
			return errors.New("injected redis command failure")
		}
		return next(ctx, cmd)
	}
}

// ProcessPipelineHook 对流水线使用相同故障开关。
func (h *redisFaultHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		if h.fail.Load() {
			return errors.New("injected redis command failure")
		}
		return next(ctx, cmds)
	}
}
