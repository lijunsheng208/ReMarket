# Day 5 聊天开发与 review 批次

## 批次与当前状态

1. 数据模型与边界：本批新增版本化 SQL、Go 模型和设计说明。未执行业务库迁移，尚无可用聊天接口。
2. 会话基础：已接入 Proto、Repository → Service → Handler、独立 Chat 入口及 Gateway；实现创建/复用、会话列表、历史分页及权限测试。
3. 消息闭环：事务序号、幂等写入、ACK、WebSocket 鉴权、连接管理和心跳。
4. 跨实例：多连接路由、租约 TTL、Redis Pub/Sub，双节点基本互通验收。
5. 恢复与已读：增量补拉、单调已读游标、对方消息未读统计、断线及通知遗漏恢复。
6. React 与综合验收：联系卖家、会话窗口、重连重试、去重排序及异常场景。Day 7 深化故障注入和压测。

每批单独记录代码完成、模拟验证和真实基础设施验收，不自动创建提交。

## 数据及业务边界

Chat 独立拥有三张表。商品与用户仅保存 ID，通过 RPC 查询，不访问其他服务业务表。创建会话只接收 product_id；买家从可信身份获取，卖家从 Product 可信查询获取，禁止自聊。商品初次须允许咨询，已有会话在商品状态改变后仍可查看与聊天；账号禁用策略在会话批次接入现有鉴权能力时明确。

会话及两条成员记录同事务创建，以三元唯一键处理并发复用。发送消息先验证成员；锁定会话行后分配 last_seq，插入消息及更新最后消息元数据同事务提交。重复 sender_id/client_msg_id 必须核对会话、类型和内容，相同请求返回原消息，不同参数返回冲突；不额外消耗序号。

本批不实现 Outbox。实时通知采用提交后尽力发布，失败不撤销已提交消息；ACK 仅代表持久化成功。后续若采用 Outbox，必须新增 Chat 自有迁移，不复用 Product 的业务表。

## 接口与恢复约定

计划 HTTP：POST/GET `/api/v1/conversations`、GET `/:id/messages`、POST `/:id/read`；WebSocket：GET `/api/v1/chat/ws`。所有实体 ID、seq 在 JSON 使用十进制字符串。

历史查询 before_seq 排他倒序，补拉 after_seq 排他正序，二者互斥；默认 20，最大 100。会话列表按最后消息时间与 ID 降序，空会话以创建时间排序；游标不承诺跨请求快照。每次查询校验成员，不能把游标当作授权。

已读游标不得超过已持久化 last_seq，使用单调最大值更新。未读只统计 sender_id 不等于本人且 seq 超过已读位置的消息。

浏览器 WebSocket 不在 URL 携带长期 JWT：后续实现短期、一次性握手票据（HTTP Bearer 获取），校验 Origin 并禁止凭据日志。服务间继续独立验签、传递 Request ID，超时从上游派生。

客户端先建立实时订阅再补拉，缓存乱序消息，只推进连续收到的 seq；重连、页面恢复及周期同步均补拉，覆盖 Pub/Sub 在连接未断开时的通知遗漏。多设备均接收消息，发送方其他连接也须同步。服务端设置队列与写超时，慢连接关闭后通过补拉恢复。

## 验收边界

本批没有实现 RPC、HTTP 或 WebSocket，也没有验证真实 MySQL 迁移。下一批需在独立临时库验证迁移、并发复用、事务回滚和非成员拒绝；不得以 SQL 文件存在宣称聊天完成。

## 第二批运行与验证（2026-11-15）

已接入三个 HTTP 接口：POST/GET `/api/v1/conversations` 和 GET `/api/v1/conversations/:id/messages`。创建请求为 `{"product_id":"123"}`，不接受买家或卖家参数。会话列表支持 cursor/page_size；消息支持 before_seq/after_seq/page_size，返回 has_more 与 next_seq，继续分页须沿原方向传入 next_seq。第一页 after_seq=0 表示最新历史；需要全量补拉时先查询最新历史，再按需要向前翻页。响应 int64 使用 Proto JSON 字符串。

每个操作独立验 JWT，并通过 User GetCurrentUser 实时检查当前账号；禁用账号不得继续读写。创建新会话以 Product 当前在售快照校验，跨服务查询与本地创建不构成分布式事务。会话列表采用活动时间与 ID 排他游标，列表在聊天活动后可能移动，刷新第一页获取最新位置。

导出 .env 后补齐 .env.example 中 CHAT_MYSQL_*、CHAT_GRPC_ADDR、CHAT_GRPC_TARGET；密码沿用 MYSQL_CHAT_PASSWORD。显式迁移 Chat 自有库后运行 `make run-chat`，Gateway 使用 CHAT_GRPC_TARGET（默认 127.0.0.1:50056）。本次未向业务库迁移或启动常驻服务。

验证：全量 Go 测试通过。配置本机测试 MySQL 后，`go test -race ./internal/chat/... -count=1 -v` 通过，真实随机临时库覆盖建表、12 请求并发复用、成员数量、卖家/非成员列表隔离、历史/增量排序和成员插入故障回滚，临时库已清理。Service 模拟测试覆盖可信参与者、自聊拒绝、既有会话复用、账号拒绝、凭据转发、上游期限和非法分页。Gateway 参数与 JSON 精度另有模拟测试。上述不代表独立进程端到端验收，跨请求会话游标及完整 RPC 权限链路仍待综合验收。

消息发送、未读、已读、WebSocket 和 Redis 均留到后续批次。
