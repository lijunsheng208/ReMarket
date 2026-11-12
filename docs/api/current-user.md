# User Service 鉴权与当前用户

新增 `GetCurrentUser(GetCurrentUserRequest) → UserResponse`，请求不接受 ID。原 GetUser、UpdateProfile 仍未实现，没有以当前用户查询替代任意用户查询的语义。

User 服务端拦截器顺序：通用处理 → JWT 鉴权 → Handler → CurrentUserService → Repository。默认保护所有一元 RPC，仅匿名放行 Register、Login 和标准 Health/Check。当前协议无业务流式 RPC，尚未实现流式鉴权。

受保护方法要求唯一 authorization metadata，格式 `Bearer <token>`；验证签名、算法、发行方、受众及有效期后，将 sub 与 role 放入私有类型的 Context key。忽略 x-user-id/x-role；未提供不经过验签的身份写入 API。令牌验证失败返回 Unauthenticated，业务不执行。

GetCurrentUser 从已验证 Context 获取 ID，再查询当前数据库。用户不存在返回 Unauthenticated，禁用返回 PermissionDenied，数据库故障返回 Internal；响应角色来自数据库而非 Token 的旧快照。不返回密码哈希。禁用检查目前落实在当前用户业务，不能据此声称所有未来受保护方法都已检查账号实时状态。

调用示例：向 User Service 的 GetCurrentUser 附加 `authorization: Bearer <Login 返回的 access_token>`。跨服务仍转发原 Token，在 User Service 独立验签，而非相信调用者写入的用户头。当前未接入 Gateway、Token 撤销或密码变更后的即时失效。

测试使用真实 gRPC 协议、JWT 及模拟 Repository，覆盖缺失/无效/重复凭据、错误认证方案、伪造身份头、匿名放行、默认保护、数据库最新角色、禁用及不存在用户和内部错误。JWT 组件自身测试覆盖过期与篡改。本次未执行真实 MySQL 端到端验证。
