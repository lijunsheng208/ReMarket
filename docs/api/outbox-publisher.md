# Outbox Publisher

实现位于 `backend/pkg/outbox`。这是本地投递技术组件：Task 是持久化投递元信息投影，不是共享 GORM 业务模型。Product、Order 各自入口只注入自身 sql.DB 和 aggregate_type，使用已有服务数据库账号；不部署持有所有业务库凭据的通用跨库 Worker。

## 状态与事务边界

沿用现有表的 `PENDING → PROCESSING → PUBLISHED`，PUBLISHED 对应计划里的 SENT，不新增 SENT 状态或自动迁移。业务变更与事件仍由各业务 Repository 同事务写入；Publisher 只操作已提交 Outbox。

1. READ COMMITTED 短事务，`FOR UPDATE SKIP LOCKED` 按 ID 领取一条已到 next_attempt_at 的 PENDING，或租约过期的 PROCESSING。
2. 更新随机 128 位 lock_token、数据库时间 locked_until、attempts，然后提交事务；attempts 达 INT 上限后饱和，不溢出。
3. 事务外从元信息和 payload（仅 data）组装公共 Envelope，复用 event_id/occurred_at/聚合版本，通过 Producer 发往主 Topic，aggregate_id 为 Key。
4. 等待 Kafka ACK 成功才按 ID、服务聚合类型、PROCESSING、令牌及未过期租约条件更新 PUBLISHED/published_at，并清理锁和错误。
5. 发送错误或结果未知回到 PENDING，记录固定类别 publish_unconfirmed，按封顶指数退避加抖动安排重试；超过十次持续告警并继续重试，不删除消息。

信封校验不通过进入 FAILED，记录 invalid_envelope，不发送 Kafka、不自动循环重试；这不是消费者 DLT。修复持久化数据并核对业务契约后才能显式恢复 PENDING，不提供自动改写/删除接口。底层数据库错误、发送错误不将原 SQL、Kafka 错误或载荷写入 last_error/日志。

取消或 ACK 后数据库更新失败时不另起脱离上游的后台确认，保留 PROCESSING，租约到期恢复。旧 Worker 迟到完成无法覆盖新令牌。ACK 与数据库更新不具备分布式原子性，已落 Kafka 的消息可能重投，同一 event_id 必须由消费者持久化幂等处理；幂等生产者不能消除这类重复。

多 Worker/多实例认领不会在有效租约内同时拥有同一行，但暂停超过租约可能导致重复发送；不续租，配置必须留出裕量。按 ID 领取不保证同一聚合跨并发、失败重试后的事件顺序，Kafka Key 只保证 Broker 接收顺序在同分区；下游必须执行 aggregate_version 检查。强制聚合串行顺序若有需要，需另行设计，不能用本实现声称已保证。

## 配置与生命周期

默认 `PRODUCT_OUTBOX_ENABLED=false`、`ORDER_OUTBOX_ENABLED=false`；分别打开后重启对应业务进程。Kafka 不可用时业务进程仍可启动并积压本地事件，发布循环重试；启用时 Outbox 表不可访问则启动失败，不自动建表。

| 配置 | 默认 |
| --- | --- |
| OUTBOX_CONCURRENCY | 每个进程4，允许1～64 |
| OUTBOX_POLL_INTERVAL | 空闲/领取失败等待1s，最多1m |
| OUTBOX_DB_TIMEOUT | 单次数据库操作3s，最多30s |
| OUTBOX_PUBLISH_TIMEOUT | 单次发送10s，最多1m |
| OUTBOX_LEASE | 30s，最多5m，必须大于发送预算加两倍数据库预算 |
| OUTBOX_RETRY_BASE / MAX | 1s / 1m，最大上限24h |

每个工作者一次只领一条，不预领大批任务以免等待队列耗尽租约。启动即扫描；取消后等所有工作者退出，再关闭 Producer，最后由业务入口关闭数据库。轮询和操作预算均有界，不执行自动清理已发布事件；归档和保留策略另行明确。Kafka 客户端和 Topic 使用既有 KAFKA_* 配置。

固定类别回调可供日志/指标接入，要求并发安全且不阻塞；当前入口使用 slog.Warn。尚未实现积压量/最老事件年龄监控面板。

## 实际验证

真实 MySQL/Kafka race 集成测试对 Product、Order 分别创建独立临时库和唯一 Kafka Topic，不读取业务库积压，不修改业务事件状态。覆盖：

- 12 个并发领取者只有一个获得任务；未过期租约不重复领取。
- 发送失败回到 PENDING，没有 published_at；重试继续使用原 event_id。
- 实际 Kafka ACK 后故意不完成数据库，强制推进测试租约到期，重建 Store 接管；旧令牌更新失败。
- 新 Publisher 恢复后记录 PUBLISHED、时间和次数正确。
- 直接读取 Kafka 测试分区确认恢复窗口出现两条相同逻辑 ID 的消息，Key 保持 aggregate_id。
- 服务聚合过滤不认领其他聚合；单测补充 ACK 顺序、无效信封隔离和退避/配置边界。

复验（从根目录加载本地环境，管理账号只供创建/清理测试库）：

```sh
set -a
. ./.env
set +a
export REMARKET_TEST_MYSQL_DSN="root:${MYSQL_ROOT_PASSWORD}@tcp(127.0.0.1:${MYSQL_PORT:-3306})/?parseTime=true&loc=UTC"
cd backend
KAFKA_INTEGRATION=1 go test -race ./pkg/outbox -count=1 -v
go test ./...
```

测试结束只清理本次临时库及 Topic；未配置开关时真实集成明确跳过。ACK 后崩溃窗口通过“不完成数据库+租约推进+重建组件”模拟，未声称做过本发布器独立进程 SIGKILL 或真实 Broker 停机恢复；未接入业务消费者持久化幂等、Retry/DLT。默认不开启业务积压投递，本轮不重启 Product/Order。
