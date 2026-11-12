# Gateway 订单 HTTP 接口

本批新增三个受保护入口，均由 Gateway 与 Order 独立校验用户 JWT；原 Token、Request ID 和派生超时传递到 Order，不传递用户伪造的 x-user-id / x-role。Gateway 不持有 Product 内部服务凭据，也不参与订单跨服务事务。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| POST | /api/v1/orders | 幂等创建订单 |
| GET | /api/v1/orders/:id | 查询本人参与的订单 |
| GET | /api/v1/orders?role=buyer 或 seller | 查询本人买入或卖出的订单 |

## 创建与幂等

必须提供唯一 `Idempotency-Key` 请求头，1～128 字节、无空格可打印 ASCII、区分大小写。重复头（即使相同值）、缺失、空值、过长或含空白均返回 400。不清理或改写键，原样转换为 CreateOrderRequest.idempotency_key。幂等范围、摘要比较和持久化由 Order 执行，Gateway 不做内存去重。

```http
POST /api/v1/orders
Authorization: Bearer <用户访问令牌>
Content-Type: application/json
Idempotency-Key: checkout-20261112-001

{"product_id":"9007199254740993"}
```

请求体仅允许 product_id，必须为正 BIGINT 的字符串；金额、参与人、状态及 idempotency_key 请求体字段均拒绝，沿用 16 KiB JSON 大小限制。重复同键同商品返回既有订单；同键换商品由 Order 返回 AlreadyExists，HTTP 映射 409。

订单仍 PENDING 返回 202 Accepted，表示已持久化并等待后台预占；已取得其他状态返回 200，包含当前真实状态（包括 FAILED），不表示一定购买成功。无法区分首次与幂等重放，因此不返回 201。成功响应含 Location: /api/v1/orders/<id>，Cache-Control: no-store，ID/金额/版本按 Proto JSON 为字符串。客户端应读取响应状态并查询详情，不能把 202 当作预占成功。

HTTP 超时或断开不证明订单未创建，应使用相同幂等键和原参数重试，禁止自动换键产生第二个订单。

## 查询

详情 ID 必须是正数有符号 BIGINT，归属授权由 Order 处理，非参与人与不存在统一 404。列表要求唯一 role=buyer 或 role=seller；page 默认 1，page_size 默认 20，显式设置时必须为正整数，page_size 最大 100。重复、未知查询参数（包括 user_id）均拒绝，偏移范围由 Order 再次校验。

统一错误映射：无效认证 401、参数 400、归属/记录不存在 404、幂等冲突 409、上游不可用 503、调用超时 504、未知故障 500。内部错误不回显 SQL、连接地址、Token 等细节。

## 配置与范围

Gateway 新增 ORDER_GRPC_TARGET，默认 127.0.0.1:50053；沿用 GATEWAY_RPC_TIMEOUT 和 JWT 配置。连接是惰性的，创建 Client 成功不代表 Order 可达。Order 启动仍需独立数据库和 ORDER_PRODUCT_SERVICE_TOKEN；业务库迁移不由 Gateway 执行。

本批未新增取消、确认 HTTP 路由，Order 对应业务仍未实现；未修改前端或执行订单过期关闭。

## 本轮验证

真实 HTTP 请求适配→bufconn gRPC→模拟 Order（真实 JWT 拦截器）race 测试通过：唯一幂等键原样传递、重复请求转换一致、缺失/重复/非法键拒绝、请求体字段白名单、匿名及无效 JWT 拒绝、伪造身份头隔离、原 Token/Request ID/Deadline 传递、大整数无损、详情与分页转换、未知/重复查询参数拒绝、PENDING 202 和已完成预占 200、409/500/503/504 错误映射、敏感错误隐藏。测试中的 Order 为模拟实现，真实数据库幂等由此前 Order 临时 MySQL 测试覆盖，不把本轮适配测试称为完整端到端联调。

尚未完成 Gateway→独立 Order→独立 Product 的真实进程联调、浏览器验收与进程故障注入。业务库未修改。

## 最新终结入口

已新增 POST /api/v1/orders/:id/cancel 与 /confirm，要求有效 JWT 及空请求体。处理中 202、单方确认或已落地重放 200，业务规则及验收边界见 [订单决定与恢复](order-decisions.md)。这覆盖此前“未新增取消/确认路由”的历史记录。
