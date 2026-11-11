# ReMarket

Go 微服务二手交易平台，使用 Monorepo 管理前后端、接口契约及部署配置。

## 目录

- `frontend/`：React + Vite + TypeScript 前端。
- `backend/cmd/`：Gateway、User、Product、Order、Search、Feed、Chat 和 Worker 入口。
- `backend/internal/`：各服务内部实现。
- `backend/pkg/`：公共技术组件，不存放共享业务模型。
- `backend/gen/`：生成的接口代码。
- `api/proto/`：版本化 gRPC 接口定义。
- `api/events/`：异步事件契约。
- `migrations/`：按服务划分的数据库迁移。
- `deployments/`：部署与监控配置。
- `tests/`：集成、并发与压测。
- `docs/`：架构、数据库及接口文档。
- `scripts/`：工程辅助脚本。

## 本地开发

安装 Go 和 Node.js 后执行：

```sh
make install
make dev
```

`make build` 构建工程，`make check` 执行检查，`make fmt` 格式化 Go 代码。

当前已建立工程骨架及本地基础设施配置，后端业务服务和接口尚未实现。
Go Module 暂定为 `remarket/backend`，确定远程仓库地址后可统一调整。

## 本地基础设施

首次复制 `.env.example` 为 `.env`（已有文件不要覆盖），执行 `make infra-config` 校验，`make infra-up` 启动五个组件并等待健康，`make infra-ps` 查看状态。

`make infra-down` 停止容器并保留数据卷，`make infra-logs` 查看日志。详见 [基础设施文档](docs/architecture/local-infrastructure.md)。
