package repository

import (
 "context"
 "remarket/backend/internal/user/model"
 "gorm.io/gorm/clause"
)

type Follow struct { ID int64; FollowerID int64; FolloweeID int64 }
// SetFollow 通过唯一键裁决并发重复关注；删除不存在的关系同样成功。
func (r *UserRepository) SetFollow(ctx context.Context, actor, target int64, enabled bool) error {
 db := r.db.WithContext(ctx).Table("follows")
 if !enabled { return db.Where("follower_id = ? AND followee_id = ?", actor, target).Delete(&Follow{}).Error }
 return db.Clauses(clause.OnConflict{DoNothing:true}).Create(&Follow{FollowerID:actor, FolloweeID:target}).Error
}
type FollowUser struct { ID int64; User *model.User }
// ListFollows 以关系主键升序分页，不跨服务访问业务表。
func (r *UserRepository) ListFollows(ctx context.Context, target, after int64, size int, followers bool) ([]FollowUser,error) {
 owner, other := "follower_id", "followee_id"
 if followers { owner,other = other,owner }
 var rows []Follow
 if err := r.db.WithContext(ctx).Table("follows").Where(owner+" = ? AND id > ?",target,after).Order("id ASC").Limit(size).Find(&rows).Error; err != nil { return nil,err }
 result:=make([]FollowUser,0,len(rows))
 for _,row:=range rows { id:=row.FolloweeID; if followers { id=row.FollowerID }; user,err:=r.GetByID(ctx,id); if err!=nil{return nil,err}; result=append(result,FollowUser{ID:row.ID,User:user}) }
 return result,nil
}
