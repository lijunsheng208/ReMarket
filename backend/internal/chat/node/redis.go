package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

const leaseScript = `
local t=redis.call('TIME')
local now=t[1]*1000+math.floor(t[2]/1000)
redis.call('ZREMRANGEBYSCORE',KEYS[1],'-inf',now)
redis.call('ZADD',KEYS[1],now+tonumber(ARGV[2]),ARGV[1])
redis.call('PEXPIRE',KEYS[1],tonumber(ARGV[2])*2)
return 1`
const routesScript = `
local t=redis.call('TIME')
local now=t[1]*1000+math.floor(t[2]/1000)
redis.call('ZREMRANGEBYSCORE',KEYS[1],'-inf',now)
return redis.call('ZRANGE',KEYS[1],0,-1)`

// RedisConfig 定义聊天通知的隔离空间和租约，独立于消息事实数据。
type RedisConfig struct {
	Prefix                  string
	LeaseTTL, RenewInterval time.Duration
}
type notification struct {
	Source string          `json:"source"`
	Users  []int64         `json:"users"`
	Event  json.RawMessage `json:"event"`
}
type redisRelay struct {
	node   *Node
	client *goredis.Client
	cfg    RedisConfig
	id     string
	pubsub *goredis.PubSub
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{}
	once   sync.Once
	// known 仅由租约协程访问，清理失败时保留条目以便重试。
	known map[int64]bool
}

// EnableRedis 在开始接收连接前启用跨实例通知，先确认订阅再允许启动服务。
func (n *Node) EnableRedis(parent context.Context, client *goredis.Client, cfg RedisConfig) error {
	if client == nil || cfg.Prefix == "" || len(cfg.Prefix) > 100 || cfg.LeaseTTL < time.Second || cfg.LeaseTTL > 5*time.Minute || cfg.RenewInterval <= 0 || cfg.RenewInterval > cfg.LeaseTTL/3 {
		return errors.New("聊天 Redis 配置无效")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	relay := &redisRelay{node: n, client: client, cfg: cfg, id: hex.EncodeToString(random[:]), cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}), known: make(map[int64]bool)}
	relay.pubsub = client.Subscribe(ctx, relay.channel(relay.id))
	probe, stop := context.WithTimeout(ctx, 2*time.Second)
	_, err := relay.pubsub.Receive(probe)
	stop()
	if err != nil {
		cancel()
		relay.pubsub.Close()
		return fmt.Errorf("聊天通知订阅失败")
	}
	n.mu.Lock()
	if n.closed || n.relay != nil || len(n.peers) != 0 {
		n.mu.Unlock()
		cancel()
		relay.pubsub.Close()
		return errors.New("聊天 Redis 只能在连接启动前启用")
	}
	n.relay = relay
	n.mu.Unlock()
	go relay.run(ctx)
	return nil
}

// routeKey 用十进制 ID 构造当前聊天命名空间中的用户路由。
func (r *redisRelay) routeKey(user int64) string {
	return r.cfg.Prefix + "route:" + strconv.FormatInt(user, 10)
}

// channel 每个进程实例订阅独立频道，不复用已退出进程的身份。
func (r *redisRelay) channel(id string) string {
	return r.cfg.Prefix + "db:" + strconv.Itoa(r.client.Options().DB) + ":node:" + id
}

// signalRoutes 合并连接变更通知，避免每次上下线创建后台任务。
func (n *Node) signalRoutes() {
	n.mu.Lock()
	relay := n.relay
	n.mu.Unlock()
	if relay != nil {
		select {
		case relay.wake <- struct{}{}:
		default:
		}
	}
}

// localUsers 复制当前在线用户，Redis 网络调用不持有连接管理锁。
func (n *Node) localUsers() map[int64]bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	users := make(map[int64]bool)
	for user, peers := range n.peers {
		if len(peers) > 0 {
			users[user] = true
		}
	}
	return users
}

// reconcile 在单协程中先续期当前用户，再清理离线用户；连接变更触发下一轮校正。
func (r *redisRelay) reconcile(parent context.Context) {
	users := r.node.localUsers()
	// 每条命令都有短超时；不因 Redis 故障阻塞 WebSocket 读写或消息 ACK。
	for user := range users {
		r.known[user] = true
		ctx, cancel := context.WithTimeout(parent, 500*time.Millisecond)
		err := r.client.Eval(ctx, leaseScript, []string{r.routeKey(user)}, r.id, r.cfg.LeaseTTL.Milliseconds()).Err()
		cancel()
		if err != nil {
			if parent.Err() != nil {
				return
			}
			slog.Warn("聊天路由续期失败")
			return
		}
	}
	for user := range r.known {
		if !users[user] {
			ctx, cancel := context.WithTimeout(parent, 500*time.Millisecond)
			err := r.client.ZRem(ctx, r.routeKey(user), r.id).Err()
			cancel()
			if err != nil {
				if parent.Err() != nil {
					return
				}
				slog.Warn("聊天离线路由清理失败")
				return
			}
			delete(r.known, user)
		}
	}
}

// run 管理订阅与租约生命周期，订阅断线由 go-redis 重订阅，遗漏通知不回放。
func (r *redisRelay) run(ctx context.Context) {
	defer close(r.done)
	messages := r.pubsub.Channel(goredis.WithChannelSize(128), goredis.WithChannelSendTimeout(100*time.Millisecond))
	receiverDone := make(chan struct{})
	go func() {
		defer close(receiverDone)
		for {
			select {
			case <-ctx.Done():
				return
			case message, ok := <-messages:
				if !ok {
					return
				}
				r.accept(message.Payload)
			}
		}
	}()
	ticker := time.NewTicker(r.cfg.RenewInterval)
	defer ticker.Stop()
	defer func() {
		_ = r.pubsub.Close()
		<-receiverDone
		// 清理只移除当前进程的成员，不删除其他 Node 的租约；失败由 TTL 兜底。
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		pipe := r.client.Pipeline()
		for user := range r.known {
			pipe.ZRem(cleanup, r.routeKey(user), r.id)
		}
		if len(r.known) > 0 {
			_, _ = pipe.Exec(cleanup)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reconcile(ctx)
		case <-r.wake:
			r.reconcile(ctx)
		}
	}
}

// accept 严格校验内部通知格式，仅按指定用户分发本地连接，禁止广播所有连接。
func (r *redisRelay) accept(payload string) {
	if len(payload) > 32<<10 {
		return
	}
	var notice notification
	if strictJSON([]byte(payload), &notice) != nil || notice.Source == r.id || len(notice.Source) != 32 || len(notice.Users) == 0 || len(notice.Users) > 2 {
		return
	}
	if _, err := hex.DecodeString(notice.Source); err != nil {
		return
	}
	var event frame
	if strictJSON(notice.Event, &event) != nil || (event.Type != "chat.message" && event.Type != "chat.read") || len(event.Data) == 0 {
		return
	}
	for _, user := range notice.Users {
		if user <= 0 {
			return
		}
	}
	r.node.deliver(notice.Users, notice.Event, nil)
}

// forward 查询当前活跃路由并按目标节点合并发布，始终排除已完成本地分发的节点。
func (r *redisRelay) forward(parent context.Context, users []int64, event []byte) error {
	ctx, cancel := context.WithTimeout(parent, 500*time.Millisecond)
	defer cancel()
	targets := make(map[string][]int64)
	for _, user := range users {
		nodes, err := r.client.Eval(ctx, routesScript, []string{r.routeKey(user)}).StringSlice()
		if err != nil {
			return err
		}
		for _, id := range nodes {
			if id != r.id {
				targets[id] = append(targets[id], user)
			}
		}
	}
	for id, recipients := range targets {
		data, err := json.Marshal(notification{Source: r.id, Users: recipients, Event: event})
		if err != nil {
			return err
		}
		if err = r.client.Publish(ctx, r.channel(id), data).Err(); err != nil {
			return err
		}
	}
	return nil
}

// deliver 只访问本地连接；同一用户多设备均推送，排除发起请求的当前连接。
func (n *Node) deliver(users []int64, event []byte, exclude *peer) {
	n.mu.Lock()
	defer n.mu.Unlock()
	seen := make(map[int64]bool)
	for _, user := range users {
		if seen[user] {
			continue
		}
		seen[user] = true
		for target := range n.peers[user] {
			if target != exclude {
				n.enqueue(target, event)
			}
		}
	}
}

// notify 先完成独立于 Redis 的本地推送，再尝试跨实例通知；失败不改变已提交结果。
func (n *Node) notify(ctx context.Context, users []int64, event []byte, exclude *peer) {
	n.deliver(users, event, exclude)
	n.mu.Lock()
	relay := n.relay
	n.mu.Unlock()
	if relay != nil {
		if err := relay.forward(ctx, users, event); err != nil {
			slog.Warn("聊天跨实例通知失败，消息可从历史恢复")
		}
	}
}

// close 停止订阅和续期，调用方在完成消息操作及本地连接释放后执行。
func (r *redisRelay) close() { r.once.Do(func() { r.cancel(); <-r.done }) }
