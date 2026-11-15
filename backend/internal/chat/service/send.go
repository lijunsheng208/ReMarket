package service

import (
	"context"
	"regexp"
	"remarket/backend/internal/chat/repository"
	"strings"
	"unicode/utf8"
)

var clientMessageID = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,64}$`)

// Send 校验文本消息后核实账号，发送者仅取自可信身份；不修改原始正文。
func (s *Service) Send(ctx context.Context, conversation int64, key, content string) (*repository.SendResult, error) {
	if conversation <= 0 || !clientMessageID.MatchString(key) || !utf8.ValidString(content) || strings.TrimSpace(content) == "" || utf8.RuneCountInString(content) > 2000 || len(content) > 8000 {
		return nil, invalid()
	}
	ctx, cancel, user, err := s.session(ctx)
	defer cancel()
	if err != nil {
		return nil, err
	}
	return s.store.Send(ctx, user, conversation, key, content)
}
