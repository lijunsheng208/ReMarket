# User 模型与 Repository

本次只实现 MySQL 连接、User 模型及创建和查询，不接入注册规则、JWT、Gateway 或 gRPC 业务方法，不执行业务库迁移。

## 文件及职责

| 文件 | 职责 |
| --- | --- |
| backend/pkg/database/mysql.go | DSN、UTC 会话、连接池、带 Context 的 Ping；失败时关闭连接池 |
| backend/internal/user/model/user.go | users 表映射，PasswordHash 禁止 JSON 序列化 |
| backend/internal/user/repository/user.go | Create、GetByUsername、GetByID；唯一冲突及未找到错误转换 |
| backend/internal/user/repository/user_test.go | 使用独立临时库验证 MySQL 行为 |

OpenMySQL 接收显式 MySQLConfig，不自动加载环境变量；返回 GORM 实例和底层 sql.DB，由组装层负责关闭。默认最大连接数 10、空闲连接数 5、连接最大寿命三分钟、连接超时五秒、读写超时五秒；Ping 使用调用方 Context。配置为空或参数为负时拒绝连接。生产 TLS 配置不在当前本地连接组件范围内。

GORM 开启 TranslateError、关闭 SQL 日志，避免创建用户时将密码哈希输出。没有 AutoMigrate。实际建表使用已定义的版本化 SQL。

CreateUserParams 不允许指定 ID、角色或账号状态，只接收用户名、密码哈希和昵称；创建固定为 USER/ACTIVE。Repository 不生成密码哈希、不规范化用户名，不承担注册校验；后续 Service 必须执行这些规则。

用户名冲突由数据库唯一约束保证，不通过先查询来防重。当前创建不传入主键且 users 只有 username 业务唯一键，因此重复键转换为 ErrUsernameExists。查询使用参数绑定，按 ID 显式 Where，避免零 ID 意外查询首条记录。

错误使用 errors.Is 判断 ErrUserNotFound、ErrUsernameExists，两者属于 apperror，现有 gRPC 错误映射可直接处理。内部错误保留根因；取消和超时仍可用 errors.Is 判断。对外必须经过统一错误映射，不直接输出原始数据库错误。

## 测试

常规 `go test ./...` 在未配置 REMARKET_TEST_MYSQL_DSN 时跳过真实数据库测试。提供该变量时执行 `go test -race ./internal/user/repository -v`。

测试 DSN 应连接专用开发 MySQL，账号需具备创建和删除测试库权限；不要使用生产实例。测试自动创建 remarket_user_test_ 前缀的唯一临时库，应用 User SQL 后验证读写、默认值、大小写唯一规则、并发冲突、取消、超时及连接故障，结束后清理该临时库。业务服务仍应使用自身库的最小权限账号，不能沿用测试管理账号。
