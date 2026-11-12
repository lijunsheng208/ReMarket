# 超时订单处理

本批实现到期扫描、EXPIRING 决定和释放后的 EXPIRED 落地；与已有确认/取消共用订单锁及不可逆中间态。无新增 HTTP、Proto 或数据库迁移；使用现有 (status, expires_at, id) 索引及 RELEASE 操作。

## 扫描与本地事务

Order 启动即运行扫描器，每秒最多领取一百个到期 RESERVED 订单，按 expires_at、id 排序，以 FOR UPDATE SKIP LOCKED 跳过其他实例或请求持有的订单锁。取得锁后重新读取数据库 UTC_TIMESTAMP(6)，只有 expires_at <= now 且仍 RESERVED 才进入 EXPIRING。

订单递增版本、插入唯一 PENDING RELEASE 操作、插入 order.expiring.v1 同事务完成；事件或操作失败则全部回滚。不跨事务调用 Product，不在决定阶段提前设置 closed_at。单方确认标志保留，不阻止安全过期。若发现 RESERVED 中双方均确认等不可能由当前业务产生的数据，返回错误并记录通用扫描错误，不猜测是否可释放。

扫描器与恢复工作者并行运行，避免慢 Product RPC 阻塞过期决定；共享 Order 生命周期，取消后等待两者退出，再关闭数据库及下游连接。异常扫描每轮停止并记录脱敏错误，下一秒继续；尚未接入外部告警平台。

## 截止与竞争规则

确认、主动取消要求 now < expires_at；过期要求 now >= expires_at。两者都在持有同一订单锁后取数据库时间，不根据客户端时间判定。

- 截止前第二方确认已进入 CONFIRMING：到期扫描不处理，恢复工作者继续成交，不释放。
- 截止前主动取消已进入 CANCELLING：继续完成 CANCELLED，不改为 EXPIRED。
- 过期决定先进入 EXPIRING：后续确认/取消返回状态冲突，不创建反向操作。
- PENDING 预占结果未知：扫描器不直接关闭或释放；预占执行器查询恢复为 RESERVED 后，后续扫描再处理到期。
- COMPLETED / CANCELLED / EXPIRED / FAILED 均不重新打开。

数据库行锁与状态条件共同仲裁，重复扫描及多实例扫描只会写一个 RELEASE 和一个过期决定事件。

## 释放与故障恢复

复用统一 RELEASE 执行器：先查询实际预占，ACTIVE 才调用释放；超时后查询，确认 RELEASED 才把订单 EXPIRING→EXPIRED、操作→SUCCEEDED，设置 closed_at 并写 order.expired.v1，同事务提交。

Product 不可用、超时结果未知、查询失败时保持 EXPIRING / RETRY，沿用退避和租约，不假装商品已恢复可售。发现 CONFIRMED 或不匹配预占保留冲突并记录需人工检查，不能释放已成交商品。Product 侧释放事务与商品 ON_SALE 状态、事件一致性由已有 Product 实现保证。

## 本轮验证

独立临时 MySQL + 真实 Order 业务 + 模拟 Product gRPC 的 race 测试通过：

- 已到期订单的扫描、确认、取消同时执行，确认/取消拒绝，只有过期决定生效。
- 重复扫描无变化，十个扫描器仅一个成功且只写一条 RELEASE。
- 远程不可用保留 EXPIRING，恢复后关闭 EXPIRED；模拟远程释放提交后返回超时，经查询仍能落地。
- 截止前 CONFIRMING / CANCELLING 决定即使后来到期也不被覆盖，最终分别 COMPLETED / CANCELLED，成交订单无 RELEASE。
- 过期事件写入失败时订单/操作一起回滚，恢复后可再次扫描和释放。
- 到期 PENDING 不直接关闭，未到期 RESERVED 不处理；后台 Run 可扫描并响应取消。

另有纯业务边界测试验证“截止前一微秒”“恰好截止”“截止后一微秒”。真实数据库测试为避免等待，调整测试夹具及模拟远端的绝对期限；生产不改写订单期限。全量 Go 测试、gofmt、git diff --check 通过，临时库已清理，业务库未修改。

实际验收边界：本轮未做独立 Order/Product 进程全链路、杀进程或真实网络故障注入；数据库与业务逻辑为真实实现，Product 使用模拟 gRPC。不得将本轮验证写成真实部署故障验收。
