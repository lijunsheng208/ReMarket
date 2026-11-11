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
| RabbitMQ 4.1 | 5672、管理台 15672 | rabbitmq:5672 | rabbitmq_data |
| Elasticsearch 8.19.25 | 9200 | elasticsearch:9200 | elasticsearch_data |
| MinIO | API 9000、控制台 9001 | minio:9000 | minio_data |

端口和开发密码见 `.env`。所有发布端口仅绑定本机；Elasticsearch 为单节点且关闭认证。这是本地开发配置。Redis 开启 AOF，日志轮转限制为每个容器最多三个 10 MiB 文件。ES 默认堆 512 MiB、容器内存上限 2 GiB，Docker 建议至少分配 6 GiB 内存。

本机验证时发现其他项目占用 6379 和 9200，因此本地 `.env` 分别改为 16379 和 19200；示例文件仍保留标准端口。未修改其他项目容器。

MySQL 首次空卷启动时执行 `deployments/mysql/init/01-databases.sh`，创建 `remarket_user`、`remarket_product`、`remarket_order`、`remarket_chat` 四个库和同名账号，各账号仅拥有自身库的 SELECT、INSERT、UPDATE、DELETE 权限。密码变量分别为 `MYSQL_USER_PASSWORD` 等。业务表尚未创建，后续通过 `migrations/` 执行版本化迁移；迁移需管理账号或专用迁移账号。

已有数据卷时更改初始化脚本或 `.env` 不会自动修改 MySQL 账号密码；RabbitMQ 初始账号同样仅在新数据目录初始化。修改已有账号需显式执行变更。不要删除已有数据卷来代替迁移。Worker 不配置跨库通用账号。

卷名带项目名前缀，默认 `remarket`。停止服务不会删除卷。当前不创建业务队列、ES 索引或 MinIO bucket，这些在业务契约明确后实现。

MinIO 复用本机已有的官方 `RELEASE.2024-12-18T13-15-44Z` 开发镜像：尝试下载较新版本时 Docker Hub 加速器返回 403、Quay 返回 401。后续升级需显式验证。

参考：[MySQL 初始化机制](https://hub.docker.com/_/mysql)、[Elasticsearch Docker 部署](https://www.elastic.co/guide/en/elasticsearch/reference/8.19/docker.html)。
