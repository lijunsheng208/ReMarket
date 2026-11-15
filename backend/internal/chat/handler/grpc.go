// Package handler 适配 Chat gRPC，不承担业务授权规则。
package handler

import (
	"context"
	"google.golang.org/protobuf/types/known/timestamppb"
	chatv1 "remarket/backend/gen/chat/v1"
	"remarket/backend/internal/chat/model"
	"remarket/backend/internal/chat/service"
)

type Server struct {
	chatv1.UnimplementedChatServiceServer
	service *service.Service
}

// New 注入聊天业务服务。
func New(s *service.Service) *Server { return &Server{service: s} }

// conversation 转换领域会话，可空字段保留未发送消息语义。
func conversation(c *model.Conversation) *chatv1.Conversation {
	out := &chatv1.Conversation{Id: c.ID, ProductId: c.ProductID, BuyerId: c.BuyerID, SellerId: c.SellerID, LastSeq: c.LastSeq, CreatedAt: timestamppb.New(c.CreatedAt)}
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
	rows, more, err := s.service.Messages(ctx, r.GetConversationId(), r.GetBeforeSeq(), r.GetAfterSeq(), int(r.GetPageSize()))
	if err != nil {
		return nil, err
	}
	out := &chatv1.ListMessagesResponse{HasMore: more}
	for _, m := range rows {
		out.Messages = append(out.Messages, &chatv1.Message{Id: m.ID, ConversationId: m.ConversationID, SenderId: m.SenderID, ClientMsgId: m.ClientMsgID, Seq: m.Seq, Content: m.Content, MessageType: m.MessageType, CreatedAt: timestamppb.New(m.CreatedAt)})
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
	return &chatv1.SendMessageResponse{Message: &chatv1.Message{Id: m.ID, ConversationId: m.ConversationID, SenderId: m.SenderID, ClientMsgId: m.ClientMsgID, Seq: m.Seq, Content: m.Content, MessageType: m.MessageType, CreatedAt: timestamppb.New(m.CreatedAt)}, ParticipantIds: result.Participants}, nil
}
