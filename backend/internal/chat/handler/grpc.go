// Package handler 适配 Chat gRPC，不承担业务授权规则。
package handler

import (
	"context"
	"google.golang.org/protobuf/types/known/timestamppb"
	chatv1 "remarket/backend/gen/chat/v1"
	"remarket/backend/internal/chat/model"
	"remarket/backend/internal/chat/service"
	"remarket/backend/pkg/apperror"
)

type Server struct {
	chatv1.UnimplementedChatServiceServer
	service *service.Service
}

// New 注入聊天业务服务。
func New(s *service.Service) *Server { return &Server{service: s} }

// conversation 转换领域会话，可空字段保留未发送消息语义。
func conversation(c *model.Conversation) *chatv1.Conversation {
	out := &chatv1.Conversation{Id: c.ID, ProductId: c.ProductID, BuyerId: c.BuyerID, SellerId: c.SellerID, LastSeq: c.LastSeq, LastReadSeq: c.LastReadSeq, UnreadCount: c.UnreadCount, LastMessageContent: c.LastMessageContent, CreatedAt: timestamppb.New(c.CreatedAt)}
	if c.LastMessageID != nil {
		out.LastMessageId = *c.LastMessageID
	}
	if c.LastMessageAt != nil {
		out.LastMessageAt = timestamppb.New(*c.LastMessageAt)
	}
	return out
}

// CreateConversation 只接收商品 ID，参与者由业务层决定。
func (s *Server) CreateConversation(ctx context.Context, r *chatv1.CreateConversationRequest) (*chatv1.Conversation, error) {
	c, err := s.service.Create(ctx, r.GetProductId())
	if err != nil {
		return nil, err
	}
	return conversation(c), nil
}

// ListConversations 返回当前账号可见会话及下一页游标。
func (s *Server) ListConversations(ctx context.Context, r *chatv1.ListConversationsRequest) (*chatv1.ListConversationsResponse, error) {
	rows, next, err := s.service.List(ctx, r.GetCursor(), int(r.GetPageSize()))
	if err != nil {
		return nil, err
	}
	out := &chatv1.ListConversationsResponse{NextCursor: next}
	for i := range rows {
		out.Conversations = append(out.Conversations, conversation(&rows[i]))
	}
	return out, nil
}

// ListMessages 返回成员可见消息，next_seq 为最后一条序号。
func (s *Server) ListMessages(ctx context.Context, r *chatv1.ListMessagesRequest) (*chatv1.ListMessagesResponse, error) {
	if r == nil {
		r = &chatv1.ListMessagesRequest{}
	}
	if r.GetAfterSeq() < 0 || r.GetSyncUntilSeq() < 0 || (r.AfterSeq != nil && r.GetBeforeSeq() != 0) || (r.AfterSeq == nil && r.GetSyncUntilSeq() != 0) {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "聊天分页参数无效"}
	}
	out := &chatv1.ListMessagesResponse{}
	var rows []model.Message
	if r.AfterSeq != nil {
		page, more, err := s.service.Sync(ctx, r.GetConversationId(), r.GetAfterSeq(), r.GetSyncUntilSeq(), int(r.GetPageSize()))
		if err != nil {
			return nil, err
		}
		rows = page.Messages
		out.HasMore = more
		out.SyncUntilSeq = page.Until
		out.NextSeq = r.GetAfterSeq()
	} else {
		var err error
		rows, out.HasMore, err = s.service.Messages(ctx, r.GetConversationId(), r.GetBeforeSeq(), 0, int(r.GetPageSize()))
		if err != nil {
			return nil, err
		}
	}
	for _, m := range rows {
		out.Messages = append(out.Messages, message(m))
		out.NextSeq = m.Seq
	}

	return out, nil
}

// SendMessage 返回已持久化结果；传输断开后客户端可使用同一幂等键重试。
func (s *Server) SendMessage(ctx context.Context, r *chatv1.SendMessageRequest) (*chatv1.SendMessageResponse, error) {
	result, err := s.service.Send(ctx, r.GetConversationId(), r.GetClientMsgId(), r.GetContent())
	if err != nil {
		return nil, err
	}
	m := result.Message
	return &chatv1.SendMessageResponse{Message: message(m), ParticipantIds: result.Participants}, nil
}

// message 将领域消息转换为字符串 BIGINT 的 Proto JSON 数据来源。
func message(m model.Message) *chatv1.Message {
	return &chatv1.Message{Id: m.ID, ConversationId: m.ConversationID, SenderId: m.SenderID, ClientMsgId: m.ClientMsgID, Seq: m.Seq, Content: m.Content, MessageType: m.MessageType, CreatedAt: timestamppb.New(m.CreatedAt)}
}

// ReadConversation 返回已提交的真实游标，不把客户端请求位置直接当作最终已读位置。
func (s *Server) ReadConversation(ctx context.Context, r *chatv1.ReadConversationRequest) (*chatv1.ReadConversationResponse, error) {
	result, err := s.service.Read(ctx, r.GetConversationId(), r.GetLastReadSeq())
	if err != nil {
		return nil, err
	}
	state := result.State
	return &chatv1.ReadConversationResponse{State: &chatv1.ReadState{ConversationId: state.ConversationID, UserId: state.UserID, LastReadSeq: state.LastReadSeq, UnreadCount: state.UnreadCount, LastSeq: state.LastSeq}, ParticipantIds: result.Participants}, nil
}
