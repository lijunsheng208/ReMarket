// Package node 管理单实例 WebSocket 连接；消息业务通过独立验签的 Chat RPC 执行。
package node

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	chatv1 "remarket/backend/gen/chat/v1"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/grpcx"
)

type ticket struct {
	raw     string
	expires time.Time
}
type peer struct {
	ws      *websocket.Conn
	queue   chan []byte
	user    int64
	raw     string
	expires time.Time
}
type SessionChecker interface {
	GetCurrentUser(context.Context, *userv1.GetCurrentUserRequest, ...grpc.CallOption) (*userv1.UserResponse, error)
}

// Observer 只接收固定结果和时长，不接收用户、会话、正文或认证凭据。
type Observer interface {
	HandshakeFinished(bool, time.Duration)
	ConnectionOpened() func()
	SendFinished(codes.Code)
}
type Node struct {
	users        SessionChecker
	observer     Observer
	relay        *redisRelay
	mu           sync.Mutex
	tickets      map[[32]byte]ticket
	peers        map[int64]map[*peer]struct{}
	closed       bool
	wg           sync.WaitGroup
	chats        chatv1.ChatServiceClient
	verifier     auth.TokenVerifier
	origins      map[string]bool
	pingInterval time.Duration
	pongWait     time.Duration
	writeWait    time.Duration
}
type frame struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

var requestID = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)

// New 建立本地连接管理器，允许来源须为完整 scheme://host，不信任转发 Host 头。
func New(chats chatv1.ChatServiceClient, users SessionChecker, verifier auth.TokenVerifier, origins []string, observers ...Observer) *Node {
	n := &Node{users: users, tickets: make(map[[32]byte]ticket), peers: make(map[int64]map[*peer]struct{}), chats: chats, verifier: verifier, origins: make(map[string]bool), pingInterval: 20 * time.Second, pongWait: 60 * time.Second, writeWait: 5 * time.Second}
	if len(observers) > 0 {
		n.observer = observers[0]
	}
	for _, origin := range origins {
		if origin = strings.TrimSpace(origin); origin != "" {
			n.origins[origin] = true
		}
	}
	return n
}

// OriginAllowed 要求浏览器 Origin 与当前服务同源或匹配显式白名单；拒绝缺失与多值。
func (n *Node) OriginAllowed(r *http.Request) bool {
	values := r.Header.Values("Origin")
	if len(values) != 1 {
		return false
	}
	u, err := url.Parse(values[0])
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if n.origins[values[0]] {
		return true
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return u.Scheme == scheme && u.Host == r.Host
}

// Issue 签发 30 秒内有效的一次性随机票据；只存哈希键，容量有界，不写日志。
func (n *Node) Issue(raw string) (string, time.Time, error) {
	claims, err := n.verifier.VerifyAccessToken(raw)
	if err != nil {
		return "", time.Time{}, auth.ErrInvalidToken
	}
	expires := time.Now().Add(30 * time.Second)
	if claims.ExpiresAt.Time.Before(expires) {
		expires = claims.ExpiresAt.Time
	}
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return "", time.Time{}, err
	}
	value := base64.RawURLEncoding.EncodeToString(random[:])
	key := sha256.Sum256([]byte(value))
	n.mu.Lock()
	defer n.mu.Unlock()
	for k, t := range n.tickets {
		if !time.Now().Before(t.expires) {
			delete(n.tickets, k)
		}
	}
	if n.closed || len(n.tickets) >= 4096 {
		return "", time.Time{}, errors.New("票据服务暂不可用")
	}
	n.tickets[key] = ticket{raw: raw, expires: expires}
	return value, expires, nil
}

// take 原子消费票据，即使后续握手失败也不能复用。
func (n *Node) take(value string) (ticket, bool) {
	if len(value) != 43 {
		return ticket{}, false
	}
	key := sha256.Sum256([]byte(value))
	n.mu.Lock()
	defer n.mu.Unlock()
	t, ok := n.tickets[key]
	delete(n.tickets, key)
	return t, ok && !n.closed && time.Now().Before(t.expires)
}

// ServeHTTP 消费票据后重新验签，连接不能超过 JWT 到期时间。
func (n *Node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	accepted := false
	defer func() {
		if !accepted && n.observer != nil {
			n.observer.HandshakeFinished(false, time.Since(start))
		}
	}()

	if !n.OriginAllowed(r) {
		http.Error(w, "来源不允许", http.StatusForbidden)
		return
	}
	values := r.URL.Query()["ticket"]
	if len(values) != 1 {
		http.Error(w, "无效票据", 401)
		return
	}
	t, ok := n.take(values[0])
	if !ok {
		http.Error(w, "无效票据", 401)
		return
	}
	claims, err := n.verifier.VerifyAccessToken(t.raw)
	if err != nil {
		http.Error(w, "无效凭据", 401)
		return
	}
	user, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || user <= 0 {
		http.Error(w, "无效凭据", 401)
		return
	}
	// 握手前实时核实账号状态，票据不替代身份或账号有效性校验。
	checkCtx, checkCancel := context.WithTimeout(r.Context(), 3*time.Second)
	checkCtx = metadata.NewOutgoingContext(checkCtx, metadata.Pairs("authorization", "Bearer "+t.raw))
	_, checkErr := n.users.GetCurrentUser(checkCtx, &userv1.GetCurrentUserRequest{})
	checkCancel()
	if checkErr != nil {
		http.Error(w, "账号验证失败", http.StatusUnauthorized)
		return
	}
	upgrader := websocket.Upgrader{HandshakeTimeout: 5 * time.Second, CheckOrigin: n.OriginAllowed}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	accepted = true
	if n.observer != nil {
		n.observer.HandshakeFinished(true, time.Since(start))
	}
	p := &peer{ws: ws, queue: make(chan []byte, 64), user: user, raw: t.raw, expires: claims.ExpiresAt.Time}
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		ws.Close()
		return
	}
	if n.peers[user] == nil {
		n.peers[user] = make(map[*peer]struct{})
	}
	if len(n.peers[user]) >= 8 {
		n.mu.Unlock()
		ws.Close()
		return
	}
	n.peers[user][p] = struct{}{}
	n.wg.Add(1)
	n.mu.Unlock()
	n.signalRoutes()
	defer n.wg.Done()
	if n.observer != nil {
		finishConnection := n.observer.ConnectionOpened()
		defer finishConnection()
	}
	// 关闭网络连接解除读写阻塞，等待写循环退出后才释放 peer。
	ctx, cancel := context.WithCancel(r.Context())
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); n.write(ctx, p) }()
	defer func() {
		cancel()
		ws.Close()
		<-writerDone
		n.mu.Lock()
		delete(n.peers[user], p)
		if len(n.peers[user]) == 0 {
			delete(n.peers, user)
		}
		n.mu.Unlock()
		n.signalRoutes()
	}()
	ws.SetReadLimit(16 << 10)
	_ = ws.SetReadDeadline(time.Now().Add(n.pongWait))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(n.pongWait)) })
	for {
		kind, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.TextMessage {
			n.failure(p, "", "InvalidArgument", "只支持文本 JSON")
			continue
		}
		n.receive(ctx, p, data)
	}
}

// write 是应用消息的唯一写协程；控制帧与关闭按库的并发约定处理。
func (n *Node) write(ctx context.Context, p *peer) {
	ticker := time.NewTicker(n.pingInterval)
	defer ticker.Stop()
	expiry := time.NewTimer(time.Until(p.expires))
	defer expiry.Stop()
	defer p.ws.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case <-expiry.C:
			return
		case data := <-p.queue:
			_ = p.ws.SetWriteDeadline(time.Now().Add(n.writeWait))
			if p.ws.WriteMessage(websocket.TextMessage, data) != nil {
				return
			}
		case <-ticker.C:
			if p.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
				return
			}
		}
	}
}

// enqueue 使用有界队列；慢连接关闭后由客户端查询持久化消息恢复。
func (n *Node) enqueue(p *peer, data []byte) {
	select {
	case p.queue <- data:
	default:
		p.ws.Close()
	}
}

// emit 编码统一事件，正文保留 Proto JSON 的字符串大整数。
func (n *Node) emit(p *peer, kind, id string, data any) {
	body, err := json.Marshal(data)
	if err != nil {
		p.ws.Close()
		return
	}
	encoded, err := json.Marshal(frame{Type: kind, RequestID: id, Data: body})
	if err != nil {
		p.ws.Close()
		return
	}
	n.enqueue(p, encoded)
}

// failure 只发送经过错误映射的公开信息。
func (n *Node) failure(p *peer, id, code, message string) {
	n.emit(p, "chat.error", id, map[string]string{"code": code, "message": message})
}

// receive 校验协议并调用 Chat；ACK 先排入当前连接，通知随后尽力分发。
func (n *Node) receive(parent context.Context, p *peer, data []byte) {
	var f frame
	if strictJSON(data, &f) != nil || !requestID.MatchString(f.RequestID) {
		n.failure(p, "", "InvalidArgument", "消息格式无效")
		return
	}
	if f.Type == "ping" {
		n.emit(p, "pong", f.RequestID, struct{}{})
		return
	}
	if f.Type == "chat.read" {
		n.read(parent, p, f)
		return
	}
	if f.Type != "chat.send" {
		n.failure(p, f.RequestID, "InvalidArgument", "未知消息类型")
		return
	}
	var input struct {
		ConversationID string `json:"conversation_id"`
		ClientMsgID    string `json:"client_msg_id"`
		Content        string `json:"content"`
	}
	if strictJSON(f.Data, &input) != nil {
		n.failure(p, f.RequestID, "InvalidArgument", "消息参数无效")
		return
	}
	conversation, err := strconv.ParseInt(input.ConversationID, 10, 64)
	if err != nil || conversation <= 0 {
		n.failure(p, f.RequestID, "InvalidArgument", "会话 ID 无效")
		return
	}
	// 每次操作重验原 Token，禁止长连接绕过令牌过期；下游再核实账号状态。
	if _, err = n.verifier.VerifyAccessToken(p.raw); err != nil {
		p.ws.Close()
		return
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	ctx = grpcx.WithRequestID(ctx, f.RequestID)
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+p.raw))
	result, err := n.chats.SendMessage(ctx, &chatv1.SendMessageRequest{ConversationId: conversation, ClientMsgId: input.ClientMsgID, Content: input.Content})
	if err != nil {
		st := status.Convert(grpcx.Error(err))
		if n.observer != nil {
			n.observer.SendFinished(st.Code())
		}
		n.failure(p, f.RequestID, st.Code().String(), st.Message())
		return
	}
	if result.GetMessage() == nil {
		if n.observer != nil {
			n.observer.SendFinished(codes.Internal)
		}
		n.failure(p, f.RequestID, "Internal", "服务内部错误")
		return
	}
	if n.observer != nil {
		n.observer.SendFinished(codes.OK)
	}
	m := result.Message
	n.emit(p, "chat.ack", f.RequestID, map[string]string{"message_id": strconv.FormatInt(m.Id, 10), "conversation_id": strconv.FormatInt(m.ConversationId, 10), "seq": strconv.FormatInt(m.Seq, 10), "client_msg_id": m.ClientMsgId})
	body, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		return
	}
	// message_id 与 ACK 对齐；历史接口仍保持 Message.id。
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return
	}
	payload["message_id"] = payload["id"]
	delete(payload, "id")
	encodedBody, _ := json.Marshal(payload)
	event, _ := json.Marshal(frame{Type: "chat.message", Data: encodedBody})
	n.notify(parent, result.ParticipantIds, event, p)
}

// strictJSON 拒绝未知字段、尾随内容及空载荷。
func strictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("多余 JSON")
	}
	return nil
}

// Close 停止票据签发并关闭所有长连接，等待读写协程释放后返回。
func (n *Node) Close() {
	n.mu.Lock()
	n.closed = true
	clear(n.tickets)
	for _, peers := range n.peers {
		for p := range peers {
			p.ws.Close()
		}
	}
	n.mu.Unlock()
	n.wg.Wait()
	n.mu.Lock()
	relay := n.relay
	n.mu.Unlock()
	if relay != nil {
		relay.close()
	}
}
