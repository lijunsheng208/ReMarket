package service

import (
	"context"
	"remarket/backend/internal/chat/repository"
)

// Read 校验游标后核实最新账号身份，事务内再校验会话成员及已保存序号上界。
func (s *Service) Read(ctx context.Context, id, seq int64) (*repository.ReadResult, error) {
	if id <= 0 || seq < 0 {
		return nil, invalid()
	}
	ctx, cancel, user, err := s.session(ctx)
	defer cancel()
	if err != nil {
		return nil, err
	}
	return s.store.Read(ctx, user, id, seq)
}

// Sync 支持从零开始补拉，以固定上界分页；只有已收到的消息才能推进客户端连续游标。
func (s *Service) Sync(ctx context.Context, id, after, until int64, size int) (*repository.SyncPage, bool, error) {
	size, err := pageSize(size)
	if err != nil {
		return nil, false, err
	}
	if id <= 0 || after < 0 || until < 0 || (until > 0 && until < after) {
		return nil, false, invalid()
	}
	ctx, cancel, user, err := s.session(ctx)
	defer cancel()
	if err != nil {
		return nil, false, err
	}
	page, err := s.store.Sync(ctx, user, id, after, until, size+1)
	if err != nil {
		return nil, false, err
	}
	more := len(page.Messages) > size
	if more {
		page.Messages = page.Messages[:size]
	}
	return page, more, nil
}
