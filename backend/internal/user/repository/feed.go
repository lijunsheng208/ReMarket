package repository

import (
	"context"
)

type FeedFollowers struct {
	IDs         []int64
	Next, Total int64
}

// ListFeedFollowers 查询本服务粉丝关系，不接受跨库联表或公开用户资料访问。
func (r *UserRepository) ListFeedFollowers(ctx context.Context, seller, after int64, size int) (*FeedFollowers, error) {
	out := &FeedFollowers{}
	if err := r.db.WithContext(ctx).Table("follows").Where("followee_id = ?", seller).Count(&out.Total).Error; err != nil {
		return nil, err
	}
	var rows []Follow
	if err := r.db.WithContext(ctx).Table("follows").Where("followee_id = ? AND id > ?", seller, after).Order("id ASC").Limit(size + 1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) > size {
		rows = rows[:size]
		out.Next = rows[len(rows)-1].ID
	}
	for _, row := range rows {
		out.IDs = append(out.IDs, row.FollowerID)
	}
	return out, nil
}
