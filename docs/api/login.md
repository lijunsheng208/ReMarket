# 登录业务与 Login RPC

本次接入 Login RPC → LoginService → UserRepository.GetByUsername → bcrypt 校验 → JWTManager 签发。未接入 Gateway、认证拦截器或当前用户接口。

用户名沿用注册规则，去首尾空白并转小写；密码保持原样。不存在用户、非法凭据、密码不匹配或非 ACTIVE 账号统一返回 Unauthenticated：`用户名或密码错误，或账号不可用`。数据库故障及损坏哈希作为内部错误，经通用拦截器返回 Internal，不向外暴露细节。

不存在用户时使用构造阶段生成的占位哈希执行校验，避免完全省略 bcrypt，但不承诺严格恒定响应时间。错误密码与禁用账号不签发 Token；角色和 ID 来自查询结果，不接受客户端指定身份。

成功返回 access_token、expires_at 和公开 User 字段，不返回密码哈希。Token 时间与 JWT exp 一致。服务在查询前、密码计算后及返回前检查 Context；bcrypt 无法中途取消。

User 入口现在同时要求 MySQL 和 JWT 配置，缺失或无效 JWT_SECRET 等配置会导致启动失败，不会使用默认密钥。环境变量见 `.env.example`，进程只读取已导出的变量。JWT_SECRET 应使用至少 32 字节随机值。数据库表仍需提前迁移，不自动建表。

测试通过真实 gRPC 协议、bcrypt、JWT 和模拟 Repository 验证正常登录、规范化、Token 验签及过期时间、统一失败、内部故障、取消和空请求。本次未完成真实 MySQL 到 Login RPC 的端到端实测。登录限流、刷新、撤销与身份验证中间件留至后续模块。
