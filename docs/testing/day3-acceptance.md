# Day 3 联调、测试与文档验收

日期：2026-11-14（Asia/Shanghai）。本轮按真实环境重新执行，不以历史容器状态作为当前证据。测试不投递业务库积压、不更改运行中业务商品；临时 MySQL 库、随机 Redis Key、唯一 Kafka Topic/Group 在结束时清理。隔离故障卷保留。

## 实际验收矩阵

| 范围 | 本轮结果 | 限制 |
| --- | --- | --- |
| 商品缓存 | 真实 SQL 计数证明正负命中零商品读取；跨实例共享、更新/下架/创建自动失效、同快照、正负旧读回填保护通过 | 非生产负载性能结论 |
| 缓存故障 | 隔离 Redis 实际停机→查询回源→写入保留失效任务→带AOF重启→失效恢复→读取新值通过 | 非主从切换、非任意断电持久性保证 |
| 缓存 Worker | 独立子进程领取后/ACK后强杀、租约过期接管与旧令牌隔离通过 | 租约时间由测试 SQL 推进 |
| Gateway 限流 | 真实 Redis 40并发共享5令牌、两个 HTTP Server 共享额度、429/Retry-After、防伪IP与身份、写503/读取有界降级通过 | 两个Server非两个部署进程，非压测 |
| Producer/Consumer | 真实 ACK、处理失败不提交、同组重建重投、成功提交后恢复正确通过 | 未测多成员 rebalance 故障矩阵 |
| Outbox | Product/Order 独立临时库、并发认领、租约接管、失败重试、ACK后数据库未更新时重投通过 | 不保证同聚合业务版本到达顺序 |
| 发布到消费串联 | 两种聚合分别从独立发布库→Kafka→独立消费库；真实ACK后数据库完成失败，恢复产生重复消息，业务效果仅一次通过 | 组件串联，非完整HTTP下单链路 |
| Broker 中断 | 隔离 Kafka 实际停机，事件不标记成功；恢复后发布、ACK窗口重投和消费去重通过 | 单节点停启，非多副本故障 |
| 幂等/Retry/DLT | 12并发event_id去重、旧版本保护、同ID内容冲突、永久失败、有限重试耗尽、显式重放及重复重放通过 | 默认处理器仅维护测试视图 |
| 转移失败 | Retry与DLT未确认均不提交原Offset，同组重建重发ACK后才推进；无Key损坏消息可隔离 | Sender错误注入，非每个网络故障点强杀 |
| 契约 | 6个Product与9个Order示例可由实际Envelope解码，Packet示例可解码，Schema JSON语法有效 | 未宣称通过完整Draft2020-12校验器 |

相关全量真实集成 race 包：eventtest、product/service/cache/repository/handler、gateway、redis、ratelimit、kafka、outbox。各模块历史“待组合验证”的范围，以本表本轮结果为准；未覆盖的生产故障仍保留。

## 常规复验

先确保本地 MySQL、Redis、Kafka 及 Topic 初始化可用。加载已有 .env，不覆盖文件，不打印凭据。测试管理账号只用于临时库，实际服务继续使用独立账号：

```sh
set -a
. ./.env
set +a
export REMARKET_TEST_MYSQL_DSN="root:${MYSQL_ROOT_PASSWORD}@tcp(127.0.0.1:${MYSQL_PORT:-3306})/?parseTime=true&loc=UTC"
make infra-config
make day3-test
cd backend
go test ./...
```

`day3-test` 显式开启 Redis/Kafka 集成，缺少测试 DSN 即失败，不把全部集成跳过视为验收。单独运行 Go 默认测试时，未配置的集成会明确跳过。

## 真实停机复验

隔离 Compose 固定项目 remarket-day3-test，Redis 16380、Kafka 19092；网络和卷均与正常 remarket 分离，不依赖 Elasticsearch/MinIO，不删除原 RabbitMQ 容器/卷。默认端口被占用时先处理冲突，不对其他项目强制停机。故障测试只接受这两个固定端口，不能改为业务端口。

```sh
make day3-test-config
make day3-test-up
# 已按上文加载环境及测试DSN后：
export KAFKA_BROKERS=127.0.0.1:19092
export REDIS_ADDR=127.0.0.1:16380
cd backend
KAFKA_INTEGRATION=1 DAY3_BROKER_FAULT=1 go test -race ./internal/eventtest -run '^TestOutboxToConsumerAcceptance$' -count=1 -v
REDIS_INTEGRATION=1 DAY3_REDIS_FAULT=1 go test -race ./internal/product/service -run '^TestRedisServerOutageAcceptance$' -count=1 -v
cd ..
make day3-test-down
```

两个故障测试串行运行，不与其他依赖同一隔离服务的测试并行。测试失败也会尝试恢复服务，结束后执行 down 清理隔离容器/网络，保留数据卷。复验完成须取消临时端点环境覆盖，避免随后启动业务进程连接错误实例。

## 运行与未完成项

本轮没有重启业务服务、开启 Outbox/限流开关、部署专用消费者数据库或消费业务事件。详情缓存此前的本机启用记录见缓存文档，不能将此次测试库验收视为重新部署业务链路。

已更新独立故障 Compose、README、当前架构、事件信封与Retry/DLT契约、商品预占相关事件及Order全状态示例。Feed、搜索、聊天、生产监控/Trace、吞吐压测、Kafka多副本/Redis切主、生产TLS/ACL仍不在本次验收范围。单节点与本地开发配置不等同生产可用性承诺。

本轮最终检查：正常及隔离Compose配置校验通过；隔离端点完整Day 3真实集成race套件、全量Go测试、契约示例兼容及git diff --check通过。隔离Redis/Kafka容器与网络已通过day3-test-down清理，数据卷保留；未创建Git提交。
