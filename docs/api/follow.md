# 关注系统

关注关系归 User Service 管理，复用 `migrations/user/000001_core.up.sql` 的 follows 表，无新增迁移。Feed 尚未实现。

四个接口均要求 `Authorization: Bearer <token>`，Gateway 与 User Service 分别验签，写入者取自可信上下文，不能指定 follower_id。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| POST | /api/v1/users/:id/follow | 幂等关注 |
| DELETE | /api/v1/users/:id/follow | 幂等取消 |
| GET | /api/v1/users/:id/followers | 粉丝列表 |
| GET | /api/v1/users/:id/following | 关注列表 |

写入成功返回 200 `{}`。不能关注自己，目标必须存在，新增关注目标必须启用，当前账号必须启用。重复关注由数据库唯一键裁决；取消不存在的关系成功。

列表参数 `page_size` 默认 20、最大 100；`after_id` 默认 0。按关系 ID 升序返回 `items: [{id, user}]`，有下一页时返回 `next_after_id`，下一次作为 after_id 传入。ID 按 Proto JSON 输出字符串。游标不是快照，并发新增关系可能在后续页出现。公开资料不包含密码哈希。

验证（2026-11-15）：配置本地 MySQL 后执行 `go test ./...` 全量通过；`go test -race ./internal/gateway ./internal/user/service -run 'TestFollow'` 通过。新增 `TestFollowAcceptanceMySQL` 使用独立临时库，覆盖真实 Gateway HTTP 路由 → bufconn gRPC → MySQL：16 次并发关注仅一条记录、双方分别持令牌查询关注/粉丝、两页游标不重复、重复取消落库、匿名拒绝、自关注拒绝、不存在目标及页大小限制。请求携带伪造 x-user-id，实际关系仍使用令牌身份。临时库测试后清理，未修改现有业务数据。

以上为进程内 HTTP 路由与 gRPC 测试，不代表已启动常驻 Gateway/User 进程，也未验收浏览器前端。
