# 注册业务与 Register RPC

调用链：Register RPC → handler.Server → service.Service → repository.UserRepository → MySQL。只有注册已接入，登录、JWT、当前用户和 Gateway 延后。

## 输入规则

- 用户名先去首尾空白并转小写，再校验 3 至 32 位，ASCII 字母开头，仅字母、数字、下划线。
- 昵称去首尾空白，要求合法 UTF-8、1 至 64 个 Unicode 字符，不含控制字符。
- 密码不裁剪、不规范化，沿用哈希组件：至少 8 个 Unicode 字符、最多 72 字节。
- 创建固定为 USER/ACTIVE，客户端不能指定权限或密码哈希。

Service 在哈希前及哈希后检查 Context。只将 bcrypt 哈希交给 Repository；并发用户名冲突由数据库唯一约束判定，不使用先查再写。Handler 显式构造公开 User 响应，无密码字段。

输入错误映射 InvalidArgument；用户名冲突映射 AlreadyExists；未知数据库故障经通用拦截器映射 Internal；取消和超时保留原状态。注册无自动重试及幂等键，响应丢失后重试可能收到 AlreadyExists。

## 启动

User 进程现在需要 MySQL，读取已导出的 USER_MYSQL_ADDR、USER_MYSQL_USERNAME、USER_MYSQL_DATABASE、MYSQL_USER_PASSWORD。示例见 `.env.example`，地址应与本地 MYSQL_PORT 一致。

先通过迁移工具或管理账号确认 remarket_user.users 已按 SQL 创建；本次不自动迁移业务库。然后导出配置并执行 `make run-user`。程序不会自动读取 `.env`。数据库初始化失败会退出；连接池由入口负责关闭。

当前开发 gRPC 为明文，请勿对公网开放，测试凭据不要用于生产。进程健康不等于所有业务方法可用。

验证：handler 中的 bufconn 测试使用真实 gRPC 协议和真实 Service/密码组件、模拟 Repository，覆盖规范化、哈希、非法输入不入库、唯一冲突及内部错误映射、取消、空请求和登录保持 Unimplemented。真实 MySQL Repository 测试是上一步的独立测试，本次未将注册 RPC 与 MySQL 组成端到端实测。
