# 订单幂等与补偿：第 1 批（创建意图持久化）

本轮完成 Order CreateOrder RPC 的幂等创建落库及待执行 RESERVE 操作持久化。尚未执行 ReserveProduct，不推进 RESERVED，不运行补偿扫描器，不将第 4 项整体标记完成。

## 请求及身份

CreateOrder(product_id, idempotency_key) 从独立验签后的 Context 获取买家，客户端不提供金额、卖家或买家 ID。幂等键为 1～128 字节、无空格的可打印 ASCII，区分大小写；同买家同键只对应一个订单。摘要使用规范化的版本化 product_id 请求做 SHA-256，不纳入 Token、Request ID、可变商品快照或查询时间。

先按买家及幂等键查询；相同摘要返回任何状态的已有订单，不重新读取商品或延长期限；不同摘要返回 AlreadyExists。首次请求经 Product GetProduct RPC 获取可信金额、卖家、商品版本，验证在售及禁止自购后落库。商品可能在读取后变化，后续 ReserveProduct 仍须校验快照，当前读取不等于已预占。

GetProduct 使用公共 gRPC Client 的上游 Context/凭据/Request ID 传递及 3 秒超时。商品 RPC 不成功时不创建订单。当前只接入商品读取，未使用 Order 内部服务凭据调用预占。

## 本地事务和恢复快照

orders、PENDING RESERVE order_operations 和 order.created.v1 Outbox 同事务保存。首次取得订单 ID 后再构造操作/事件；任一步失败整体回滚。唯一键竞争失败后回滚并读取赢家，再比较摘要，不产生第二个订单或重复操作/事件。

新增迁移 `migrations/order/000002_product_snapshot.up.sql` 保存 expected_product_version。卖家、价格、买家、商品及绝对 expires_at 已在订单中保存，结合版本即可重建完整预占请求。历史订单版本保持 NULL，未来恢复执行器不得从当前商品猜测历史版本。新订单明确写入版本。迁移未应用到业务库；启动前需在 review 后按流程应用，不自动建表。

首批期限固定 30 分钟，从首次可信快照读取完成后起算并截断 UTC 微秒。重复请求不延长。后续可配置化，但不得改变已持久化期限。

## 当前运行语义与未完成项

成功返回 PENDING，表示创建意图及操作记录已持久化，**不表示商品已预占或购买成功**。本批没有后台执行器，新 PENDING 订单不会自动继续，须等待后续补偿批次；不能作为可上线下单闭环验收。取消及确认仍 Unimplemented。

Gateway 尚无订单路由，因此 HTTP Idempotency-Key 的提取、重复请求头校验和 HTTP 状态语义尚未实现；目前仅支持 gRPC idempotency_key。后续还需实现：任务领取和租约、RPC 调用/未知结果查询、操作状态推进、退避、定时扫描、崩溃恢复与相应故障测试。第 5 项过期关闭也不在本批。

## 验证

真实 gRPC（bufconn）→Order 业务→独立临时 MySQL race 测试通过：20 个同键并发返回同一订单且订单/操作/事件各一条、恢复版本及参数落库、商品读取故障后重复请求仍返回既有订单、同键换商品冲突、非法键/匿名请求拒绝、Outbox 故障三表全部回滚。商品读取使用模拟业务接口，未进行真实 Product 进程联调。既有订单 Schema 和查询测试已应用新增迁移并回归通过。临时库清理成功，业务库未修改。
