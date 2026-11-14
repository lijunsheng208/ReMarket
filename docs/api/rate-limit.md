# Gateway Redis Lua 限流

实现：`backend/pkg/ratelimit` 提供与 HTTP 无关的令牌桶，`backend/internal/gateway/ratelimit.go` 提供配置与中间件。Gateway 入口在注册业务路由之前安装中间件，保留 Request ID、统一错误结构及 no-store。现有调用方仍可不传中间件，不影响测试与其他路由组装。

## 开启与配置

`GATEWAY_RATE_LIMIT_ENABLED` 默认 false，设 true 后重启 Gateway。使用现有 Redis 连接配置；开启时启动 PING 失败则拒绝启动，运行期间故障按下文降级。此次仅实现与验收，不修改本机开关或重启当前 Gateway。

| 环境变量 | 默认值/作用 |
| --- | --- |
| GATEWAY_RATE_LIMIT_TIMEOUT | 100ms，整个请求两次 Redis 检查共享预算，上游 Deadline 更早时遵守上游 |
| GATEWAY_RATE_LIMIT_USER | capacity=20，tokens=20，interval=1s，验签用户总桶 |
| GATEWAY_RATE_LIMIT_IP | capacity=40，tokens=40，interval=1s，匿名 IP 总桶 |
| GATEWAY_RATE_LIMIT_ROUTE | capacity=20，tokens=20，interval=1s，身份+接口默认桶 |
| GATEWAY_RATE_LIMIT_ROUTES | 按 `METHOD /api/路由模板` 覆盖接口规则 |

规则为 JSON，例如 `'{"capacity":5,"tokens":5,"interval":"1m"}'`。capacity 是最大积累令牌（突发），tokens/interval 是连续补充速度；5/min 不等于任意滑动一分钟严格最多 5 次。数字必须为正整数且不超过一百万，interval 为 1ms～24h，满桶恢复时间不超过七天。规则修改需重启，多个 Gateway 必须使用同一份配置，滚动期间不同参数会产生不一致的配额语义。

默认接口覆盖：登录、注册各 IP 5/min、突发5；创建订单各用户 2/s、突发2。其他路由按方法+Gin 模板限流，例如所有 `/products/:id` 共享该身份的详情接口桶，路径参数不产生无限新 Key。总桶与接口桶依次扣费，后一个拒绝不退回前一桶令牌，明确采用保守计费；不使用多 Key 脚本，不要求 Key 在 Cluster 同槽，但当前连接配置仍为单节点。

登录/注册即使附带有效 Token 也始终按 IP。其他 API 有效 JWT 使用可信 user ID，无效凭据按 IP 做保护后仍由原路由鉴权拒绝，不把任意 x-user-id/x-role 当作身份。中间件独立验签只为限流计费，不替代后续鉴权和下游认证；受保护路由可能再次验签。

Gateway 已禁用可信代理，ClientIP 使用 TCP 对端，忽略伪造 X-Forwarded-For。接入真实反向代理时必须显式配置有限可信代理范围，并另行测试，禁止信任所有代理。Key 将 IP/用户与接口散列，不保存 Token 或原始 IP；未覆盖路由/healthz 不访问 Redis。未实现按请求体 product_id 的全局购买尝试桶。

## 原子性与响应

Lua 通过 Redis TIME 获取毫秒时间，原子读取、补充、检查和扣除令牌，使用 PEXPIRE 回收空闲状态。时钟回退不会重复补充，Retry-After 包含等待回退时钟恢复的时间。Key 为 `ratelimit:v1:{scope}:{sha256}`，不滑动窗口计数。Redis 脚本不可用返回错误，不伪造“允许”。

额度不足返回 429、`error.code=ResourceExhausted` 和向上取整至少 1 秒的 Retry-After，含统一 request_id。每次通过只表示当前限流规则放行，不代表业务成功；认证失败和业务失败仍消耗额度，重放幂等请求也消耗额度。Redis 重启、淘汰或故障切换可能重置桶，不提供计费账本级配额保证。

运行期 Redis 错误：GET 使用每 Gateway 实例总桶（20/s、突发20）有界降级，通过时响应头 X-RateLimit-Mode=local；本地桶耗尽返回429。该桶为固定一份状态，不创建无限用户 map，不保证故障时跨实例共享配额。其他方法含登录/注册/订单写入返回503，不以429伪装 Redis 故障，不放行敏感写。请求已取消返回499，不继续业务。此降级固定规则，后续可按真实容量调整；并不把 Redis 故障时的读取流量全部放行。

## 验证

真实 Redis race 测试：两个独立客户端同时竞争 5 个令牌，40 次请求只通过5次；验证 TTL、时间基准推进后的补充及 Context 取消。两个真实 HTTP 测试服务分别持有独立 Redis 客户端，相同有效用户前两次放行、后两次429，证明共享接口额度。测试使用独立 Key 并清理，不影响现有配额。

HTTP 单测覆盖统一429/Retry-After/request_id、防伪代理IP与用户头、有效用户计费、登录始终使用IP、敏感接口故障503、本地读取有界降级及 healthz 豁免。配置测试覆盖 JSON 覆盖和非法参数。运行 `REDIS_INTEGRATION=1 go test -race ./pkg/ratelimit ./internal/gateway -count=1`，需先加载 .env；无集成开关明确跳过真实测试。

验证边界：共享额度测试为同进程中两个独立 HTTP Server，不代表已做双进程部署压测；未验证 Redis 服务端切主/重启、时钟回退的真实故障注入、代理部署或生产阈值。默认阈值只供开发测试。

算法背景参考：[Redis 令牌桶与 Go](https://redis.io/docs/latest/develop/use-cases/rate-limiter/go/)。
