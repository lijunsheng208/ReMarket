# Kafka Producer / Consumer 基础封装

实现位于 `backend/pkg/kafka`，仅技术能力，不共享 GORM 模型，不执行 Outbox 扫描或业务幂等。franz-go 版本维持 v1.22.3。

## Topic 与事件契约

`LoadTopics` 从 KAFKA_PRODUCT_TOPIC、KAFKA_ORDER_TOPIC、KAFKA_PRODUCT_RETRY_TOPIC、KAFKA_ORDER_RETRY_TOPIC、KAFKA_PRODUCT_DLT_TOPIC、KAFKA_ORDER_DLT_TOPIC 加载名称，默认与 Compose 六个 Topic 一致；所有名称必须合法且不同。Go 配置不建 Topic，更改名称需显式在 Broker 创建，Compose 脚本仍只初始化默认名称。

`Envelope` 沿用现有 api/events/product 契约：event_id、event_type、aggregate_type、aggregate_id、aggregate_version、schema_version、occurred_at、data。不是计划示意中的 payload。ID 与聚合版本按 JSON 字符串，schema_version 为数字，目前仅接受 1，data 为原始 JSON 对象。通用封装仅校验元信息、版本、聚合类型和 JSON 对象形态；具体业务字段、状态和 event_type 支持范围由业务消费者验证。当前支持 product/order 的 `*.v1` 类型；未知信封版本显式拒绝，不静默跳过。

商品已有 PublishedEnvelope 保持兼容，不强行迁移业务结构。后续 Publisher 必须从持久化 Outbox 的元信息和 Payload（仅 data）组装信封，不在重试时重新生成 event_id 或 occurred_at。公共包不直接引用服务 Repository 或业务模型。

## Producer ACK

`NewProducer(Config, Topics, timeout)` 创建独立 franz-go 客户端，acks=all，保留客户端默认幂等生产机制。`Publish(ctx, Envelope)` 按 aggregate_type 选择主 Topic，以十进制 aggregate_id 为 Key，等待 ProduceSync 最终 ACK 后返回 Topic/Partition/Offset Receipt。OccurredAt 输出 UTC；返回成功不代表消费者已处理。

超时派生自上游 Context，失败可能是结果未知，不能据此断言 Broker 没有落消息。Outbox 后续必须仅在成功 Receipt 后标记发布完成，并复用逻辑事件 ID 重试。客户端幂等不提供跨进程/跨 Outbox 事务的恰好一次语义；业务消费者仍需持久化去重与聚合版本校验。单节点 acks=all 也不提供多副本容灾。

调用方等待所有 Publish 返回后 Close；不共享此连接给消费者，不在关闭后继续发布。重试/DLT Topic 已配置，但 Publish 只写主 Topic，可靠转移在后续消费者阶段实现，当前不伪造重试能力。

## Consumer Group 与 Offset

`NewConsumer(Config, ConsumerConfig)` 要求明确 Group 和 Topic 列表，禁用自动提交，无已提交 Offset 时从保留范围起点消费。显式设置 HandleTimeout、CommitTimeout（各不超过1分钟），rebalance timeout 为3分钟。

`Run(ctx, Handler)` 每次 PollRecords(1)，串行处理；处理器只有在业务持久化完成后才能返回 nil，随后使用上游派生提交超时同步 CommitRecords，提交的是该消息 offset+1。处理器收到原始 Key/Value 及 Topic/Partition/Offset，可自行调用 DecodeEnvelope；不替业务决定解析失败应重试还是死信。

BlockRebalanceOnPoll 把处理与提交限制在同一轮分区所有权内，结束后 AllowRebalance。任何处理、拉取或提交错误立即返回，关闭客户端，不能继续消费同分区的后续记录跨过失败消息；Consumer 一次性使用，失败后由上层退避并重建同组。Run 返回不自动重试，不自动发布 DLT。提交失败或处理成功后进程崩溃可能重复消费，需要业务幂等。

处理器必须响应 Context，不能把业务丢给后台 goroutine 就返回成功；Go 不能强制终止忽略 Context 的处理器，业务与提交必须在 rebalance timeout 内完成。退出先取消 Run Context 并等待返回，Run 自动释放连接；创建后不运行的实例需 Close。禁止运行期间并发调用 Close 来替代取消协议。

## 验证

单元测试通过：大整数精度、非法信封、版本与聚合不匹配、Topic 映射与配置，以及已有商品 JSON 示例兼容性。

真实 Kafka 4.1.2 单节点 race 测试通过：临时唯一 Topic/Group、两条同步发布成功返回 ACK；首条处理失败后同组重建再次收到首条；首条处理成功提交后第二条失败，再重建从第二条继续；失败实例不可复用，取消发布不报告成功。只操作和清理本次测试 Topic/Group，不写项目事件 Topic。

复验：根目录加载 .env 后进入 backend，执行 `KAFKA_INTEGRATION=1 go test -race ./pkg/kafka -count=1 -v`；未设置集成开关时明确跳过真实 Broker 测试。全量 `go test ./...` 检查其他模块回归。

本阶段未接入业务 Outbox Publisher、消费者持久化去重、Retry/DLT、跨实例 rebalance 故障注入或 Broker 中断；不得表述为可靠事件链路已完整验收。

参考：[franz-go v1.22.3 文档](https://pkg.go.dev/github.com/twmb/franz-go@v1.22.3/pkg/kgo)，尤其 BlockRebalanceOnPoll、PollRecords、CommitRecords 的配套约束。

Outbox 发布器已单独实现，状态机、配置与验证边界见 [Outbox Publisher](outbox-publisher.md)。本页此前“未接入”是基础封装阶段范围；消费者幂等和 Retry/DLT 仍未实现。
