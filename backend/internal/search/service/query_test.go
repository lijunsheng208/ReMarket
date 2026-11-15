package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"remarket/backend/internal/search/repository"
	"remarket/backend/pkg/apperror"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	opened, closed int
	after          []json.RawMessage
	fail           error
	page           repository.Page
}

// OpenPIT 记录首请求打开快照。
func (f *fakeStore) OpenPIT(context.Context) (string, error) { f.opened++; return "pit-one", nil }

// ClosePIT 记录结束或错误后的资源释放。
func (f *fakeStore) ClosePIT(context.Context, string) { f.closed++ }

// Search 保存游标值并返回可控的下一页。
func (f *fakeStore) Search(_ context.Context, _ repository.Query, _ string, after []json.RawMessage) (repository.Page, error) {
	f.after = after
	return f.page, f.fail
}

// TestPagination 验证精确 sort、最新 PIT、参数绑定、签名及终页资源释放。
func TestPagination(t *testing.T) {
	sort := []json.RawMessage{json.RawMessage(`100`), json.RawMessage(`9007199254740993`), json.RawMessage(`1`)}
	f := &fakeStore{page: repository.Page{PIT: "pit-two", Hits: []repository.Hit{{Document: repository.Document{ProductID: 9007199254740993}, Sort: sort}, {Document: repository.Document{ProductID: 2}, Sort: sort}}}}
	s, _ := New(f, []byte(strings.Repeat("k", 32)))
	request := Request{Query: repository.Query{Sort: "price_asc", Size: 1}}
	first, err := s.Search(context.Background(), request)
	if err != nil || first.Cursor == "" || len(first.Products) != 1 {
		t.Fatalf("首请求: %v", err)
	}
	c, err := s.decode(first.Cursor, func() string {
		parts := strings.Split(first.Cursor, ".")
		payload, _ := base64.RawURLEncoding.DecodeString(parts[0])
		var c cursor
		_ = json.Unmarshal(payload, &c)
		return c.Digest
	}())
	if err != nil || c.PIT != "pit-two" {
		t.Fatal("未使用最新 PIT")
	}
	request.Cursor = first.Cursor
	f.page.Hits = f.page.Hits[:1]
	_, err = s.Search(context.Background(), request)
	if err != nil || f.opened != 1 || f.closed != 1 || string(f.after[1]) != "9007199254740993" {
		t.Fatalf("后续页: %v", err)
	}
	request.Query.Size = 2
	if _, err = s.Search(context.Background(), request); err == nil {
		t.Fatal("更改参数未拒绝")
	}
	request.Query.Size = 1
	request.Cursor = "x" + first.Cursor
	if _, err = s.Search(context.Background(), request); err == nil {
		t.Fatal("篡改未拒绝")
	}
	s.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
	request.Cursor = first.Cursor
	if _, err = s.Search(context.Background(), request); err == nil {
		t.Fatal("过期未拒绝")
	}
}

// TestValidationAndFailures 验证价格边界、非法排序及存储故障映射。
func TestValidationAndFailures(t *testing.T) {
	minus := int64(-1)
	for _, q := range []repository.Query{{Size: 101}, {CategoryID: &minus}, {MinPrice: &minus}, {Sort: "bad"}, {Sort: "relevance"}, {Text: strings.Repeat("字", 101)}} {
		if _, err := normalize(q); err == nil {
			t.Fatal("无效参数被接受")
		}
	}
	f := &fakeStore{fail: repository.ErrUnavailable}
	s, _ := New(f, []byte(strings.Repeat("k", 32)))
	_, err := s.Search(context.Background(), Request{})
	var app *apperror.Error
	if !errors.As(err, &app) || app.Kind != apperror.Unavailable || f.closed != 1 {
		t.Fatal("存储错误未映射/释放")
	}
}

// TestConcurrentBudget 验证并发首请求仍受会话额度约束，使用 race 检查共享状态。
func TestConcurrentBudget(t *testing.T) {
	f := &concurrentStore{}
	s, _ := New(f, []byte(strings.Repeat("k", 32)))
	var group sync.WaitGroup
	for i := 0; i < 30; i++ {
		group.Add(1)
		go func() { defer group.Done(); _, _ = s.Search(context.Background(), Request{}) }()
	}
	group.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) > 100 {
		t.Fatal("会话额度越界")
	}
}

type concurrentStore struct{}

// OpenPIT 为并发测试返回独立于可变状态的固定快照。
func (*concurrentStore) OpenPIT(context.Context) (string, error) { return "pit", nil }

// ClosePIT 是无共享可变状态的资源释放替身。
func (*concurrentStore) ClosePIT(context.Context, string) {}

// Search 为并发测试返回终页。
func (*concurrentStore) Search(context.Context, repository.Query, string, []json.RawMessage) (repository.Page, error) {
	return repository.Page{PIT: "pit"}, nil
}
