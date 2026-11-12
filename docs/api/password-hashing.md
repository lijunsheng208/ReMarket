# 密码哈希组件

本次仅实现 `backend/pkg/auth/password.go`，未接入注册、登录、JWT、数据库或 Gateway。

## 使用

```go
hash, err := auth.HashPassword(password)
// 检查 err 后将 hash 写入 users.password_hash，不保存原文。
err = auth.VerifyPassword(hash, candidatePassword)
```

使用 `golang.org/x/crypto/bcrypt`，成本为 bcrypt.DefaultCost（10），随机盐和成本编码在哈希中。输入必须是合法 UTF-8、至少 8 个 Unicode 字符且不超过 72 字节。当前不要求大小写及特殊字符组合，不裁剪空白、不做 Unicode 规范化。策略变更时需同时考虑已有账号的登录兼容性。

错误通过 `errors.Is` 区分：

| 错误 | 含义 |
| --- | --- |
| ErrInvalidPassword | 输入违反当前长度或编码规则 |
| ErrPasswordMismatch | 密码与哈希不匹配 |
| ErrInvalidPasswordHash | 存储的哈希格式损坏或不支持 |

登录业务接入时，将输入无效及不匹配统一映射为登录失败；损坏哈希按内部故障处理，不向外返回具体哈希。组件不记录密码或哈希，也不依赖 gRPC、GORM。哈希计算为同步 CPU 操作，当前不支持中途取消；限流在后续接入层实现。

测试覆盖正确/错误密码、随机盐、默认成本、中文与字节边界、无效 UTF-8、超长密码拒绝、空格保留及损坏哈希。

参考：[Go bcrypt 官方文档](https://pkg.go.dev/golang.org/x/crypto/bcrypt)。
