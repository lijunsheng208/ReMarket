# Day 5 聊天开发与 review 批次

## 批次与当前状态

1. 数据模型与边界：本批新增版本化 SQL、Go 模型和设计说明。未执行业务库迁移，尚无可用聊天接口。
2. 会话基础：已接入 Proto、Repository → Service → Handler、独立 Chat 入口及 Gateway；实现创建/复用、会话列表、历史分页及权限测试。
3. 消息闭环：已实现事务序号、幂等写入、ACK、WebSocket 票据鉴权、连接管理和心跳；真实 WebSocket/gRPC/临时 MySQL race 链路已通过。
4. 跨实例：多连接路由、租约 TTL、Redis Pub/Sub，双节点基本互通验收。
5. 恢复与已读：增量补拉、单调已读游标、对方消息未读统计、断线及通知遗漏恢复。
6. React 与综合验收：联系卖家、会话窗口、重连重试、去重排序及异常场景。Day 7 深化故障注入和压测。

每批单独记录代码完成、模拟验证和真实基础设施验收，不自动创建提交。

## 数据及业务边界

Chat 独立拥有三张表。商品与用户仅保存 ID，通过 RPC 查询，不访问其他服务业务表。创建会话只接收 product_id；买家从可信身份获取，卖家从 Product 可信查询获取，禁止自聊。商品初次须允许咨询，已有会话在商品状态改变后仍可查看与聊天；账号禁用策略在会话批次接入现有鉴权能力时明确。

会话及两条成员记录同事务创建，以三元唯一键处理并发复用。发送消息先验证成员；锁定会话行后分配 last_seq，插入消息及更新最后消息元数据同事务提交。重复 sender_id/client_msg_id 必须核对会话、类型和内容，相同请求返回原消息，不同参数返回冲突；不额外消耗序号。

第三批仍不实现 Outbox。实时通知采用提交后尽力发布，失败不撤销已提交消息；ACK 仅代表持久化成功。后续若采用 Outbox，必须新增 Chat 自有迁移，不复用 Product 的业务表。

## 接口与恢复约定

计划 HTTP：POST/GET `/api/v1/conversations`、GET `/:id/messages`、POST `/:id/read`；WebSocket：GET `/api/v1/chat/ws`。所有实体 ID、seq 在 JSON 使用十进制字符串。

历史查询 before_seq 排他倒序，补拉 after_seq 排他正序，二者互斥；默认 20，最大 100。会话列表按最后消息时间与 ID 降序，空会话以创建时间排序；游标不承诺跨请求快照。每次查询校验成员，不能把游标当作授权。

已读游标不得超过已持久化 last_seq，使用单调最大值更新。未读只统计 sender_id 不等于本人且 seq 超过已读位置的消息。

浏览器 WebSocket 不在 URL 携带长期 JWT：后续实现短期、一次性握手票据（HTTP Bearer 获取），校验 Origin 并禁止凭据日志。服务间继续独立验签、传递 Request ID，超时从上游派生。

客户端先建立实时订阅再补拉，缓存乱序消息，只推进连续收到的 seq；重连、页面恢复及周期同步均补拉，覆盖 Pub/Sub 在连接未断开时的通知遗漏。多设备均接收消息，发送方其他连接也须同步。服务端设置队列与写超时，慢连接关闭后通过补拉恢复。

## 验收边界

第一批未实现 RPC、HTTP 或 WebSocket，也未验证真实 MySQL 迁移。第二批需在独立临时库验证迁移、并发复用、事务回滚和非成员拒绝；不得以 SQL 文件存在宣称聊天完成。

## 第二批运行与验证（2026-11-15）

已接入三个 HTTP 接口：POST/GET `/api/v1/conversations` 和 GET `/api/v1/conversations/:id/messages`。创建请求为 `{"product_id":"123"}`，不接受买家或卖家参数。会话列表支持 cursor/page_size；消息支持 before_seq/after_seq/page_size，返回 has_more 与 next_seq，继续分页须沿原方向传入 next_seq。第一页 after_seq=0 表示最新历史；需要全量补拉时先查询最新历史，再按需要向前翻页。响应 int64 使用 Proto JSON 字符串。

每个操作独立验 JWT，并通过 User GetCurrentUser 实时检查当前账号；禁用账号不得继续读写。创建新会话以 Product 当前在售快照校验，跨服务查询与本地创建不构成分布式事务。会话列表采用活动时间与 ID 排他游标，列表在聊天活动后可能移动，刷新第一页获取最新位置。

导出 .env 后补齐 .env.example 中 CHAT_MYSQL_*、CHAT_GRPC_ADDR、CHAT_GRPC_TARGET；密码沿用 MYSQL_CHAT_PASSWORD。显式迁移 Chat 自有库后运行 `make run-chat`，Gateway 使用 CHAT_GRPC_TARGET（默认 127.0.0.1:50056）。本次未向业务库迁移或启动常驻服务。

验证：全量 Go 测试通过。配置本机测试 MySQL 后，`go test -race ./internal/chat/... -count=1 -v` 通过，真实随机临时库覆盖建表、12 请求并发复用、成员数量、卖家/非成员列表隔离、历史/增量排序和成员插入故障回滚，临时库已清理。Service 模拟测试覆盖可信参与者、自聊拒绝、既有会话复用、账号拒绝、凭据转发、上游期限和非法分页。Gateway 参数与 JSON 精度另有模拟测试。上述不代表独立进程端到端验收，跨请求会话游标及完整 RPC 权限链路仍待综合验收。

第二批结束时，消息发送、未读、已读、WebSocket 和 Redis 尚未实现；消息与 WebSocket 的后续进度见第三批。

## 第三批：消息持久化与单实例 WebSocket（2026-11-15）

### 职责及接入

Gateway 内的独立 node 模块承担 Chat Node，长连接不在 Chat RPC 进程中。发送通过 Gateway Node → Chat SendMessage RPC → Service → Repository，Chat 独立验签并经 User RPC 核实发送者账号状态。Node 使用 SendMessageResponse 中服务器确定的 participant_ids，仅向这两个用户的本地连接推送；发送者其他标签页也接收，当前发送连接接收 ACK。同一用户最多八条连接。直接调用 SendMessage RPC 只持久化，不触发 Gateway 本地通知；浏览器发送入口为 WebSocket。

新增 POST `/api/v1/chat/tickets`：携带 Bearer JWT 和 Origin，返回 ticket、expires_at，响应禁止缓存。票据为 32 字节安全随机数，服务器仅以其哈希作为索引，在内存保存原 JWT；有效期不超过 30 秒和 JWT 到期时间，使用一次即删除，未使用票据最多 4096 个，签发时清理过期项。票据不落盘，进程重启后需重取。握手用 GET `/api/v1/chat/ws?ticket=...`，再次验签并核实账号；不能将长期 JWT 放入 URL。部署层日志应隐藏 ticket 查询参数。后续多实例需保证票据签发与握手落在同一 Node，或改为共享一次性票据存储。

Origin 必须存在且单值；默认允许 scheme/host 与请求同源，也支持 CHAT_ALLOWED_ORIGINS 中明确列出的来源，不信任任意 X-Forwarded-Host。Vite 开发时配置实际使用的 localhost/127.0.0.1:5173 来源，示例已补齐。代理需要支持 Upgrade；本批未实现 Nginx 部署配置。

### WebSocket 协议

ID、seq 均为字符串。每条应用请求需要 request_id（1～128 个字母、数字、下划线、点或连字符），事件示例：

```json
{"type":"chat.send","request_id":"req-1","data":{"conversation_id":"123","client_msg_id":"msg-1","content":"这个商品还在吗？"}}
```

持久化成功：`chat.ack` 回显 request_id，data 含 message_id、conversation_id、seq、client_msg_id。在线通知：`chat.message` 的 data 含 message_id、conversation_id、sender_id、client_msg_id、seq、content、message_type、created_at，字段按 Proto JSON 规则省略默认值。错误：`chat.error` 回显有效 request_id，data 含 code/message，未知内部错误统一为 Internal。应用 ping 返回 pong；底层每 20 秒发送控制帧 Ping，60 秒未收到 Pong 则关闭。

文本不得为空白，正文合法 UTF-8，最多 2000 个 Unicode 字符且不超过 8000 字节，保留原文；client_msg_id 为 1～64 个字母、数字、下划线、点、冒号或连字符。单帧上限 16 KiB。消息正文不得作为 HTML 直接插入页面，后续 React 按文本渲染。

### 事务、顺序与资源释放

会话行锁串行分配 seq，消息插入和会话 last_seq/last_message_id/last_message_at 同事务；失败不消耗序号。先检查成员再读取幂等结果，相同 sender_id/client_msg_id 和同一会话、正文、类型返回原消息，参数变化返回 AlreadyExists。数据库唯一约束仲裁跨会话复用键的竞争；取消或超时可能已提交，客户端必须复用原键重试。

ACK 只说明保存成功。ACK 排入当前连接后才分发通知；不同连接的实际观察时间不作保证。多个并发 RPC 的在线通知可能乱序或重复，前端需按 seq 排序、按 message_id 去重；已落库顺序以 seq 为准。重复请求可再次触发通知。提交后进程退出或队列失败会遗漏通知，查询 MySQL 恢复，不保证实时投递。

每条连接单一应用写协程，队列最多 64 条、写超时五秒；队列溢出主动断开慢连接。JWT 到期即关闭空闲连接，每次发送再次验签；发送时 Chat 核实最新账号状态。已建立连接的被动推送尚无账号禁用广播撤销机制。Gateway 退出显式关闭升级连接并等待读写协程结束，再释放 RPC 连接。

### 验证及剩余边界

- 配置测试 MySQL 后，Chat 与 Gateway race 测试通过；随机临时库已清理，业务库未修改。
- MySQL 测试覆盖 20 个同键并发请求仅落一条、12 个不同键连续 seq、正文/跨会话键冲突、非成员拒绝、元数据失败时消息与序号回滚。
- 真实 WebSocket → 内存传输真实 Chat gRPC（独立 JWT 验签）→ MySQL 链路覆盖 ACK 对应落库、在线推送、断线重试、未读取 ACK 后重试不重复、离线消息查询、数据库故障 Internal 且不返回 ACK。User 状态检查使用模拟 RPC 客户端。
- WebSocket 测试覆盖多标签页同步、票据重放/过期/禁用账号拒绝、Origin、协议校验、慢连接队列、主动关闭、控制帧心跳超时及 JWT 到期释放。

未启动常驻服务、未验收独立 User/Chat/Gateway 进程部署或浏览器页面。本批没有客户端自动重连、已读/未读、Redis 跨实例、Outbox；分别留到后续批次。最终检查：全量 `cd backend && go test ./...`、Chat/Gateway race 验证、Gateway 票据升级专项 race 测试与 `git diff --check` 均通过。
