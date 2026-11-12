# Gateway 注册与登录

启动 User Service 后执行 `make run-gateway`。Gateway 读取已导出的 GATEWAY_HTTP_ADDR（默认 127.0.0.1:8080）、USER_GRPC_TARGET（默认 127.0.0.1:50051）、GATEWAY_RPC_TIMEOUT（默认 3s，必须大于零且不超过 30s），不自动读取 .env。Gateway 现还要求有效 JWT_SECRET、JWT_ISSUER、JWT_AUDIENCE、JWT_ACCESS_TTL，必须与 User Service 配置一致，否则启动失败或验签失败。gRPC 连接为惰性连接，启动成功不等于 User 可达。

## 路由

| HTTP | 请求 JSON | 成功响应 |
| --- | --- | --- |
| POST /api/v1/auth/register | username、password、nickname | 201，UserResponse |
| POST /api/v1/auth/login | username、password | 200，LoginResponse |
| GET /api/v1/users/me | 无，要求 Authorization: Bearer Token | 200，UserResponse |
| GET /healthz | 无 | 200，仅 Gateway 进程状态 |

注册和登录请求必须为 application/json，最大 16 KiB，拒绝未知字段、类型错误及多段 JSON。注册和登录是匿名路由，业务校验由 User Service 负责。暂未添加 CORS 或限流。

成功响应使用 Proto JSON、snake_case 字段；int64 ID 输出字符串，时间为标准 Timestamp JSON。登录返回 access_token、expires_at、user。响应设置 Cache-Control: no-store，不记录密码、请求体或 Token。

错误结构为 `{"error":{"code":"Unauthenticated","message":"...","request_id":"..."}}`。主要映射：InvalidArgument→400、Unauthenticated→401、PermissionDenied→403、NotFound→404、AlreadyExists/FailedPrecondition/Aborted→409、ResourceExhausted→429、Unavailable→503、DeadlineExceeded→504、Unimplemented→501、Canceled→499（非标准状态）。内部错误隐藏下游描述；JSON 错误返回400，类型不支持415，体积超限413。

每次请求校验或生成 X-Request-ID，同时放入响应头和 RPC Context。匿名 RPC 仅转发追踪 metadata。当前用户接口要求唯一 Authorization 头和 Bearer 方案，由 Gateway 独立验签并写入可信身份 Context，再转发原 Token 供 User Service 再次验签，不转发伪造的用户 ID/角色头。Trace 当前只是头传递，无 Span。

缺失、无效或重复凭据在 Gateway 返回 401，并附带 WWW-Authenticate: Bearer，不访问下游。User 查询到账号已禁用返回 HTTP 403，用户已不存在返回 401；角色以数据库为准。JWT 验签本身不提供退出撤销能力。

HTTP 原 Context 的取消及更早 Deadline 保留，Gateway 再施加 RPC 超时，客户端拦截器继续传递。HTTP 服务限制读写时间并支持五秒优雅关闭。不自动重试注册。当前采用本地明文 HTTP/gRPC，公网需后续 TLS/反向代理配置。

基础测试通过 httptest → Gin → 真实内存 gRPC → 模拟 User 服务验证协议转换、ID 精度、Request ID、超时与取消、错误映射和输入限制。

新增 TestAuthFlowMySQL，使用真实 HTTP 监听、内存 gRPC 传输、真实 User Handler/Service/Repository、bcrypt/JWT 及本机 MySQL。完整验证注册→登录→携带 Token 查询当前用户、篡改令牌拒绝、禁用账号后旧 Token 查询失败及再次登录失败。测试自动创建 remarket_auth_test_ 前缀临时库，应用已有迁移 SQL 后验证并清理，仅测试账号需要建库权限，不操作业务库。

该完整链路及 Gateway race 测试已实测通过；这是组件联调，不代表独立进程/TCP gRPC、Nginx 或生产部署验收。提供 REMARKET_TEST_MYSQL_DSN 后执行 `cd backend && go test -race ./internal/gateway -v`，未配置时跳过 MySQL 用例。
