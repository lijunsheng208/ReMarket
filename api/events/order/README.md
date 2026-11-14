# Order 事件

order.events.v1.schema.json 覆盖 created/reserved/confirming/cancelling/expiring/completed/cancelled/expired/failed。事件类型与 data.status 必须对应；aggregate_id=data.order_id，金额单位为分，所有关联 ID 不使用跨库外键。buyer_confirmed/seller_confirmed 表示双方决定，不代表在线支付。

这些快照由 Order 自己的事务和 Outbox 构造。事件重投必须复用持久化 event_id；新状态变化使用新的 event_id 和递增 aggregate_version。重放不重新构造业务事件，消费者仍需幂等和旧版本保护。
