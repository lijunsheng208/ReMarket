# ReMarket 事件契约

主事件使用 common/envelope.v1.schema.json；data 为服务自有载荷，不能改为 payload。Outbox payload 列只存 data，发布器从独立元信息列组装。Product 与 Order 金额、ID、聚合版本均为十进制字符串，schema_version 为数字；需要额外验证 int64 上限及跨字段一致性。

| 聚合 | 主 Topic | 契约 |
| --- | --- | --- |
| Product | remarket.product.events.v1 | product/ 下六个事件 Schema 与示例 |
| Order | remarket.order.events.v1 | order/order.events.v1.schema.json 及九个事件示例 |

每个主 Topic 有 events.retry.v1、events.dlt.v1 对应链路。Retry/DLT 的 Record Value 是 common/transfer.v1.schema.json 定义的 Packet，不是裸 Envelope；original_value 为完整原始消息的 base64，original_key 同样保留字节。重放生成新 round，保留原始 event_id；原始损坏消息可进入 DLT，但需修复后才能显式重放。

主链按 aggregate_id 为 Kafka Key。生产端 acks=all 不等于端到端恰好一次；消费端必须持久化 event_id 去重并校验聚合版本，Retry/DLT 发布确认后才提交原 Offset。并发 Publisher/Retry 不承诺同一聚合的业务版本到达顺序。

通用 Envelope 校验比业务 Schema 宽松：例如技术测试允许非UUID event_id，业务构造器生成 UUID v4；通用测试消费者仅维护版本化视图，不声称验证完整商品/订单业务载荷。JSON Schema 中的 format/contentEncoding 需要校验器支持，运行时仍须做语义校验。
