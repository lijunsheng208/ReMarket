# Day 2 订单数据库与状态机

本次完成：三张表的版本化建表 SQL、独立 Go 模型、状态机设计及 Service 层合法转换边检查。未向业务库执行迁移，未实现 Repository、实际订单状态更新、RPC、扫描器或事件投递，已在独立临时 MySQL 库通过建表、模型映射和核心约束验收；未向业务库应用迁移。SQL 位于 `migrations/order/000001_core.up.sql`；仅供 review 后在 Order 库应用，不重复执行已有 User/Product 迁移。

## 业务边界

单件二手商品、无在线支付；COMPLETED 只代表双方确认交易完成。买家不得购买自己的商品。买家、卖家都可在 RESERVED 且未过期时取消；即使一方已确认，另一方仍可取消。双方均已确认并进入 CONFIRMING 后不可取消或过期。PENDING 不提供主动取消，创建结果由恢复流程确定。

所有时间使用 UTC DATETIME(6)，边界以 Order 数据库当前时间为准：确认和主动取消要求 expires_at > 当前时间，过期要求 expires_at <= 当前时间。锁定订单后再取时间检查，避免等待行锁期间跨越截止时间。预占 RPC 沿用订单绝对过期时间，不因重试延长。

## 数据结构

- orders：买卖双方、商品外部引用、价格快照、状态、双方确认标志、聚合版本、幂等键、请求摘要、过期及关闭时间。外部引用不建立跨库外键。价格及卖家必须来自 Product 的可信快照，客户端不能指定最终金额或卖家。
- order_operations：持久化 RESERVE / RELEASE / CONFIRM 意图及重试、租约信息；同订单同类操作唯一。operation_id 为应用生成的 UUID；重试复用原记录和标识。order_id 为同库外键。
- outbox_events：沿用 Product 的事件元信息、JSON data、发布状态与租约结构；独立属于 Order，不引用 Product 模型或业务表。

orders 的 (buyer_id, idempotency_key) 唯一，幂等键区分大小写；Service 应限制为 1～128 个可打印 ASCII 字符。request_hash 保存规范化业务请求的 SHA-256 十六进制摘要，同键同摘要返回原订单，同键不同摘要拒绝。摘要纳入商品 ID 等客户端业务参数，不纳入 Token、Request ID 或随时间改变的商品查询结果。当前无订单清理机制，幂等键在订单保留期间持续有效。

product_id 不唯一：同一商品可有失败、取消及过期历史订单；唯一有效库存由 Product 的条件更新和 ACTIVE 预占唯一索引保证。买卖列表使用 (参与人, created_at, id)，订单超时扫描使用 (status, expires_at, id)。

SQL CHECK 校验取值、正金额、双方不同、确认及终态字段，不证明历史转换合法，也不保证跨表和跨服务一致性。BOOLEAN 在 MySQL 中不是严格布尔类型，因此显式限制为 0/1。初次插入明确写入统一 created_at 和 expires_at；expires_at 必须晚于 created_at。

## 合法状态转换

| 当前状态 | 目标状态 | 条件及同事务动作 |
| --- | --- | --- |
| 无 | PENDING | 保存可信快照，订单与 PENDING RESERVE 操作同事务插入 |
| PENDING | RESERVED | 确认 Product 已 ACTIVE 预占，RESERVE 操作标记 SUCCEEDED |
| PENDING | FAILED | 确认预占被业务拒绝且没有有效预占，RESERVE 标记 FAILED，设置 closed_at |
| RESERVED | RESERVED | 未过期的一方首次确认，设置其确认标志；另一方尚未确认 |
| RESERVED | CONFIRMING | 未过期的第二方确认，两个确认标志均为真，同时插入 CONFIRM 操作 |
| RESERVED | CANCELLING | 未过期，买家或卖家主动取消，同时插入 RELEASE 操作 |
| RESERVED | EXPIRING | 已到期，扫描器取得关闭决定，同时插入 RELEASE 操作 |
| CONFIRMING | COMPLETED | 确认 Product 预占已 CONFIRMED，CONFIRM 标记 SUCCEEDED，设置 closed_at |
| CANCELLING | CANCELLED | 确认 Product 预占已 RELEASED，RELEASE 标记 SUCCEEDED，设置 closed_at |
| EXPIRING | EXPIRED | 确认 Product 预占已 RELEASED，RELEASE 标记 SUCCEEDED，设置 closed_at |

其余转换均禁止；COMPLETED / CANCELLED / EXPIRED / FAILED 为终态。重复确认不增加版本，重复取消返回既有取消状态；对不兼容终态的请求返回状态冲突，不能将任意终态伪装为请求成功。PENDING 预占成功但本地处理时已经到期，仍先记录 RESERVED，再由过期事务进入 EXPIRING，确保释放有已确认的预占依据。

每次实际状态或确认标志变更递增 aggregate_version；相同请求不重复产生版本。未来事件类型约定为 order.created.v1、order.reserved.v1、order.confirmation_recorded.v1、order.confirming.v1、order.cancelling.v1、order.expiring.v1、order.completed.v1、order.cancelled.v1、order.expired.v1、order.failed.v1。每次变更与对应事件、操作记录同本地事务提交；仅设计名称，事件 Schema 和写入代码待后续实现。订单事件 data 不包含幂等键、请求摘要、Token 或内部错误。

## 并发、未知结果与恢复约束

确认、取消、过期都锁定同一订单行或使用 status + aggregate_version 条件更新，必须有且仅有一个决定成功。CONFIRMING、CANCELLING、EXPIRING 为不可逆决定：重试只能完成原决定，不可切换方向。不能持有数据库事务跨 RPC 调用。

操作状态：PENDING / RETRY → PROCESSING → SUCCEEDED；确定业务拒绝才进入 FAILED。临时故障或远程结果不确定时 PROCESSING → RETRY，增加 retry_count、设置退避时间并清空租约。PROCESSING 租约过期可由新工作者重新领取；完成更新必须匹配 lock_token，同时检查订单仍处于预期状态。重试次数耗尽不代表远程失败，不自动关闭订单或释放商品，应保留待恢复状态并告警。last_error 只保存脱敏错误分类，不保存 SQL、凭据或完整 RPC 错误文本。

订单行及操作行统一先锁订单、再锁操作；领取任务使用短事务，领取后释放锁，落地结果仍按统一锁序并检查租约。租约不能防止远程重复调用，Product RPC 必须按订单幂等处理。

RESERVE 超时或服务崩溃后先查询预占状态，不能直接转 FAILED。查询暂时不存在也不能证明原请求不会稍后提交：应重试同一幂等预占并确认最终结果；Product 必须对同订单参数一致性、并发重复调用和绝对截止时间提供保证。PENDING 不能因本地到期直接宣告 EXPIRED，更不能在未知预占结果时释放后结束恢复。此协议是后续 Product RPC 实现与故障测试的验收要求，本次尚未实现。

CONFIRM/RELEASE 超时通过查询和幂等重试恢复。Product 不得仅凭自身 expires_at 独立释放 ACTIVE 预占，否则可能与 Order 已持久化的成交决定冲突。若查询结果与不可逆决定相矛盾，保留记录并告警，不通过反向操作掩盖异常。

## 后续迁移验收

review 通过后，在独立临时 MySQL 8.4 库验证建表和约束，记录结果，不操作现有业务数据。覆盖幂等唯一键及大小写差异、同商品多历史订单、参与人/金额/状态/确认标志/关闭时间非法值、操作外键及唯一键、租约/完成时间约束、Outbox JSON 与事件去重。DDL 非事务性，失败后检查已执行部分，不盲目重跑；不提供自动 DROP 回滚。

真正的合法转换、事务回滚、并发竞争和故障恢复需在后续 Service/Repository 阶段验证，本次 SQL 文件存在不代表业务实现或迁移成功。

## 本轮代码验证

新增 `backend/internal/order/model/order.go` 映射三张表，`service/state.go` 仅检查合法转换边，不替代事务中的权限、期限、确认标志或远程结果校验。状态对测试覆盖全部九种状态及未知值，终态不可重开、成交与释放方向不可互换。

`gofmt`、`cd backend && go test ./...`、订单定向测试及 `git diff --check` 通过。真实 MySQL 迁移/约束测试已编写，但本轮因未配置 REMARKET_TEST_MYSQL_DSN 明确跳过；不表示数据库验收通过。业务库未修改。

## 本轮真实 MySQL 验收

已读取本地开发配置，在随机 `remarket_order_test_` 库执行迁移并运行 `go test -count=1 -v ./internal/order/...`，全部通过，临时库清理成功。未向现有业务库执行 DDL 或修改业务数据。

本次实际验证：三张表建表、订单/操作/Outbox 模型读写、幂等键重复拒绝及大小写区分、同商品多条订单、参与人/金额/状态/确认标志/关闭时间/过期时间约束、操作外键与同订单同类型唯一约束、操作租约和完成时间、非法 JSON 与聚合事件去重、Outbox 租约和发布时间，以及合法成交行和操作完成行可写入。直接 SQL 构造合法行只验证行内约束，不表示实际业务状态转换已实现。

此前 DSN 未配置的跳过记录保留为历史，本轮已完成上述真实数据库验证。订单业务事务、并发决策及故障恢复仍待后续实现与验收。
