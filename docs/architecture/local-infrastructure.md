# 本地基础设施

从仓库根目录操作。首次使用复制 `.env.example` 为 `.env`，已有文件不要覆盖。

```sh
make infra-config
make infra-up
make infra-ps
```

`infra-up` 等待五个组件健康，超时 240 秒。`make infra-logs` 查看日志，`make infra-down` 停止容器并保留命名卷。

| 服务 | 默认本机端口 | 容器内地址 | 数据卷 |
| --- | --- | --- | --- |
| MySQL 8.4 | 3306 | mysql:3306 | mysql_data |
| Redis 7.4 | 6379 | redis:6379 | redis_data |
| Kafka 4.1.2（KRaft） | 9092 | kafka:19092 | kafka_data |
| Elasticsearch 8.19.25 | 9200 | elasticsearch:9200 | elasticsearch_data |
| MinIO | API 9000、控制台 9001 | minio:9000 | minio_data |

端口和开发密码见 `.env`。所有发布端口仅绑定本机；Elasticsearch 为单节点且关闭认证。这是本地开发配置。Redis 开启 AOF，日志轮转限制为每个容器最多三个 10 MiB 文件。ES 默认堆 512 MiB、容器内存上限 2 GiB，Docker 建议至少分配 6 GiB 内存。

本机验证时发现其他项目占用 6379 和 9200，因此本地 `.env` 分别改为 16379 和 19200；示例文件仍保留标准端口。未修改其他项目容器。

MySQL 首次空卷启动时执行 `deployments/mysql/init/01-databases.sh`，创建 `remarket_user`、`remarket_product`、`remarket_order`、`remarket_chat` 四个库和同名账号，各账号仅拥有自身库的 SELECT、INSERT、UPDATE、DELETE 权限。密码变量分别为 `MYSQL_USER_PASSWORD` 等。业务表尚未创建，后续通过 `migrations/` 执行版本化迁移；迁移需管理账号或专用迁移账号。

已有数据卷时更改初始化脚本或 `.env` 不会自动修改 MySQL 账号密码。修改已有账号需显式执行变更。不要删除已有数据卷来代替迁移。Worker 不配置跨库通用账号。

卷名带项目名前缀，默认 `remarket`。停止服务不会删除卷。当前创建 Kafka 事件 Topic，不创建 ES 索引或 MinIO bucket，这些在业务契约明确后实现。

MinIO 复用本机已有的官方 `RELEASE.2024-12-18T13-15-44Z` 开发镜像：尝试下载较新版本时 Docker Hub 加速器返回 403、Quay 返回 401。后续升级需显式验证。

参考：[MySQL 初始化机制](https://hub.docker.com/_/mysql)、[Elasticsearch Docker 部署](https://www.elastic.co/guide/en/elasticsearch/reference/8.19/docker.html)。

## Day 3 Kafka

`make kafka-up` 仅启动 Kafka、等待协议健康检查并运行 Topic 初始化；`make kafka-topics` 可重复补建缺失 Topic。完整 `make infra-up` 也包含一次性 kafka-init 服务。旧 RabbitMQ 容器不会被自动删除，旧数据卷保留；确认不再使用后再单独处理，不使用 `down -v`。

宿主机使用 `KAFKA_BROKERS=127.0.0.1:9092`（改端口时同步修改 KAFKA_PORT 和 KAFKA_BROKERS），同 Compose 网络内使用 `kafka:19092`。控制器端口不发布。固定 Cluster ID 与数据卷绑定，已有数据不要随意修改。此配置为单节点明文开发环境，无 TLS/SASL，不提供多副本容灾。

初始化商品与订单的 `remarket.{product|order}.events.v1`、`events.retry.v1`、`events.dlt.v1`，各 3 分区、1 副本；普通及 Retry 保留 7 天，DLT 保留 30 天。Key 后续分别使用 product_id/order_id。环境变量只影响新建 Topic，脚本不会覆盖已有 Topic 配置或分区，变更需显式管理。

`make kafka-check` 加载根目录 .env，运行 franz-go 协议 Ping，失败返回非零状态；支持 KAFKA_BROKERS、KAFKA_CLIENT_ID、KAFKA_CONNECT_TIMEOUT。客户端保留默认幂等生产能力并设置 acks=all，本阶段尚未实现 Producer/Consumer 业务封装、Outbox 投递及 Retry/DLT 流转。

参考：[Apache Kafka Docker](https://kafka.apache.org/41/getting-started/docker/)、[franz-go 客户端](https://pkg.go.dev/github.com/twmb/franz-go/pkg/kgo)。

## Redis Go 客户端（商品缓存计划 2）

`make redis-check` 加载根目录 `.env` 并只执行 PING，不写业务 Key。`backend/pkg/redis.Open` 校验配置、派生启动超时、探测失败关闭连接池；成功后由调用方 Close。尚未组装进 Product，也未启用缓存或限流。

配置：`REDIS_ADDR` 优先，否则使用 `127.0.0.1:${REDIS_PORT:-6379}`；容器内显式设置 `redis:6379`。`REDIS_USERNAME` 可为空，密码来自现有 `REDIS_PASSWORD`；`REDIS_DB` 默认 0，不假定服务端 DB 数量。当前仅单节点 TCP 明文配置，不含 TLS、Sentinel 或 Cluster。

连接池 `REDIS_POOL_SIZE=10` 同时作为 MaxActiveConns 硬上限；`REDIS_MIN_IDLE_CONNS=0`。启动探测 `REDIS_CONNECT_TIMEOUT=2s`，拨号 `REDIS_DIAL_TIMEOUT=1s`，读、写、池等待分别为 `REDIS_READ_TIMEOUT/REDIS_WRITE_TIMEOUT/REDIS_POOL_TIMEOUT=200ms`。这些为开发初始值，需结合延迟调整。开启 ContextTimeoutEnabled，不延长上游 Deadline；禁用命令自动重试，拨号仅一次尝试。读写超时也约束驱动内部操作，调用者仍需传入有限的 Context，不把这些超时之和当作整个请求预算。

单测：`cd backend && go test ./pkg/redis`。真实只读集成及 race：从根目录加载 `.env` 后，在 backend 下执行 `REDIS_INTEGRATION=1 go test -race ./pkg/redis -v`；未设置该变量时明确跳过真实测试。检查失败不会自动启动基础设施。后续缓存将自行决定 Redis 故障降级，Open 本身不会返回一个伪装健康的客户端。

实现参数依据 [go-redis Options](https://github.com/redis/go-redis/blob/v9.17.2/options.go)；连接池与超时背景见 [Redis 生产使用说明](https://redis.io/docs/latest/develop/clients/go/produsage/)。
