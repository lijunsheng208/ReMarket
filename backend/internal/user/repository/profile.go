package repository

import (
	"context"
)

type FollowStats struct {
	FollowerCount  int64
	FollowingCount int64
	IsFollowing    bool
}

// ProfileStats 在单条查询的快照内读取粉丝、关注总数及当前调用者的关系，零身份不会冒充用户。
func (r *UserRepository) ProfileStats(ctx context.Context, target, viewer int64) (*FollowStats, error) {
	var stats FollowStats
	err := r.db.WithContext(ctx).Raw("SELECT (SELECT COUNT(*) FROM follows WHERE followee_id = ?) AS follower_count, (SELECT COUNT(*) FROM follows WHERE follower_id = ?) AS following_count, EXISTS(SELECT 1 FROM follows WHERE follower_id = ? AND followee_id = ?) AS is_following", target, target, viewer, target).Scan(&stats).Error
	return &stats, err
}
