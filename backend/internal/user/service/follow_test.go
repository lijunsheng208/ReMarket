package service

import (
	"context"
	"remarket/backend/internal/user/model"
	"remarket/backend/internal/user/repository"
	"remarket/backend/pkg/auth"
	"strings"
	"sync"
	"testing"
	"time"
)

type followMemory struct {
	mu    sync.Mutex
	pairs map[int64]int64
}

// GetByID 返回启用账号供业务规则测试。
func (m *followMemory) GetByID(ctx context.Context, id int64) (*model.User, error) {
	return &model.User{ID: id, Status: model.StatusActive}, nil
}

// SetFollow 以锁模拟数据库唯一键的原子写入。
func (m *followMemory) SetFollow(ctx context.Context, a, b int64, on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if on {
		m.pairs[b] = a
	} else {
		delete(m.pairs, b)
	}
	return nil
}

// ListFollows 返回固定记录以验证游标截断边界。
func (m *followMemory) ListFollows(context.Context, int64, int64, int, bool) ([]repository.FollowUser, error) {
	return []repository.FollowUser{{ID: 2}, {ID: 3}, {ID: 4}}, nil
}

// TestFollowRules 验证认证、自关注、并发重复操作与分页边界；不替代真实 MySQL 约束验收。
func TestFollowRules(t *testing.T) {
	repo := &followMemory{pairs: map[int64]int64{}}
	s := NewFollow(repo)
	if s.Set(context.Background(), 2, true) == nil {
		t.Fatal("匿名写入成功")
	}
	tokens, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("x", 32), Issuer: "test", Audience: "test", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := tokens.IssueAccessToken(1, "USER")
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := auth.Authenticate(context.Background(), raw, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if s.Set(ctx, 1, true) == nil {
		t.Fatal("允许自关注")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Set(ctx, 2, true); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(repo.pairs) != 1 {
		t.Fatal("重复关系")
	}
	for i := 0; i < 2; i++ {
		if err := s.Set(ctx, 2, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(repo.pairs) != 0 {
		t.Fatal("取消失败")
	}
	rows, next, err := s.List(ctx, 2, 0, 2, true)
	if err != nil || len(rows) != 2 || next != 3 {
		t.Fatalf("分页失败 %v %d", err, next)
	}
	if _, _, err := s.List(ctx, 2, 0, 101, false); err == nil {
		t.Fatal("未限制页大小")
	}
}
