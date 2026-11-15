package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"remarket/backend/internal/product/model"
	"remarket/backend/pkg/apperror"
	"time"
)

type FeedRepository interface {
	ListFeed(context.Context, time.Time, int64, int, []int64) ([]model.Product, error)
}
type FeedCandidateRepository interface {
	ListFeedCandidates(context.Context, time.Time, int64, int, []int64, []int64) ([]model.Product, error)
}
type FeedQuery struct{ repo FeedRepository }
type feedCursor struct {
	Time time.Time `json:"t"`
	ID   int64     `json:"id"`
}

// NewFeedQuery 创建以 MySQL 为事实来源的游标查询。
func NewFeedQuery(repo FeedRepository) *FeedQuery { return &FeedQuery{repo: repo} }

// List 校验游标和筛选条件，多取一条判断是否存在下一页；游标不授予任何权限。
func (s *FeedQuery) List(ctx context.Context, cursor string, size int, sellers []int64, candidates ...int64) ([]model.Product, string, error) {
	invalid := &apperror.Error{Kind: apperror.Invalid, Message: "Feed 分页参数无效"}
	if size < 0 || size > 100 || len(cursor) > 512 || len(sellers) > 10000 || len(candidates) > 2000 {
		return nil, "", invalid
	}
	if size == 0 {
		size = 20
	}
	c := feedCursor{}
	if cursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil {
			return nil, "", invalid
		}
		if json.Unmarshal(b, &c) != nil || c.ID <= 0 || c.Time.IsZero() || c.Time.Nanosecond()%1000 != 0 {
			return nil, "", invalid
		}
	}
	for _, id := range sellers {
		if id <= 0 {
			return nil, "", invalid
		}
	}
	var rows []model.Product
	var err error
	if len(candidates) > 0 {
		for _, id := range candidates {
			if id <= 0 {
				return nil, "", invalid
			}
		}
		repo, ok := s.repo.(FeedCandidateRepository)
		if !ok {
			return nil, "", &apperror.Error{Kind: apperror.Unavailable, Message: "候选查询不可用"}
		}
		rows, err = repo.ListFeedCandidates(ctx, c.Time, c.ID, size+1, sellers, candidates)
	} else {
		rows, err = s.repo.ListFeed(ctx, c.Time, c.ID, size+1, sellers)
	}
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > size {
		rows = rows[:size]
		last := rows[len(rows)-1]
		b, _ := json.Marshal(feedCursor{Time: last.CreatedAt, ID: last.ID})
		next = base64.RawURLEncoding.EncodeToString(b)
	}
	return rows, next, nil
}
