# 订单取消、双方确认与终结恢复

本批实现 CancelOrder、ConfirmOrder、对应 HTTP 路由，以及 RELEASE / CONFIRM 持久化操作的领取、执行和恢复。过期扫描已在后续批次接入，详见 [超时订单处理](order-expiry.md)；下文未完成项为历史批次范围。

## 业务与并发边界

Order 独立验签，买卖双方才可操作；无效 ID 为 InvalidArgument，非参与人与不存在统一 NotFound。管理员不享额外权限。事务先锁订单，再取数据库 UTC 微秒时间；新决定要求 expires_at > now。

- RESERVED 任一方可取消，即使另一方已经单方确认；进入 CANCELLING，同时创建唯一 RELEASE 操作与 order.cancelling.v1。
- 首次确认只设置当前参与者标志，仍为 RESERVED，写 order.confirmation_recorded.v1，不创建远程操作。
- 第二方确认进入 CONFIRMING，同时创建唯一 CONFIRM 操作与 order.confirming.v1。
- CANCELLING / CANCELLED 重复取消返回已有结果；CONFIRMING / COMPLETED 重复确认返回已有结果；已记录的单方确认重复调用不改变期限、版本或事件。相反终态请求拒绝。
- 不支持 PENDING 主动取消。CONFIRMING、CANCELLING 是不可逆决定，不允许互换。

所有实际变更递增 aggregate_version，更新订单、操作及 Outbox 同事务提交；事件包含双方确认标志。数据库行锁及版本条件使第二方确认与取消只能一个决定获胜。外部 Product RPC 在事务提交后执行，不能持锁等待网络。

## 终结恢复

统一 ClaimOperation / FinishOperation 承接三种操作，保留一分钟租约、令牌校验与订单→操作锁序。候选操作类型必须与订单状态一致：RESERVE→PENDING，CONFIRM→CONFIRMING，RELEASE→CANCELLING/EXPIRING。EXPIRING 仅预留落地支持，当前没有扫描器产生该决定。

终结操作先 GetReservation；核对订单、商品、买家及绝对期限。已处目标终态直接完成本地落地；ACTIVE 才调用 ConfirmReservation / ReleaseReservation。写 RPC 返回错误时再次查询，查到目标终态则完成；不存在、状态相反、未知或查询失败均保留 RETRY，不宣告成功，不反向释放或重新成交。

成功 CONFIRM：订单→COMPLETED，操作→SUCCEEDED，设置 closed_at，写 order.completed.v1。成功 RELEASE：CANCELLING→CANCELLED，写 order.cancelled.v1；EXPIRING→EXPIRED 的落地分支留供下一批扫描器使用。订单、操作与终态事件同事务，事件故障不会部分提交。租约旧令牌不得覆盖当前决定。

重复请求由业务状态和 (order_id,operation_type) 唯一约束实现幂等，不要求新 Idempotency-Key。不能将 Product 的确认成功理解为真实在线支付。

## HTTP

- POST /api/v1/orders/:id/cancel
- POST /api/v1/orders/:id/confirm

两者要求 Bearer 用户 JWT，无请求体；任何非空 body（包括 {}）返回 400，避免静默忽略伪造参数。CANCELLING / CONFIRMING 返回 202，单方确认仍 RESERVED 或已完成的重复请求返回 200；带 Location 查询当前状态。超时后重复原操作或查详情，不切换反向操作。Order gRPC 同样可查询实际状态。

## 本轮实际验证

独立临时 MySQL + Order Service 业务 + 模拟 Product 真实 gRPC race 测试通过：参与者隔离、首方/重复确认、第二方成交决定、确认后取消拒绝、单方确认后取消、重复取消、确认与取消并发唯一决定、截止后新请求拒绝、决定事件写入失败全量回滚。模拟 Product 在释放/确认提交后返回 DeadlineExceeded，执行器查询恢复为 CANCELLED / COMPLETED。测试中的订单决定直接调用 Service，不宣称这部分是 Order gRPC 全链路。

既有 RESERVE 租约与恢复、创建、查询、Schema 和 Gateway race 测试也通过；Gateway 新增终结适配用例覆盖空体请求、202/200、身份与参数拦截。全量 Go 测试、make proto、gofmt 和 git diff --check 通过。临时库已清理，业务库未修改，无新增迁移。

未完成：第 5 项过期扫描、过期与确认/取消三方竞争实测、真实独立进程 Order→Product 联调及故障注入。历史文档中的“取消/确认未实现”由本文当前进度覆盖。

最新进度：到期扫描及过期/确认/取消仲裁已实现，并通过临时 MySQL race 测试，见超时订单文档。独立进程联调与真实网络故障注入仍待验收。
