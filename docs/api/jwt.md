# JWT 组件

本次仅实现 `backend/pkg/auth/jwt.go`、`jwt_config.go` 及测试；未接入登录、Gateway、认证拦截器或可信身份 Context。

## 配置与调用

`JWTConfigFromEnv` 读取已导出的 JWT_SECRET、JWT_ISSUER、JWT_AUDIENCE、JWT_ACCESS_TTL，不自动读取 .env。密钥至少 32 字节且应使用密码学随机值；无默认值，不能将开发示例当生产密钥。issuer 和 audience 必填，有效期范围一秒至 24 小时。示例时长为 1h；算法固定 HS256，不接受环境或 Token 动态选择。

```go
config, err := auth.JWTConfigFromEnv()
// 检查 err 后创建组件。
manager, err := auth.NewJWTManager(config)
token, expiresAt, err := manager.IssueAccessToken(user.ID, user.Role)
claims, err := manager.VerifyAccessToken(token)
```

实际调用需逐次检查错误；签发只允许正数用户 ID 及 USER/ADMIN。调用方必须先验证登录密码、账号状态，并从数据库获得角色，不能使用客户端提交的角色。

令牌包含 sub（十进制字符串 ID）、role、iss、aud、iat、nbf、exp。时间精度为秒，返回过期时间与令牌一致。固定 HS256 算法白名单并校验签名，强制 exp、iat、nbf；校验发行方、受众、身份及时间，零时钟宽限，到达 exp 即无效。验签失败统一返回中文 ErrInvalidToken，禁止使用失败返回的声明。

JWT 提供签名而非加密，不存密码、哈希或敏感信息。HS256 验签方持有同一密钥，也具有签发能力；当前不支持密钥轮换、refresh token、退出撤销或账号禁用即时失效。验签通过不等于账号当前可用，后续业务仍需处理状态和角色更新。

测试使用固定时钟，无需 sleep，覆盖正常签发、过期边界、内容篡改、错误密钥、none/HS384、错误 issuer/audience、缺失时间、未来时间、非法身份及配置。

实现依据：[golang-jwt 官方验证选项](https://golang-jwt.github.io/jwt/usage/parse/)。
