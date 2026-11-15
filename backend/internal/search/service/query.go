// Package service 实现搜索参数与游标规则，不依赖 HTTP 或 gRPC。
package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"remarket/backend/internal/search/repository"
	"remarket/backend/pkg/apperror"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Store 定义快照查询边界。
type Store interface {
	OpenPIT(context.Context) (string, error)
	ClosePIT(context.Context, string)
	Search(context.Context, repository.Query, string, []json.RawMessage) (repository.Page, error)
}

// Request 包含查询与不透明分页凭据。
type Request struct {
	Query  repository.Query
	Cursor string
}

// Result 返回搜索快照，不提供购买保证。
type Result struct {
	Products []repository.Document
	Cursor   string
}
type cursor struct {
	Version int               `json:"v"`
	PIT     string            `json:"pit"`
	After   []json.RawMessage `json:"after"`
	Digest  string            `json:"digest"`
	Issued  int64             `json:"issued"`
	Expires int64             `json:"expires"`
}

// Service 保存签名密钥及单进程 PIT/请求预算，跨实例上限需部署层控制。
type Service struct {
	store    Store
	key      []byte
	mu       sync.Mutex
	sessions map[string]int64
	active   chan struct{}
	now      func() time.Time
}

// New 要求至少 32 字节专用密钥，限制单实例最多 100 个活跃 PIT 和 10 个并发查询。
func New(store Store, key []byte) (*Service, error) {
	if store == nil || len(key) < 32 {
		return nil, errors.New("搜索依赖或签名密钥无效")
	}
	return &Service{store: store, key: append([]byte(nil), key...), sessions: map[string]int64{}, active: make(chan struct{}, 10), now: time.Now}, nil
}

// invalid 返回可公开参数错误。
func invalid() error {
	return &apperror.Error{Kind: apperror.Invalid, Message: "搜索参数或游标无效"}
}

// normalize 校验业务边界，并统一默认值以绑定后续翻页条件。
func normalize(q repository.Query) (repository.Query, error) {
	q.Text = strings.TrimSpace(q.Text)
	if !utf8.ValidString(q.Text) || utf8.RuneCountInString(q.Text) > 100 || q.Size < 0 || q.Size > 100 {
		return q, invalid()
	}
	if q.Size == 0 {
		q.Size = 20
	}
	if q.CategoryID != nil && *q.CategoryID <= 0 || q.MinPrice != nil && *q.MinPrice < 0 || q.MaxPrice != nil && *q.MaxPrice < 0 || q.MinPrice != nil && q.MaxPrice != nil && *q.MinPrice > *q.MaxPrice {
		return q, invalid()
	}
	if q.Sort == "" {
		q.Sort = "newest"
		if q.Text != "" {
			q.Sort = "relevance"
		}
	}
	switch q.Sort {
	case "relevance":
		if q.Text == "" {
			return q, invalid()
		}
	case "newest", "price_asc", "price_desc":
	default:
		return q, invalid()
	}
	return q, nil
}

// sign 对游标载荷签名，不在日志中记录载荷。
func (s *Service) sign(raw []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	_, _ = m.Write(raw)
	return m.Sum(nil)
}

// encode 保留 sort 原始数字，生成带完整性保护的不透明游标。
func (s *Service) encode(c cursor) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(s.sign(raw))
	if len(token) > 16<<10 {
		return "", errors.New("搜索游标过大")
	}
	return token, nil
}

// decode 先验签再解析，过期与参数不匹配分别返回明确错误。
func (s *Service) decode(token, digest string) (cursor, error) {
	var c cursor
	if len(token) > 16<<10 {
		return c, invalid()
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return c, invalid()
	}
	raw, e := base64.RawURLEncoding.DecodeString(parts[0])
	sig, e2 := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil || e2 != nil || !hmac.Equal(sig, s.sign(raw)) || json.Unmarshal(raw, &c) != nil || c.Version != 1 || c.PIT == "" || len(c.After) != 3 || c.Digest != digest || c.Issued > s.now().Unix() || c.Expires-c.Issued != 300 {
		return c, invalid()
	}
	if c.Expires <= s.now().Unix() {
		return c, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "搜索已过期，请重新搜索"}
	}
	return c, nil
}

// release 删除会话额度；仅本实例记录的首请求占用额度。
func (s *Service) release(id string) { s.mu.Lock(); delete(s.sessions, id); s.mu.Unlock() }

// Search 管理快照生命周期；错误或最后一页关闭 PIT，后续页使用最新 PIT ID。
func (s *Service) Search(ctx context.Context, request Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	q, err := normalize(request.Query)
	if err != nil {
		return Result{}, err
	}
	raw, _ := json.Marshal(q)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	select {
	case s.active <- struct{}{}:
		defer func() { <-s.active }()
	default:
		return Result{}, &apperror.Error{Kind: apperror.Unavailable, Message: "搜索繁忙，请稍后重试"}
	}
	var c cursor
	if request.Cursor != "" {
		c, err = s.decode(request.Cursor, digest)
		if err != nil {
			return Result{}, err
		}
	} else {
		s.mu.Lock()
		now := s.now().Unix()
		for id, expiry := range s.sessions {
			if expiry <= now {
				delete(s.sessions, id)
			}
		}
		if len(s.sessions) >= 100 {
			s.mu.Unlock()
			return Result{}, &apperror.Error{Kind: apperror.Unavailable, Message: "搜索会话已满"}
		}
		// 占位在联网前预留，避免并发首请求突破额度。
		placeholder := string(raw) + time.Now().String()
		s.sessions[placeholder] = now + 300
		s.mu.Unlock()
		pit, openErr := s.store.OpenPIT(ctx)
		if openErr != nil {
			s.release(placeholder)
			return Result{}, mapError(openErr)
		}
		c = cursor{Version: 1, PIT: pit, Digest: digest, Issued: now, Expires: now + 300}
		s.mu.Lock()
		delete(s.sessions, placeholder)
		s.sessions[pit] = c.Expires
		s.mu.Unlock()
	}
	page, err := s.store.Search(ctx, q, c.PIT, c.After)
	if err != nil {
		s.store.ClosePIT(ctx, c.PIT)
		s.release(c.PIT)
		return Result{}, mapError(err)
	}
	old := c.PIT
	c.PIT = page.PIT
	result := Result{Products: []repository.Document{}}
	limit := len(page.Hits)
	if limit > q.Size {
		limit = q.Size
	}
	for _, h := range page.Hits[:limit] {
		result.Products = append(result.Products, h.Document)
	}
	if len(page.Hits) > q.Size {
		c.After = page.Hits[limit-1].Sort
		result.Cursor, err = s.encode(c)
		if err != nil {
			s.store.ClosePIT(ctx, c.PIT)
			s.release(old)
			return Result{}, err
		}
		s.mu.Lock()
		if _, ok := s.sessions[old]; ok {
			delete(s.sessions, old)
			s.sessions[c.PIT] = c.Expires
		}
		s.mu.Unlock()
	} else {
		s.store.ClosePIT(ctx, c.PIT)
		s.release(old)
	}
	return result, nil
}

// mapError 只映射已知存储故障，保留取消与未知错误供统一传输层处理。
func mapError(err error) error {
	if errors.Is(err, repository.ErrPITExpired) {
		return &apperror.Error{Kind: apperror.FailedPrecondition, Message: "搜索快照已失效，请重新搜索"}
	}
	if errors.Is(err, repository.ErrUnavailable) {
		return &apperror.Error{Kind: apperror.Unavailable, Message: "搜索暂不可用"}
	}
	return err
}
