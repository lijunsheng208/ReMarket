# Order Service 与 gRPC（第 1 批）

已实现 Order Proto、生成代码、独立进程入口及 Handler → Service → Repository 查询链路。GetOrder、ListOrders 接入独立 JWT 验签和 Order 自有数据库；CreateOrder 已在后续批次实现幂等 PENDING 意图落库（已有定时预占恢复执行器）；CancelOrder、ConfirmOrder 已在最新批次接入，见 [订单决定与恢复](order-decisions.md)。Product ReserveProduct 已在后续批次补齐，Order 尚未接入，不宣称已实现订单创建或跨服务编排。

## 查询契约

GetOrder 只接受订单 ID，当前身份来自已验证 JWT。数据库查询同时约束 buyer_id / seller_id，非参与人与不存在统一返回 NotFound，管理员不享有额外读取权限。无有效 JWT 返回 Unauthenticated，非正 ID 返回 InvalidArgument。

ListOrders 必须选择 BUYER / SELLER，限定认证用户。page 默认 1、page_size 默认 20，最大 100；负数、未知角色或偏移超过有符号 32 位范围返回 InvalidArgument。按 created_at DESC、id DESC 稳定排序；只读 REPEATABLE READ 事务保证列表及总数来自同一快照。不同分页请求之间不保证快照不变。

响应包含金额快照、双方确认标志、状态、聚合版本及时间，不返回幂等键、请求摘要、操作内部错误。Proto JSON 中 int64 为字符串，时间使用 Timestamp，closed_at 未关闭时为空。COMPLETED 不代表线上支付。

写接口暂定 CreateOrder(product_id, idempotency_key)、CancelOrder(id)、ConfirmOrder(id)，均不接受用户 ID、金额或卖家等可伪造字段。写操作业务、并发与补偿将在后续批次实现。

## 启动与配置

先 review 并通过正式迁移流程向 Order 业务库应用 `migrations/order/000001_core.up.sql`，本轮未代执行。Order 不自动建表或迁移。导出配置后执行 `make run-order`：

- ORDER_GRPC_ADDR：默认 127.0.0.1:50053。
- ORDER_MYSQL_ADDR / ORDER_MYSQL_USERNAME / MYSQL_ORDER_PASSWORD / ORDER_MYSQL_DATABASE：独立 Order 数据库账号与连接。
- JWT_SECRET / JWT_ISSUER / JWT_AUDIENCE / JWT_ACCESS_TTL：沿用现有 JWT 配置，由 Order 独立校验。

进程不会自动加载 .env。复用公共 Request ID、上游取消与最多五秒 RPC 超时及信号优雅退出。健康检查匿名，其余 RPC 强制认证；当前健康检查不代表业务表已迁移。本地 gRPC 明文仅用于开发，尚未实现 TLS。Gateway 尚未接入订单路由，前端未改动，尚未进行独立进程启动和 Gateway 验收。

## 本轮验收

真实 gRPC（bufconn）→Handler→Service→Repository→独立临时 MySQL 的 race 测试通过：买家/卖家详情、非参与人隔离、拒绝伪造身份头与匿名访问、无效 ID 和不存在订单、买卖列表角色隔离、稳定分页和总数、空列表、无效角色和分页、Request ID 传递、三个写 RPC 明确 Unimplemented。订单 Schema 及状态转换测试也通过。

全量 Go 测试、make proto、gofmt 和 git diff --check 通过。临时库已清理，业务库未修改。测试使用直接构造的订单夹具，不表示真实下单、预占或订单恢复已实现。

## 后续 review 批次

先补齐 Product ReserveProduct 和 Order 预占请求快照持久化，再实现创建与 Product RPC 编排；随后实现取消/双方确认、幂等恢复、Gateway 及联调。各批次完成后停止供 review，不将 Unimplemented 方法计入功能完成。

最新幂等持久化范围与未完成项见 [幂等与补偿](order-idempotency.md)。启动前还需应用 Order 000002 快照迁移；第 1 批历史验收的创建 Unimplemented 只适用于未注入 Creator 的查询测试。

最新第 2 批已接入 RESERVE 领取、结果查询和重试，详见幂等与补偿文档；当前 Order 启动需要配置与 Product 相同的独立 ORDER_PRODUCT_SERVICE_TOKEN。取消、确认、过期及 HTTP 订单路由仍未实现。

最新 HTTP 接入：Gateway 已提供创建、详情及买卖列表，校验唯一 Idempotency-Key 并原样传递；详见 [订单 HTTP API](gateway-orders.md)。此前“Gateway 尚未接入”为历史范围，独立进程联调仍未验收。

最新取消与确认能力、恢复行为和实际验收边界见 [订单决定与恢复](order-decisions.md)，覆盖此前尚未实现的历史记录；过期扫描仍待实现。

最新超时能力：到期 RESERVED 扫描及安全释放已实现，与确认/取消共用订单锁，详见 [超时订单处理](order-expiry.md)。Order 进程启动同时运行恢复执行器及过期扫描器，此条覆盖此前过期扫描待实现的历史范围。
