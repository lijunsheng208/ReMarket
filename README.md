# ReMarket

React + TypeScript 前端与 Go 微服务后端的二手交易平台 Monorepo。

## 当前范围

User、Product、Order 与 Gateway 已有业务实现。Day 3 已接入商品详情缓存、Gateway Redis Lua 限流、Kafka Producer/Consumer、Outbox Publisher，以及独立测试消费者的幂等、Retry/DLT 和重放。Feed、搜索消费和聊天不在本轮范围内。

代码实现、测试验收和运行启用分别记录：详见 [Day 3 联调验收](docs/testing/day3-acceptance.md)。Outbox 和限流默认关闭；测试消费者需独立建库迁移，不随基础设施自动启动。

## 目录

- `frontend/`：React + Vite + TypeScript。
- `backend/cmd/`：User、Product、Order、Gateway、连接探测及 event-consumer 入口。
- `backend/internal/`：服务业务实现；`backend/pkg/`：技术组件。
- `api/proto/`、`backend/gen/`：协议源及生成代码。
- `api/events/`：主事件与 Retry/DLT 契约。
- `migrations/`：按服务版本化迁移；`deployments/`：基础设施。
- `docs/`：设计、运行及验收说明。

## 本地开发

首次复制 `.env.example` 为 `.env`，已有文件不要覆盖。安装项目要求的 Go、Node.js 与 Docker Compose，然后执行：

```sh
make infra-config
make infra-up
make infra-ps
make install
make dev
```

基础设施包含 MySQL、Redis、Kafka、Elasticsearch、MinIO 五个组件及一次性 Topic 初始化；不自动迁移业务表。已有卷修改初始化脚本不会重新建账号，不使用删除数据卷代替迁移。

运行 Go 服务前加载根目录环境，再按需启动 User、Product、Order、Gateway：

```sh
set -a
. ./.env
set +a
make run-user
# 其他终端分别运行 make run-product / run-order / run-gateway
```

`make check` 执行 Go 测试与前端 lint/build，`make build` 构建，`make fmt` 格式化。`make kafka-check` / `make redis-check` 加载 .env 后只读探测连接。`make infra-down` 保留数据卷。

## Day 3 操作与复验

| 模块 | 运行约束与说明 |
| --- | --- |
| 商品缓存 | 先执行 Product 000004，详情缓存与失效恢复必须同时启用；见 [缓存设计](docs/cache/product-detail.md) |
| Lua 限流 | GATEWAY_RATE_LIMIT_ENABLED=true 后重启 Gateway；[配额与故障策略](docs/api/rate-limit.md) |
| Kafka | `make kafka-up` 启动与初始化；[Producer/Consumer](docs/api/kafka.md) |
| Outbox | 各服务 *_OUTBOX_ENABLED 显式启用，仅访问自身库；[发布器说明](docs/api/outbox-publisher.md) |
| 测试消费者 | 专用库迁移后 `make run-event-consumer`；[Retry/DLT/重放](docs/api/event-consumer.md) |
| 契约 | [事件契约目录](api/events/README.md)，主消息使用 data，Retry/DLT 使用 Packet |

加载 Redis/Kafka 环境并设置可创建/清理临时库的 `REMARKET_TEST_MYSQL_DSN` 后，运行 `make day3-test`。测试使用独立临时资源，不消费业务库积压。完整命令和边界见 [联调验收](docs/testing/day3-acceptance.md)。

真实停机实验使用 `make day3-test-up` 创建独立项目（Redis 16380、Kafka 19092、独立网络和卷），结束后 `make day3-test-down` 保留隔离卷；不能在正常基础设施上直接做停机故障注入。
