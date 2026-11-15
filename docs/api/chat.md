# Day 5 聊天开发与 review 批次

## 批次与当前状态

1. 数据模型与边界：第一批交付版本化 SQL、Go 模型和设计说明；当时尚无聊天接口，后续实现见下列批次。未执行业务库迁移。
2. 会话基础：已接入 Proto、Repository → Service → Handler、独立 Chat 入口及 Gateway；实现创建/复用、会话列表、历史分页及权限测试。
3. 消息闭环：已实现事务序号、幂等写入、ACK、WebSocket 票据鉴权、连接管理和心跳；真实 WebSocket/gRPC/临时 MySQL race 链路已通过。
4. 跨实例：已实现多 Node 用户路由、租约 TTL、Redis Pub/Sub；双 Node WebSocket/真实 Redis/临时 MySQL race 验收通过。
5. 恢复与已读：已实现从零开始的固定上界增量补拉、单调已读游标、会话未读统计及本地/跨 Node 已读通知；真实 MySQL/Redis 与 HTTP/WebSocket/gRPC race 验证通过。
6. React 与综合验收：已接入联系卖家、会话窗口、自动重连、稳定键重试、去重补拉和可见已读；Chrome 桌面/手机夹具及真实 Vite→双 Node→MySQL/Redis 接线验收通过，边界见 [聊天前端](chat-web.md)。Day 7 深化独立进程故障注入和压测。

每批单独记录代码完成、模拟验证和真实基础设施验收，不自动创建提交。

## 数据及业务边界

Chat 独立拥有三张表。商品与用户仅保存 ID，通过 RPC 查询，不访问其他服务业务表。创建会话只接收 product_id；买家从可信身份获取，卖家从 Product 可信查询获取，禁止自聊。商品初次须允许咨询，已有会话在商品状态改变后仍可查看与聊天；账号禁用策略在会话批次接入现有鉴权能力时明确。

会话及两条成员记录同事务创建，以三元唯一键处理并发复用。发送消息先验证成员；锁定会话行后分配 last_seq，插入消息及更新最后消息元数据同事务提交。重复 sender_id/client_msg_id 必须核对会话、类型和内容，相同请求返回原消息，不同参数返回冲突；不额外消耗序号。

第三批仍不实现 Outbox。实时通知采用提交后尽力发布，失败不撤销已提交消息；ACK 仅代表持久化成功。后续若采用 Outbox，必须新增 Chat 自有迁移，不复用 Product 的业务表。

## 接口与恢复约定

已接入 HTTP：POST/GET `/api/v1/conversations`、GET `/:id/messages`、POST `/:id/read`；WebSocket：GET `/api/v1/chat/ws`。所有实体 ID、seq 在 JSON 使用十进制字符串。

历史查询 before_seq 排他倒序，补拉 after_seq 排他正序，二者互斥；默认 20，最大 100。会话列表按最后消息时间与 ID 降序，空会话以创建时间排序；游标不承诺跨请求快照。每次查询校验成员，不能把游标当作授权。

已读游标不得超过已持久化 last_seq，使用单调最大值更新。未读只统计 sender_id 不等于本人且 seq 超过已读位置的消息。

浏览器 WebSocket 不在 URL 携带长期 JWT：后续实现短期、一次性握手票据（HTTP Bearer 获取），校验 Origin 并禁止凭据日志。服务间继续独立验签、传递 Request ID，超时从上游派生。

客户端先建立实时订阅再补拉，缓存乱序消息，只推进连续收到的 seq；重连、页面恢复及周期同步均补拉，覆盖 Pub/Sub 在连接未断开时的通知遗漏。多设备均接收消息，发送方其他连接也须同步。服务端设置队列与写超时，慢连接关闭后通过补拉恢复。

## 验收边界

第一批未实现 RPC、HTTP 或 WebSocket，也未验证真实 MySQL 迁移。第二批需在独立临时库验证迁移、并发复用、事务回滚和非成员拒绝；不得以 SQL 文件存在宣称聊天完成。

## 第二批运行与验证（2026-11-15）

已接入三个 HTTP 接口：POST/GET `/api/v1/conversations` 和 GET `/api/v1/conversations/:id/messages`。创建请求为 `{"product_id":"123"}`，不接受买家或卖家参数。会话列表支持 cursor/page_size；消息支持 before_seq/after_seq/page_size，返回 has_more 与 next_seq，继续分页须沿原方向传入 next_seq。第二批当时将 after_seq=0 解释为最新历史；第五批已升级为区分字段存在性，当前显式 after_seq=0 表示从头正序补拉，省略 after_seq 才表示倒序历史。响应 int64 使用 Proto JSON 字符串。

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

未启动常驻服务、未验收独立 User/Chat/Gateway 进程部署或浏览器页面。第三批结束时没有客户端自动重连、已读/未读、Redis 跨实例、Outbox；Redis 后续进度见第四批。最终检查：全量 `cd backend && go test ./...`、Chat/Gateway race 验证、Gateway 票据升级专项 race 测试与 `git diff --check` 均通过。

## 第四批：Redis 路由与跨实例通知（2026-11-15）

### 路由及生命周期

Gateway 中每个 Chat Node 启动时生成 128 位随机实例 ID，先完成 Redis 频道订阅确认，再允许 HTTP 服务启动。CHAT_REDIS_ENABLED 默认 false，保持单实例本地模式；显式启用时 Redis 探活或订阅失败会导致启动失败，不静默伪装成可跨实例运行。

路由键为 `<prefix>route:<user_id>`，ZSet 成员为 Node 实例 ID，score 为毫秒到期时间。一个用户可以对应多个 Node；同一 Node 内多个连接共用租约，仍按已有每用户每 Node 八条连接上限管理。租约由 Redis TIME 原子计算并清理过期成员，避免各机器时钟差异；BIGINT 用户 ID 只用于键字符串，不转成浮点 score。key 本身还有两倍租期的空闲 TTL。

连接变化合并通知一个租约协程：复制本地在线用户后执行 Redis 操作，续期与清理串行，不在连接管理锁内访问网络。最后一个连接退出后清理当前 Node 的成员，不删除其他 Node 路由。快照与连接变化之间仍有短暂异步窗口，后续通知重新校正；异常退出时有效租约最多保留到 score 截止时间，key 物理清理由空闲 TTL 兜底。优雅退出停止订阅/续期，最后尝试有界清理；失败依赖 TTL。Redis 恢复后定时续期重新建立路由，不要求用户重新握手。

### 消息通知及错误边界

消息提交并排入 ACK 后，先向本地其他连接推送，再查询参与者的有效 Node 路由，按目标 Node 合并一次通知。当前 Node 不从 Redis 再接收自身消息，避免本地重复分发。发送者在其他 Node 的连接也接收通知。订阅端只向通知列出的用户分发，过滤自身来源、无效格式及非聊天事件。

频道为 `<prefix>db:<redis_db>:node:<node_id>`，显式纳入 DB，因为 Redis Pub/Sub 自身不按 DB 隔离。所有属于同一环境的实例必须配置相同前缀和 DB，不同环境使用不同前缀。Redis 为受信任内部设施，凭据不能交给客户端；内部通知不是客户端可直接发布的授权入口。

路由查询与发布共用从上游派生的 500ms 超时；失败仅记录不含正文/凭据的警告，不撤销已提交消息或发送失败 ACK。本地推送不依赖 Redis。路由维护每条命令也有 500ms 上限，不阻塞 WebSocket 读写；当前逐用户续期实现尚未经过大量连接压测。

每个 Node 只有一个订阅入口及有界 128 条通知缓冲；慢消费者可丢实时通知，连接自身仍受 64 条队列限制。go-redis 自动重订阅，但没有消息回放：订阅断开、路由注册窗口、发布前宕机、TTL 过期、队列溢出均可能遗漏通知，需通过 MySQL 查询恢复。未增加 Outbox，也不承诺推送顺序或恰好一次。

### 配置与本地运行

沿用 REDIS_ADDR/REDIS_PORT、REDIS_USERNAME、REDIS_PASSWORD、REDIS_DB 和连接超时参数，另增加：

```dotenv
CHAT_REDIS_ENABLED=true
CHAT_REDIS_PREFIX=chat:
CHAT_ROUTE_TTL=30s
CHAT_ROUTE_RENEW_INTERVAL=10s
```

租约允许 1 秒至 5 分钟，续期间隔必须为正且不超过 TTL 三分之一。环境变量需由运行方显式导出，本次未修改本机 .env 的启用状态。

准备好 User/Product/Chat 服务和数据库迁移后，可在两个终端分别运行：

```sh
GATEWAY_HTTP_ADDR=127.0.0.1:8080 CHAT_REDIS_ENABLED=true make run-gateway
GATEWAY_HTTP_ADDR=127.0.0.1:8081 CHAT_REDIS_ENABLED=true make run-gateway
```

两个终端均须先导出同一套现有配置及允许的前端 Origin。分别在对应 Gateway 获取 ticket 并握手，再连接两个用户。当前票据仍存本地内存，签发与握手必须落在同一 Gateway；负载均衡需保持该亲和性，不能把任意 Node 签发的票据送到另一 Node。共享票据与 Nginx 配置不在本批范围。

### 实际验证

在本机真实 Redis、随机临时 MySQL 库上，设置 CHAT_REDIS_INTEGRATION=1 与 REMARKET_TEST_MYSQL_DSN，运行：

```sh
cd backend
go test -race ./internal/chat/... ./internal/gateway -count=1
```

已通过：双 Node 在线互通、本地推送、跨 Node 发送者同步、多标签页共享路由、关闭单个标签不误删、关闭最后连接清理、超过初始 TTL 仍续期、旧租约过滤、无续期租约按真实时间自然过期、优雅退出不删除其他 Node 路由、精确终止测试订阅连接后的自动重订阅。

原 WebSocket→独立验签真实内存 gRPC→真实 MySQL 测试已支持双 Node，覆盖跨节点 ACK 丢失重试、权限及数据库回滚。命令故障钩子注入 Redis 错误时，验证 ACK 仍成功、消息仍可查询、恢复后继续跨节点推送。User 状态 RPC 仍为模拟客户端。测试仅清理随机 Redis 前缀和临时库，不修改业务表、业务键，也不清空 Redis；订阅断开只针对带随机测试名称的精确连接 ID。

全量 `go test ./...`、上述 race 验证及 `git diff --check` 均通过。两个 Node 位于同一个测试进程，未验收独立 Gateway 进程部署、浏览器、真实 Redis 整体停机/重启或独立 Node 强杀；自然过期测试模拟遗留租约，不能等同于独立进程崩溃验收。本批未启动常驻服务。

第四批结束时待办为已读游标、未读统计、增量补拉与通知遗漏恢复，当前实现见第五批；React 自动重连与完整聊天界面仍按后续前端批次接入。

## 第五批：补拉、未读统计与已读游标（2026-11-15）

### 当前接口

会话创建/复用及列表返回当前调用者的 last_read_seq、unread_count，均为只读派生字段，不增加会话表列。未读数为当前会话 `seq > last_read_seq AND sender_id <> 当前用户` 的消息数量，在同一查询快照内计算，不使用 `last_seq - last_read_seq`。已发送、已收到、已补拉和用户实际阅读是不同状态，发送或查询历史都不自动标记已读。

POST `/api/v1/conversations/:id/read` 请求：

```json
{"last_read_seq":"15"}
```

返回当前用户 ReadState：conversation_id、user_id、last_read_seq、unread_count、last_seq。遵循现有 Proto JSON 规则，int64 为字符串，默认零值可能省略，客户端需按零处理。HTTP 必须显式提供字符串 last_read_seq，不接受缺失、null、数字或客户端指定 user_id。

ReadConversation RPC/Service 使用可信身份并核实最新账号状态。事务按会话行→当前成员行加锁，顺序与发送一致；只有成员能更新，游标不能为负或超过已保存 last_seq，新旧位置取较大值。旧设备/重复请求返回当前真实位置，不回退。事务内读取未读计数，新到且超出已读位置的消息仍未读，数据库失败返回 Internal，不伪造成功。零表示不向前推进，可用于获取当前会话状态。会话不存在与非成员均返回 PermissionDenied。

本批复用第一批已有成员 last_read_seq 字段及消息会话序号索引，不增加迁移，不运行 AutoMigrate。

### 增量补拉协议

GET `/api/v1/conversations/:id/messages`：

- 不传 after_seq：最新历史倒序；before_seq 为排他上界。
- 显式 `after_seq=0`：从第一条开始正序补拉；其他 after_seq 为排他下界。
- after_seq 与 before_seq 不可同时传入，即使都是零；page_size 默认 20，最大 100。
- 首次 sync_until_seq 省略或为零，服务在只读可重复读快照中校验成员并捕获 last_seq，作为本轮固定上界。
- 后续页携带返回的 sync_until_seq 与 next_seq，查询 `after_seq < seq <= sync_until_seq`。后续插入的新消息留给下一轮，不使当前轮追赶无休止增长。
- 响应含 messages、has_more、next_seq、sync_until_seq；空补拉页保留请求的 next_seq，不把游标重置为零。空会话上界为零时字段也可能省略。
- 请求位置超过已保存范围、负值、反向上界及非成员查询均拒绝。每页重新认证授权，序号或上界不构成权限。

例：第一轮 `?after_seq=0&page_size=2` 返回 seq 1、2，next_seq=2、sync_until_seq=5、has_more=true；下一页传 `?after_seq=2&sync_until_seq=5&page_size=2`。即使此时已有 seq 6，本轮仍在 5 结束；下一轮 `?after_seq=5` 再获取 6。

gRPC after_seq 改为 proto3 optional，保留原字段编号；正值旧客户端仍兼容，未发送该字段仍查询最新历史，新客户端需显式发送零才能从头补拉。

### 已读实时事件

WebSocket 请求：

```json
{"type":"chat.read","request_id":"read-1","data":{"conversation_id":"123","last_read_seq":"15"}}
```

提交成功后当前连接收到 `chat.read.ack`，data 为持久化后的 ReadState，包含显式零值；错误返回 chat.error，不发送成功 ACK。其他参与者连接收到 `chat.read`，仅包含 conversation_id、user_id、last_read_seq，不公开对方未读计数。HTTP 更新也通过同一个 Node 通知本地与跨 Node 连接。直接 RPC 只更新状态，不触发 Gateway 通知，与直接发送 RPC 的边界一致。

已读通知同消息通知一样允许重复、乱序或遗漏。设备按 user_id 区分本人游标同步与对方阅读回执，对同一人的游标取最大值，不能因迟到通知回退；自身未读数字应刷新权威接口，不对重复事件盲目加减。Redis 发布失败不改变已读事务结果，后续会话列表/状态查询仍可恢复。

### 客户端恢复约定与范围

第五批完成了后端恢复接口；当时的测试客户端重连不等同于浏览器自动重连。第六批已实现 React 自动重连、待发消息与可见阅读策略，准确验收边界见 [聊天前端](chat-web.md)。

前端接入时应先建立实时监听再补拉，缓冲同时到来的推送；补拉按 seq 顺序合并，按消息 ID 去重。保存的同步位置必须是本设备实际拥有消息的最高连续序号，不可直接取最高推送序号，也不可拿账号共享的 last_read_seq 代替本设备同步位置。发现缺口、重连、页面恢复以及周期校验都应发起新一轮补拉，覆盖 WebSocket 仍连接但 Pub/Sub 通知遗漏的情况。单轮 has_more=false 后还要处理缓冲中高于本轮上界的消息与缺口。

消息补齐与标记阅读必须分开，只有满足前端可见窗口与阅读策略时才显式推进 last_read_seq；后台补拉不得清空未读。

### 实际验证

配置 REMARKET_TEST_MYSQL_DSN、CHAT_REDIS_INTEGRATION=1 后，`go test -race ./internal/chat/... ./internal/gateway -count=1` 通过，覆盖：

- 真实 MySQL 未读排除本人消息、空会话、旧位置幂等、多设备乱序位置取最大值、双方游标隔离、并发发送后新消息仍未读、更新故障回滚。
- 从零分页补拉、固定上界、补拉期间新增消息由下一轮恢复、空页不回退、未来/负值/非成员拒绝。
- HTTP→真实内存鉴权 gRPC→MySQL：列表统计、参数校验、历史/补拉方向、已读更新、HTTP 触发 WebSocket 已读通知、Internal 错误隐藏内部内容。
- 双 Node WebSocket+真实 Redis+MySQL：已读 ACK、对方及同账号其他标签页同步、旧请求不回退、越界和越权拒绝；断线后通过真实 RPC 分页恢复全部消息，包括注入 Redis 命令失败期间遗漏的通知；补拉不自动已读，显式提交后未读清零。

最终全量 `cd backend && go test ./...`、上述 race 验证及 `git diff --check` 均通过。测试清理随机临时库和 Redis 前缀，业务数据未修改；保留上一批未提交的 Redis 改动，未创建提交。User 状态 RPC 使用模拟客户端；双 Node 在同一测试进程中，独立进程故障与浏览器交互尚未验收。

## 第六批：React 聊天与真实浏览器接线（2026-11-15）

已接入消息导航、会话/历史窗口、商品联系卖家、一次性票据 WebSocket、稳定键待发箱、自动重连、连续窗口补拉及可见已读。会话新增只读 last_message_content 预览字段，按消息主键关联，无新迁移。前端构建、无警告 lint、十一个聊天/四个订单测试、后端全量/race、Chrome 桌面/手机夹具及真实 Vite→双 Chat Node→鉴权 gRPC→MySQL/Redis 浏览器链路通过。测试资源与临时进程已清理，业务数据未修改；辅助 User/Product 仍为夹具，独立进程故障与真实移动设备未验收。完整设计、运行方式与复现见 [聊天前端](chat-web.md)。
