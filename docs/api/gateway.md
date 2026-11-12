# Gateway 注册与登录

启动 User Service 后执行 `make run-gateway`。Gateway 读取已导出的 GATEWAY_HTTP_ADDR（默认 127.0.0.1:8080）、USER_GRPC_TARGET（默认 127.0.0.1:50051）、GATEWAY_RPC_TIMEOUT（默认 3s，必须大于零且不超过 30s），不自动读取 .env。gRPC 连接为惰性连接，启动成功不等于 User 可达。

## 路由

| HTTP | 请求 JSON | 成功响应 |
| --- | --- | --- |
| POST /api/v1/auth/register | username、password、nickname | 201，UserResponse |
| POST /api/v1/auth/login | username、password | 200，LoginResponse |
| GET /healthz | 无 | 200，仅 Gateway 进程状态 |

请求必须为 application/json，最大 16 KiB，拒绝未知字段、类型错误及多段 JSON。注册和登录是匿名路由，业务校验由 User Service 负责。暂未添加当前用户 HTTP 接口、JWT HTTP 鉴权、CORS 或限流。

成功响应使用 Proto JSON、snake_case 字段；int64 ID 输出字符串，时间为标准 Timestamp JSON。登录返回 access_token、expires_at、user。响应设置 Cache-Control: no-store，不记录密码、请求体或 Token。

错误结构为 `{"error":{"code":"Unauthenticated","message":"...","request_id":"..."}}`。主要映射：InvalidArgument→400、Unauthenticated→401、PermissionDenied→403、NotFound→404、AlreadyExists/FailedPrecondition/Aborted→409、ResourceExhausted→429、Unavailable→503、DeadlineExceeded→504、Unimplemented→501、Canceled→499（非标准状态）。内部错误隐藏下游描述；JSON 错误返回400，类型不支持415，体积超限413。

每次请求校验或生成 X-Request-ID，同时放入响应头和 RPC Context。只向匿名 RPC 转发追踪 metadata，不转发伪造的用户 ID、角色或 Authorization。Trace 当前只是头传递，无 Span。用户身份传递在后续受保护接口实现。

HTTP 原 Context 的取消及更早 Deadline 保留，Gateway 再施加 RPC 超时，客户端拦截器继续传递。HTTP 服务限制读写时间并支持五秒优雅关闭。不自动重试注册。当前采用本地明文 HTTP/gRPC，公网需后续 TLS/反向代理配置。

测试通过 httptest → Gin → 真实内存 gRPC → 模拟 User 服务验证协议转换、ID 精度、Request ID、超时与取消、错误映射和输入限制；未启动真实 MySQL，也未将登录 JWT 业务串联进行端到端实测。
