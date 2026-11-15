package node

import (
	"context"
	"encoding/json"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	chatv1 "remarket/backend/gen/chat/v1"
	"remarket/backend/pkg/grpcx"
	"strconv"
	"time"
)

// read 为已读请求校验参数、独立传递凭据，ACK 只使用持久化后的真实游标。
func (n *Node) read(parent context.Context, p *peer, f frame) {
	var input struct {
		ConversationID string  `json:"conversation_id"`
		LastReadSeq    *string `json:"last_read_seq"`
	}
	if strictJSON(f.Data, &input) != nil || input.LastReadSeq == nil {
		n.failure(p, f.RequestID, "InvalidArgument", "已读参数无效")
		return
	}
	id, err := strconv.ParseInt(input.ConversationID, 10, 64)
	if err != nil || id <= 0 {
		n.failure(p, f.RequestID, "InvalidArgument", "会话 ID 无效")
		return
	}
	seq, err := strconv.ParseInt(*input.LastReadSeq, 10, 64)
	if err != nil || seq < 0 {
		n.failure(p, f.RequestID, "InvalidArgument", "已读位置无效")
		return
	}
	if _, err = n.verifier.VerifyAccessToken(p.raw); err != nil {
		p.ws.Close()
		return
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	ctx = grpcx.WithRequestID(ctx, f.RequestID)
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+p.raw))
	result, err := n.chats.ReadConversation(ctx, &chatv1.ReadConversationRequest{ConversationId: id, LastReadSeq: seq})
	if err != nil {
		st := status.Convert(grpcx.Error(err))
		n.failure(p, f.RequestID, st.Code().String(), st.Message())
		return
	}
	if result.GetState() == nil {
		n.failure(p, f.RequestID, "Internal", "服务内部错误")
		return
	}
	body, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}.Marshal(result.State)
	if err != nil {
		n.failure(p, f.RequestID, "Internal", "服务内部错误")
		return
	}
	n.emit(p, "chat.read.ack", f.RequestID, json.RawMessage(body))
	n.notifyRead(parent, result, p)
}

// NotifyRead 将 HTTP 已提交的已读状态通知本地及远端参与者，不公开他人的未读计数。
func (n *Node) NotifyRead(ctx context.Context, result *chatv1.ReadConversationResponse) {
	n.notifyRead(ctx, result, nil)
}

// notifyRead 仅发布游标事实，多设备应取最大值合并，不能依赖通知到达顺序。
func (n *Node) notifyRead(ctx context.Context, result *chatv1.ReadConversationResponse, exclude *peer) {
	state := result.GetState()
	if state == nil {
		return
	}
	body, _ := json.Marshal(map[string]string{"conversation_id": strconv.FormatInt(state.ConversationId, 10), "user_id": strconv.FormatInt(state.UserId, 10), "last_read_seq": strconv.FormatInt(state.LastReadSeq, 10)})
	event, _ := json.Marshal(frame{Type: "chat.read", Data: body})
	n.notify(ctx, result.ParticipantIds, event, exclude)
}
