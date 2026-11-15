// Package service 编排 Feed 查询，不直接访问其他服务数据库。
package service

import (
	"context"
	"errors"
	"fmt"
	"google.golang.org/grpc/metadata"
	feedv1 "remarket/backend/gen/feed/v1"
	productv1 "remarket/backend/gen/product/v1"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/auth"
	"time"
)

type Service struct {
	users    userv1.UserServiceClient
	products productv1.ProductServiceClient
	cache    Timeline
}

// New 注入 User 与 Product RPC 客户端，基础版不依赖 Redis 或 Kafka。
func New(users userv1.UserServiceClient, products productv1.ProductServiceClient) *Service {
	return &Service{users: users, products: products}
}

// downstream 仅转发凭据供下游独立验签，不将身份头视为认证结果。
func downstream(ctx context.Context) context.Context {
	incoming, _ := metadata.FromIncomingContext(ctx)
	outgoing, _ := metadata.FromOutgoingContext(ctx)
	outgoing = outgoing.Copy()
	if values := incoming.Get("authorization"); len(values) > 0 {
		outgoing.Set("authorization", values...)
	}
	return metadata.NewOutgoingContext(ctx, outgoing)
}

// List 校验当前账号并分页读取完整关注集合，查询失败不降级成首页或返回部分结果。
func (s *Service) List(ctx context.Context, r *feedv1.ListFeedRequest, following bool) (*feedv1.ListFeedResponse, error) {
	if r == nil {
		r = &feedv1.ListFeedRequest{}
	}
	if r.PageSize < 0 || r.PageSize > 100 || len(r.Cursor) > 512 {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "Feed 分页参数无效"}
	}
	// 限制完整编排时长，避免大量关注分页逐次重置超时。
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ctx = downstream(ctx)
	var sellers []int64
	var userID int64
	var relations []string
	fingerprint := ""
	if following {
		identity, ok := auth.IdentityFromContext(ctx)
		if !ok {
			return nil, &apperror.Error{Kind: apperror.Unauthenticated, Message: "请先登录"}
		}
		userID = identity.UserID
		if _, err := s.users.GetCurrentUser(ctx, &userv1.GetCurrentUserRequest{}); err != nil {
			return nil, err
		}
		var after int64
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			page, err := s.users.ListFollowing(ctx, &userv1.ListFollowsRequest{UserId: identity.UserID, AfterId: after, PageSize: 100})
			if err != nil {
				return nil, err
			}
			for _, item := range page.Items {
				if item.GetUser().GetId() <= 0 {
					return nil, errors.New("关注数据无效")
				}
				sellers = append(sellers, item.User.Id)
				relations = append(relations, fmt.Sprintf("%d:%d", item.Id, item.User.Id))
			}
			// 基础版限制单次 IN 查询规模；超过限制明确失败，不能截断关注集合造成漏数据。
			if len(sellers) > 10000 {
				return nil, &apperror.Error{Kind: apperror.Invalid, Message: "关注数量超过基础查询容量"}
			}
			if page.NextAfterId == 0 {
				break
			}
			if page.NextAfterId <= after {
				return nil, errors.New("关注游标无效")
			}
			after = page.NextAfterId
		}
		if len(sellers) == 0 {
			return &feedv1.ListFeedResponse{}, nil
		}
		fingerprint = relationFingerprint(relations)
		if s.cache != nil {
			if result, ok := s.followingCache(ctx, userID, sellers, fingerprint, r); ok {
				return result, nil
			}
		}
	}
	result, err := s.products.ListFeedProducts(ctx, &productv1.ListFeedProductsRequest{Cursor: r.Cursor, PageSize: r.PageSize, SellerIds: sellers})
	if err != nil {
		return nil, err
	}
	if following && s.cache != nil && r.Cursor == "" {
		// 只对第一页回填至多 100 条权威记录，深页始终允许 MySQL 回退。
		seed := result.Products
		if r.PageSize != 100 {
			page, e := s.products.ListFeedProducts(ctx, &productv1.ListFeedProductsRequest{PageSize: 100, SellerIds: sellers})
			if e == nil {
				seed = page.Products
			}
		}
		if ctx.Err() == nil {
			warm, cancelWarm := context.WithTimeout(ctx, 200*time.Millisecond)
			_ = s.cache.EnsureModes(warm, sellers)
			_ = s.cache.Seed(warm, userID, fingerprint, seed)
			cancelWarm()
		}
	}
	return &feedv1.ListFeedResponse{Products: result.Products, NextCursor: result.NextCursor}, nil
}
