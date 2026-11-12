# product.published.v1

契约见 `product.published.v1.schema.json`，完整消息示例见 `product.published.v1.example.json`。当前只定义首次发布事件，不提前实现消费者或 MQ 投递。

## 产生条件

仅在商品首次从 DRAFT 成功进入 ON_SALE 时生成。后续 Service 必须在同一数据库事务中锁定商品、校验身份和版本、更新状态与首次发布时间、递增 aggregate_version，再插入 Outbox。新草稿版本为 1，首次发布后的版本至少为 2；草稿被编辑过时可更大。

重新上架使用 product.updated.v1，保留首次发布时间，不重复产生本事件。对应 updated、off_shelf 等契约将在实现相应业务时补充。

## 数据约定

- event_id 是 UUID v4，消息重投复用同一 ID。
- aggregate_type 固定 product，aggregate_id 与 data.product_id 必须相同。
- aggregate_version 对应更新后的商品版本；schema_version 固定整数 1，两者含义不同。
- data 是发布完成时的快照，包含商品 ID、卖家、分类、标题、描述、价格、状态及首次发布时间。当前不要求图片；图片能力和契约扩展留至后续。
- ID、聚合版本、金额以十进制字符串传输，必须在正数有符号 int64 范围内。JSON Schema 的 pattern 约束格式，数值上限及跨字段一致性需消费者额外检查。
- 标题非空且不超过 128 个 Unicode 字符，描述合法 UTF-8 且不超过 16 KiB；字节长度、非空白及时间先后等语义约束不能仅依赖本 Schema。
- 时间使用 UTC RFC3339，精度不超过微秒；occurred_at 不早于首次发布时间。
- outbox_events.payload **仅存 data**；event_id、类型、聚合标识、版本、occurred_at 来自 Outbox 独立列，发布器组装消息信封，避免重复存储。

Go 构造函数 `events.NewPublishedOutbox` 仅从已发布快照生成 PENDING 记录，不执行状态转换、不判断历史是否已经发布、不落库。数据库事务及首次发布判断仍由后续 Service/Repository 实现。

消费者需按 event_id 幂等，对状态快照按 aggregate_version 防止旧事件覆盖新视图。Feed 的首次发布动态应与搜索状态投影分别处理，不能简单以“见过更高版本”为由丢弃尚未处理的首次发布业务动作。
