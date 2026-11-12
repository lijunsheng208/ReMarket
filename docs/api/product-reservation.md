# Product 预占 RPC 契约与实现进度

第 1 批完成 Proto、生成代码及业务规则设计。第 2 批已接入独立服务鉴权、快照增量迁移及模型。四个 RPC 现均已接入 Handler → Service → Repository 和事务事件写入；分批历史记录见下文。Gateway 不新增这些接口。

## 调用身份与边界

ReserveProduct、GetReservation、ReleaseReservation、ConfirmReservation 仅允许 Order 服务调用。已接入独立 Order 服务凭据：开发环境采用独立高熵服务 Token，Product 按完整方法路径验证，不能与用户 JWT_SECRET 或用户 access token 复用；未配置时拒绝服务调用，不退化为用户鉴权。部署环境使用 TLS 保护凭据，后续可替换为 mTLS；当前未实现 TLS 或 mTLS。

Order 必须先验证买卖双方身份、订单归属及状态，并持久化操作决定，然后携服务身份调用。buyer_id 等参数是经服务认证后信任的 Order 声明，不读取 x-user-id/x-role，不把普通用户 Token 当作内部服务授权。Product 仍校验禁止本人购买及参数与预占一致。服务凭据不得向 Gateway、浏览器或日志暴露。

## 参数与结果

所有 ID、金额、版本为正 int64，时间使用有效 Timestamp、UTC 和微秒精度。expires_at 必须为精确微秒时间（纳秒部分能被 1000 整除），禁止隐式截断导致幂等比较失真；期限由 Order 持久化，同一订单重试不得延长。首次预占必须在数据库当前时间之前尚未到期，具体最大预占时长待配置阶段确定。

- ReserveProduct：order_id、product_id、buyer_id、expected_seller_id、expected_price_cent、expected_product_version、expires_at。首次预占同时检查在售、卖家、价格与版本，防止查询后改价或上下架产生陈旧快照。Order 必须在首次调用前持久化完整请求参数，恢复时不可重新读取商品拼接不同参数。
- GetReservation：按 order_id 查询实际记录，不因 expires_at 到达返回不存在。
- ReleaseReservation / ConfirmReservation：必须传 order_id 和 product_id，校验归属；无通用“直接修改商品状态”接口。
- ReservationResponse：仅返回实际预占记录，不附带后来可能变更的商品数据；ACTIVE / RELEASED / CONFIRMED 对应现有数据库值。UNSPECIFIED 不可落库。

已新增 000003_reservation_snapshot 增量迁移及可空模型字段。历史记录三项均为空，不从当前商品伪造历史值；新快照三项必须全部有效。后续 Reserve 必须完整写入快照，对历史未知快照返回 FailedPrecondition，不宣称完整幂等匹配。未向业务库应用迁移。Order 也须在后续创建编排中持久化预占请求快照；本轮不提前修改订单表或实现该流程。

## 幂等与状态语义

| 操作 | 当前预占 | 处理规则 |
| --- | --- | --- |
| Reserve | 不存在 | 校验全部参数、期限及商品快照后原子预占 |
| Reserve | 任意已有状态 | 完整参数相同返回实际历史状态；不同则拒绝；不重新激活 |
| Get | 任意已有状态 | 返回实际状态，即使已经到期或结束 |
| Release | ACTIVE | 商品 RESERVED → ON_SALE，预占 → RELEASED |
| Release | RELEASED | 原样返回，不增加商品版本或写重复事件 |
| Release | CONFIRMED | 拒绝，不得释放已成交商品 |
| Confirm | ACTIVE | 商品 RESERVED → SOLD，预占 → CONFIRMED |
| Confirm | CONFIRMED | 原样返回，不增加版本或写重复事件 |
| Confirm | RELEASED | 拒绝，不得恢复已释放预占 |

Get、Release、Confirm 查询不到记录返回 NotFound，不伪造成功。NotFound 仅表示当前查询快照没有记录，不能证明在途 Reserve 不会随后提交。Reserve 重复请求先匹配历史记录，再判断首次预占的到期与商品条件，否则合法重试会被商品已预占或版本变化误拒绝。

Confirm 不单独用 Product 当前时间否决已到期预占：Order 必须在截止前持久化 CONFIRMING 决定，Product 执行该决定；Product 不得自行扫描并释放过期预占。Release 只执行 Order 的不可逆关闭决定，不校验是买家还是卖家，参与人权限由 Order 负责。

## 事务与并发要求（后续实现）

先锁商品，再锁预占；按 order_id 查到记录用于确定商品时只能先做无锁读取，事务加锁后必须重新核实。Reserve 使用 status + aggregate_version + 卖家 + 价格条件更新并检查 RowsAffected=1，与预占插入和 product.reserved.v1 同一事务；任何一步失败全部回滚。

同商品的不同订单竞争仅一方成功。同订单并发创建还须依赖 order_id 唯一键；唯一键冲突回滚后重新读取并比较参数，不作为未知 Internal 或无条件成功。不同商品使用相同 order_id 必须判定参数冲突。

Release / Confirm 校验商品仍 RESERVED 且预占 ACTIVE，商品与预占同事务更新，分别写 product.released.v1 / product.sold.v1，商品版本只增加一次。确认和释放竞争同样的行，只有一种终态能成功。沿用现有 Outbox 信封规则，消费者仍须幂等和版本校验。

## 错误约定

| 场景 | gRPC 状态 |
| --- | --- |
| 缺失/无效服务凭据 | Unauthenticated |
| 已认证但不是授权 Order 服务 | PermissionDenied |
| 非正数参数、无效或非微秒时间 | InvalidArgument |
| 商品或预占不存在 | NotFound |
| 同订单参数不同 | AlreadyExists |
| 首次预占过期、非在售、自购、价格/卖家/版本不匹配、终态冲突 | FailedPrecondition |
| 数据库/远端超时、取消 | DeadlineExceeded / Canceled |
| 未知数据库错误或不符合预期的数据不一致 | Internal，通用消息 |

错误不得泄露 SQL、服务凭据或敏感请求。RPC 超时是结果未知，Order 通过 GetReservation 与同参数重试恢复，不能立即宣告预占失败。

## Review 与后续批次

本轮 review：四个 RPC 字段、独立服务身份方案、完整快照幂等要求、到期及确认/释放边界。通过后再分批实现服务鉴权与快照迁移、Reserve 本地事务、查询/释放/确认、并发和故障测试。每批完成后停下 review。

## 本轮验证

`make proto`、`cd backend && go test ./...`、`git diff --check` 通过。首次测试受沙箱 Go 缓存访问权限限制，放宽权限后全量通过。本轮没有数据库变更，未运行预占业务的数据库或并发验收；全量测试通过不代表预占业务已实现。

## 第 2 批配置与验收

Product 读取 ORDER_PRODUCT_SERVICE_TOKEN：空值禁用四个预占方法；非空值至少 32 字节、无空白，不得等于 JWT_SECRET，否则启动失败且不回显密钥。使用标准 authorization Bearer metadata，固定长度哈希常量时间比较。缺失、错误、重复凭据及普通用户/管理员 JWT 均返回 Unauthenticated。服务凭据不能用于用户写接口。当前只有一个 Order 服务身份，尚无其他已认证服务的 PermissionDenied 分支。

真实 gRPC 测试覆盖四方法凭据隔离、禁用配置及用户写接口；临时 MySQL 库先建表、插入历史记录，再应用快照迁移，验证历史保留、非法部分/零价/版本/自购快照拒绝及新快照读写。三项均 NULL 为历史兼容保留，后续 Service 必须保证新记录完整快照，SQL 不能区分新旧请求。

Product model 与 handler 的真实 MySQL race 测试、全量 Go 测试、make proto 和差异检查通过；临时库已清理，业务库未修改。四个预占业务尚未实现。

## 第 3 批：查询、释放及确认

已实现 GetReservation、ReleaseReservation、ConfirmReservation，并接入 Product 进程。沿用独立服务凭据，未新增 HTTP 路由。查询按 order_id 返回实际状态；结束或到期记录仍可查询。释放与确认按商品→预占行锁顺序处理，校验订单与商品匹配，ACTIVE 分别转换为 RELEASED / CONFIRMED，商品分别 RESERVED→ON_SALE / SOLD，商品版本加一；两行更新和 product.released.v1 / product.sold.v1 Outbox 同本地事务提交。

相同目标终态重复调用原样返回，不增加版本和事件；另一终态返回 FailedPrecondition。历史 RELEASED 记录不受商品后续再预占影响。确认执行 Order 已持久化的决定，Product 不自行根据本地过期时间否决或释放。当前未实现 Order 决策流程，调用方仍须遵守契约，不能把 Product 执行能力等同于端到端订单功能。

事件 data 沿用商品快照结构，金额和 ID 为 JSON 字符串，不含订单参与人凭据；released 为 ON_SALE 快照、sold 为 SOLD 快照，Outbox 为 PENDING，不执行消息投递。

真实 HTTP 入口未新增。真实 gRPC→Service→Repository→独立临时 MySQL 的 race 测试通过：到期查询、无效参数、记录不存在、错误商品归属、重复释放/确认、相反终态拒绝、20 个确认/释放并发请求只有一种终态成功且只写一个事件、Outbox 故障后商品与预占全部回滚。临时库已清理，业务库未修改；全量 Go 测试和 git diff --check 通过。

测试直接构造 RESERVED 商品及 ACTIVE 预占夹具；ReserveProduct 尚未实现，未验收从真实下单创建预占的完整闭环。本批不包括 RPC 超时补偿或服务重启恢复。

## ReserveProduct 补齐与实际验收

已实现完整请求校验、微秒截止时间、禁止自购和商品卖家/金额/版本快照校验。先锁商品、再查预占，锁定后读取数据库 UTC_TIMESTAMP(6) 判定首次期限；READ COMMITTED 避免缺失预占的间隙锁影响不同商品的同订单竞争。条件更新 ON_SALE 商品并递增版本，与 ACTIVE 预占和 product.reserved.v1 快照事件同事务提交。

已有订单优先比较完整参数，返回 ACTIVE 或已结束记录，不重新激活、不增加事件；参数不同返回 AlreadyExists，历史未知快照返回 FailedPrecondition。跨商品同订单唯一键竞争失败后回滚，再读取赢家比较参数，同时兼容 GORM 翻译错误与 MySQL 驱动重复键错误。首次预占截止期限不限制固定最大时长，由可信 Order 持久化业务期限，后续配置阶段可补充上限。

真实 gRPC→业务→独立临时 MySQL race 测试已通过：首次及重复预占、同键不同参数、释放后重复预占、自购/价格/版本/卖家/到期校验、非微秒时间、100 个订单竞争一个商品恰有一个成功、一个订单竞争两个商品一成功一冲突且失败商品回滚、Outbox 故障后商品与预占全部回滚。既有确认释放竞争测试也通过。临时库已清理，业务库未修改。首次跨商品测试发现重复键翻译识别缺失，修正后重跑通过。

当前补齐 Product 前置能力，尚未实现 Order 创建编排、RPC 超时后的定时恢复，不能宣称端到端下单或分布式故障恢复已通过。
