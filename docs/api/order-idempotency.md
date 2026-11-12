# 订单幂等与补偿（分批实现）

当前完成幂等创建持久化，以及 RESERVE 操作领取、超时结果查询和定时恢复执行器。Gateway HTTP Idempotency-Key 已在第 3 批接入；取消、确认和过期操作尚未实现，不将第 4 项端到端验收标记完成。下文第 1 批未完成项为历史记录，以第 2 批为准。

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

## 第 2 批：RESERVE 执行与恢复

Order 启动时要求配置独立 ORDER_PRODUCT_SERVICE_TOKEN：至少 32 字节、无空白且不等于 JWT_SECRET。缺失或错误配置启动失败，不启动持续无法鉴权的工作者。Product 须配置相同服务凭据。服务 Token 仅显式写入预占 RPC metadata，不复用用户 JWT，沿用 Request ID 与上游取消，单次调用最多两秒且不延长上游期限。当前开发连接为明文，不宣称实现 TLS。

执行器启动即扫描，此后每秒扫描，每轮最多二十条。仅领取 PENDING 订单的 RESERVE 操作：到期 PENDING / RETRY 或租约过期 PROCESSING。先无锁选择候选，再按订单→操作顺序加锁重查资格；锁定订单使用 SKIP LOCKED。数据库时间生成一分钟租约与新 UUID 令牌。多实例通过行锁和重新检查隔离领取；不跨 RPC 持有数据库事务。

先 GetReservation；已有匹配的 ACTIVE 预占直接落地成功。当前查不到记录时才重试持久化原参数的 ReserveProduct。Reserve 出错后再次查询：查到 ACTIVE 则恢复成功；明确业务拒绝且再次查询无记录才落地 FAILED；超时、不可用、查询失败仍为 RETRY。NotFound 本身不证明预占失败。相同订单的幂等与锁序协议由 Product 保证。未知结果不因次数上限自动失败。

成功将订单 PENDING→RESERVED、操作→SUCCEEDED，与 order.reserved.v1 同事务保存；明确拒绝将订单→FAILED、操作→FAILED，与 order.failed.v1 同事务保存。每次实际变更订单版本加一。完成必须匹配当前未过期租约和令牌；过期工作者的结果直接丢弃，由后续领取者查询恢复。

重试清空租约、递增次数（防 int32 溢出）、按 1/2/4/8/16/32 秒退避，随后封顶一分钟；last_error 仅为脱敏分类，不存原始 RPC 错误。缺失历史快照或远程终态/参数不匹配保留重试并记录需要人工检查的日志，不通过反向操作掩盖冲突。当前未实现告警平台或人工修复接口。

工作者随进程 Context 取消退出；入口先等待工作者，再关闭 Product 连接和数据库。取消导致未落地的操作保留 PROCESSING，租约到期可重新领取。订单表与操作为事实来源，重建执行器无需内存任务队列。已到期但实际预占成功的订单仍先记为 RESERVED，第 5 项的安全过期处理尚未实现，本批不会主动释放它。

创建 RPC 仍可先返回 PENDING；之后通过详情或重复同键请求读取最新状态。未提供同步等待预占结果。尚无 HTTP 订单路由，因此还未支持 HTTP Idempotency-Key；目前仅 gRPC 字段。

### 本批实际验收

真实 Order MySQL + 真实 gRPC 协议（模拟 Product 服务）race 测试通过：远端成功但返回 DeadlineExceeded 后查询恢复、有效租约不重复领取、过期租约接管、旧令牌不能覆盖新租约、重建执行器读取持久化任务、远端不可用持久化 RETRY、明确拒绝安全 FAILED、状态事件失败时订单/操作回滚并可再次恢复、后台 Run 处理待执行任务并可取消退出。服务凭据由模拟 Product 明确检查。全量 Go 测试及差异检查通过，临时库已清理，业务库未修改。

这里的远端超时和中断使用模拟服务状态及重建执行器验证，未进行真实 Product 进程联调、杀进程重启或网络故障注入。RELEASE / CONFIRM 操作执行与补偿等待订单取消/确认能力接入，当前执行器只处理 RESERVE。

## 第 3 批：HTTP 接入

Gateway 已实现创建、详情与买卖列表，唯一 Idempotency-Key 头原样传给 Order，同键去重仍由 Order 持久化执行。PENDING 返回 202 和 Location，其他已取得状态返回 200；超时后必须同键重试。真实 HTTP→gRPC 适配及认证 race 测试通过。此条覆盖此前“尚无 HTTP 订单路由”的历史记录，细节与实际验证范围见 [订单 HTTP API](gateway-orders.md)。本批未扩展 RELEASE / CONFIRM 操作或第 5 项过期任务。

## 最新终结操作进度

RELEASE / CONFIRM 已接入统一任务领取、租约及超时查询恢复，详情见 [订单决定与恢复](order-decisions.md)，覆盖此前只处理 RESERVE 的历史范围。订单过期扫描及真实独立进程故障验收仍待完成。

## 最新过期处理进度

已接入 RESERVED 到期扫描，EXPIRING→EXPIRED 复用 RELEASE 持久化恢复，详见 [超时订单处理](order-expiry.md)。这覆盖此前过期扫描未实现的历史记录；真实独立进程故障验收仍待完成。
