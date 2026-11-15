package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	feedv1 "remarket/backend/gen/feed/v1"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/feed/timeline"
	"sort"
	"strings"
	"time"
)

type Timeline interface {
	Candidates(context.Context, int64, []int64, string) ([]int64, string, error)
	Seed(context.Context, int64, string, []*productv1.Product) error
	EnsureModes(context.Context, []int64) error
}

// WithTimeline 允许显式开启 Redis 混合读取；未配置时保留 MySQL 基础路径。
func (s *Service) WithTimeline(cache Timeline) *Service { s.cache = cache; return s }

// relationFingerprint 包含关系主键，取消后重新关注也会触发历史窗口回填。
func relationFingerprint(relations []string) string {
	sort.Strings(relations)
	sum := sha256.Sum256([]byte(strings.Join(relations, ",")))
	return fmt.Sprintf("%x", sum)
}

// followingCache 合并收件箱与作者候选，必须经 Product 当前状态及关注卖家集合复核。
func (s *Service) followingCache(ctx context.Context, user int64, sellers []int64, fingerprint string, r *feedv1.ListFeedRequest) (*feedv1.ListFeedResponse, bool) {
	probe, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	ids, floor, err := s.cache.Candidates(probe, user, sellers, fingerprint)
	cancel()
	if err != nil || len(ids) == 0 {
		return nil, false
	}
	result, err := s.products.ListFeedProducts(ctx, &productv1.ListFeedProductsRequest{Cursor: r.Cursor, PageSize: r.PageSize, SellerIds: sellers, CandidateIds: ids})
	if err != nil {
		return nil, false
	}
	// 下一页和安全下界共同证明当前页没有落入缓存裁剪范围；不足一页继续 MySQL 查询而非提前结束。
	if result.NextCursor == "" || len(result.Products) == 0 || timeline.Boundary(result.Products) <= floor {
		return nil, false
	}
	return &feedv1.ListFeedResponse{Products: result.Products, NextCursor: result.NextCursor}, true
}
