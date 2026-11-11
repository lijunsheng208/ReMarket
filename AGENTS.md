# ReMarket 开发约定

## 项目与范围

本仓库是 React + TypeScript 前端与 Go 微服务后端的 Monorepo。架构和进度见 `系统开发文档.md`，数据库设计见 `docs/database/day1-data-model.md`，gRPC 基础见 `docs/api/grpc.md`。

按用户当前指定范围推进。Order、Feed 前置 Proto 契约及商品预占 RPC 已明确延后，不主动提前实现。文档区分设计完成、代码完成和实际验收通过，不能将脚手架或 SQL 文件存在视为业务已实现。

## 中文注释规范

- 新增或修改的手写函数、方法，在定义前添加中文注释，说明职责；必要时补充参数含义、返回结果、前提和边界。
- 导出 Go 函数注释以函数名开头，后接中文说明；测试函数也说明测试目标。
- 在关键流程添加中文注释，重点解释原因和约束：事务边界、状态转换、幂等、锁与并发、超时取消、错误映射、认证凭据传递和资源释放。
- 简单赋值、显而易见的语句无需逐行注释。注释与实现同步，不声称尚未实现的能力。
- `backend/gen/` 为生成代码，不手工添加或修改注释；需要调整协议说明时修改 Proto 源文件后重新生成。

## 架构与数据约束

- 服务内依赖方向为 Handler → Service → Repository，业务规则不依赖 Gin 或 gRPC 具体实现。
- 服务只拥有自身业务数据，跨服务通过 RPC 或事件访问；禁止跨库外键和直接操作其他服务的业务表。
- 公共包存放技术基础能力，不共享 GORM 业务模型。
- MySQL 为业务事实来源，Redis、ES 为缓存或可重建视图。
- 金额使用 `price_cent BIGINT`，单位为分；ID 为实体内唯一的 BIGINT 自增，JSON 中注意大整数精度。
- 商品状态转换经过 Service；商品变更与 Outbox 同本地事务提交，消费者仍需幂等与版本校验。
- 使用版本化 SQL，不以 AutoMigrate 代替迁移记录，不自动执行会删除现有数据的操作。

## gRPC 与安全上下文

- 从上游 Context 派生超时，继续传给数据库和下游 RPC，不延长上游 Deadline。
- Request ID 通过 metadata 跨进程传递；Token 传递不等于已验证身份，不信任任意 x-user-id/x-role。
- 未知错误统一对外返回 Internal，不暴露内部 SQL、凭据或敏感请求内容。
- 业务方法尚未实现时明确返回 Unimplemented，不伪造成功。
- 本地明文连接及示例密码仅用于开发；认证和 TLS 能力按实际实现准确说明。

## 目录与常用验证

- `api/proto/`：协议源文件；`make proto` 生成 `backend/gen/`。
- `backend/cmd/`：服务入口；`backend/internal/`：业务实现；`backend/pkg/`：技术组件。
- `migrations/`：按服务划分 SQL；`deployments/`：基础设施配置；`docs/`：设计与操作说明。
- Go 代码修改后执行 gofmt，并按改动运行 `cd backend && go test ./...`；涉及并发时运行相关 race 测试。
- 前端修改按需执行 lint 和 build；Compose 修改执行 `make infra-config`。
- 提交前检查 `git diff --check`，保留用户已有修改，不自动创建提交。
- 同步文档时记录真实验证结果及待办，不将历史容器状态写成重新核验的当前状态。
