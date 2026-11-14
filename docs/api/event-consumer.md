# 测试消费者：幂等、Retry、DLT 与重放

实现：`backend/internal/eventtest`、`backend/cmd/event-consumer`。此服务仅维护测试可重建视图，不实现 Feed、Search 或其他业务消费者。

## 独立数据与去重事务

管理员需显式建立独立消费者库及专用账号，按版本应用 `migrations/eventtest/000001_core.up.sql`。账号仅授予该库 SELECT/INSERT/UPDATE/DELETE，不授予 Product/Order 库权限，不使用业务服务账号。运行入口拒绝常见业务库名称，但最终隔离仍须由 MySQL 授权保证。程序不自动建库、建表或迁移，本轮没有部署消费者业务库。

同一库对应一个逻辑消费者的持久化状态：

- event_inbox：二进制区分大小写的 event_id 主键、信封指纹、处理轮次、重试阶段和结果。
- event_views：按 aggregate_type/aggregate_id 保存最高版本快照。
- event_transfers：按事件+轮次+阶段固定决定 ID 保存不可变 Retry/DLT 字节；无效信封使用原 Kafka 坐标生成决定 ID。

每次事务先插入/锁定 event_inbox，再检查已持久化决定。业务结果、去重成功状态或下一转移决定同事务提交。仅在处理成功后写 DONE。测试视图只接受更高 aggregate_version，同版本/旧版本不覆盖新值。重复 event_id 不重新执行业务；同 ID 信封内容变化进入冲突 DLT，而不是静默当成成功。

处理器 Apply 必须只使用传入 SQL 事务，不能执行不可回滚的外部副作用。BusinessError 明确区分暂时/永久失败；发生业务失败时通过保存点回滚业务写入，再保存 Retry/DLT 决定。未分类错误（包括数据库错误）回滚整个事务，返回错误，不把数据库故障伪装为死信。

event_id 按通用信封限制为 1～128 位 ASCII 字母、数字、下划线、点、冒号或连字符。信封指纹依据重新编码的信封 JSON；业务原始 data 成员顺序变化可能被判为内容冲突，重投应复用原始持久化消息，不重新构造逻辑事件。

## 有限重试与 ACK 边界

主 Topic 消费原始 Envelope；Retry/DLT 消费协议为版本1 Packet，包含主来源、原 Key/原 Value（JSON base64）、原始 Topic/Partition/Offset、处理轮次、阶段、ready_at、错误类别。转移时保留逻辑 event_id 和原始数据，不能把重试当成新的业务事件。

初始业务尝试 stage=0，暂时失败且 stage<MaxRetries 时持久化 stage+1 的 Retry 包。默认最多3次重试，即最多4次业务执行。永久业务失败直接 DLT，重试耗尽进入 DLT。默认等1秒，可配置1ms～5秒；单次 Consumer 处理预算30秒，提交预算5秒，单次转移发布预算10秒。重试消费者收到未到 ready_at 的包时沿用 Context 有界等待；串行等待会阻塞当前消费者，不是高吞吐延迟队列。此版为固定退避，不宣称指数延迟 Topic 调度。

事务内只保存转移决定，不调用 Kafka。事务提交后调用 acks=all PublishRaw；只有 Broker ACK 成功，Handle 才返回 nil，底层 Consumer 才提交原 Offset。发布失败、结果未知、进程退出或提交失败将重建同组；重复输入复用已有转移决定，不再增加业务尝试。

ACK 后 Offset 提交前崩溃可产生重复 Retry/DLT，重试阶段与去重状态保证不会无限增加业务执行次数。DLT 本身允许重复，后续运维处理应按 event_id/轮次/阶段或决定 ID 去重。未获 ACK 时不能跳过原消息；不承诺 Kafka 与 MySQL 跨系统恰好一次。

同一 event_id 在不同 Kafka 消费组中若共用此库，也会共享去重结果。因此每个独立业务消费者应使用自己的库/表命名空间；改变 Group 不等于重做业务。主链和重试链必须由同一逻辑消费者维护；另一个消费者不能直接复用该 inbox 作为独立结果来源。

## 启动与显式重放

配置见 .env.example：EVENT_CONSUMER_AGGREGATE=product/order，GROUP，以及独立 MySQL 地址/用户名/密码/库名、MAX_RETRIES、BACKOFF。两类聚合可分别运行进程；更改为 order 时需同步修改 Group。订阅对应主 Topic 与 Retry Topic，不自动消费 DLT。Topic 使用现有 KAFKA_* 配置，必须事先创建。

从根目录加载环境再启动：

```sh
set -a
. ./.env
set +a
make run-event-consumer
```

将选定 DLT Record 的 Value 导出为 Packet JSON 文件后，显式重放：

```sh
cd backend
go run ./cmd/event-consumer replay /absolute/path/dead-letter.json
```

重放入口验证原始信封，保留 event_id/内容，生成新的32位随机轮次并清零阶段，发送到 Retry Topic，等待 ACK。原状态必须为 DLT 才能重开既有事件；DONE 不重做业务。相同重放包重复投递仍受持久化去重保护。多次重新执行 replay 命令是多次人工重放请求，可能在再次失败进入 DLT 后开启新轮次，不提供自动无限重放。

无法解析的信封需先修复契约才能通过重放入口；若已有 event_id 的内容发生变化，不能冒充原事件重放，应由业务明确修正为新的事件身份或修复处理器后原样重放。文件限制512KiB。正常商品/订单消息远小于 Broker 限额；超大消息经过 base64 包装可能超过 Broker 限额，转移会失败并保持原 Offset，须人工处理，不能把 ACK 失败当作已进入死信。

## 验证与边界

真实 MySQL/Kafka 临时库、唯一三条 Topic 和测试 Group 的 race 测试覆盖：12请求并发 event_id 去重、Store 重建去重、旧版本保护、同ID内容冲突；临时业务错误失败写入回滚、有限重试直至DLT、显式新轮次重放及重放重复；永久业务错误直接DLT；Retry/DLT 发送失败均不提交原 Offset，同组重建重发获 ACK 后才推进。原始损坏消息可从 DLT 还原。

复验：从根目录加载 .env，设置管理账号临时库 DSN 后，执行 `KAFKA_INTEGRATION=1 go test -race ./internal/eventtest ./pkg/kafka -count=1 -v`。测试仅清理本次临时库、Topic/Group，不投递业务事件、不修改运行中业务库。未设置集成开关时明确跳过真实验证。

故障测试注入 Sender 未确认错误，Broker 及实际重建消费组仍为真实连接；未停止共享 Broker 或对本消费者做独立进程 SIGKILL。真实 Broker 中断、多进程 rebalance、DLT 导出工具与运维页面、积压指标和事件保留/归档策略尚未实现。event_transfers/inbox 不自动清理，删除过早会破坏重投幂等；需覆盖 Kafka 保留期及允许重放期限后另行制定清理策略。
