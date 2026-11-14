# 商品缓存计划 6 验收

日期：2026-11-14（Asia/Shanghai）。实现见 `backend/internal/product/service/cache_integration_test.go`。测试真实连接 MySQL 与 Redis，单独创建 `remarket_cache_test_*` 临时库，执行 Product 000001～000004 迁移，使用高位随机商品 ID 隔离 Redis Key，退出精确删除本次 Key 并删除本次临时库。不对业务商品、共享 Redis 服务执行停机或 FLUSHDB。

## 实际通过项目

| 项目 | 证据 |
| --- | --- |
| 正负命中不回源 | GORM Query 回调计数真实商品读取 SQL，首次加载后再次读取计数不增加 |
| 多实例共享缓存 | 两个独立 QueryService 实例使用真实 Redis，第二实例命中不增加 SQL；不声称跨实例 Singleflight |
| 更新、下架、创建失效 | 真实 Service→Repository 同事务任务→Runner→Redis；新值可见、匿名不能查看已下架商品、卖家可见、新建清除此前空值 |
| 旧读晚回填 | 屏障停在真实 MySQL 旧快照加载之后，另一实例修改并失效/回填后释放旧读，Redis 新值未被覆盖 |
| 不存在与创建竞争 | 旧不存在查询停在回填前，创建/发布并填入新值后释放旧读，空值不能覆盖新商品 |
| 同快照商品与图片 | 商品 SELECT 完成后暂停，另一事务同时修改商品和图片，原事务图片仍来自旧快照 |
| 全写入口任务矩阵 | 草稿创建、草稿编辑、发布、下架、下架编辑、重新上架、预占、释放、再次预占、成交；任务数与商品版本一致，预占/释放/成交重试不重复建任务 |
| 任务失败事务回滚 | 临时库触发器拒绝失效任务插入，在售商品编辑及业务 Outbox 一同回滚；计划 5 另有草稿回滚验证 |
| Redis 客户端断连 | 真正关闭独立连接池后详情回源、写入成功且失效任务留待重试；恢复有效连接后处理任务 |
| 独立 Worker 崩溃 | 使用同一测试可执行文件启动子进程，在租约领取后或 Redis ACK 后发送就绪信号；父进程强杀，再启动独立恢复进程，最终任务 DONE 且读取新值 |
| 租约恢复 | 强杀后通过 SQL 将测试租约推进到过期，不依赖 sleep；另有 Repository 旧令牌拒绝测试 |
| Lua/TTL/并发保护 | 真实 Redis 组件 race 回归覆盖正负 TTL、NX、代次丢失与重建、损坏缓存条件清理、并发回填 |

相关真实 race 回归通过：Product Service、Cache、Repository、Handler、Gateway。全量 Go 测试通过；未配置集成环境变量的测试明确跳过，不将跳过视为通过真实验收。

## 复验

从仓库根目录加载本机 .env，提供可创建/删除临时库的测试管理账号。避免将密码直接写进命令历史或打印配置。下例从现有本地环境读取凭据：

```sh
set -a
. ./.env
set +a
export REMARKET_TEST_MYSQL_DSN="root:${MYSQL_ROOT_PASSWORD}@tcp(127.0.0.1:${MYSQL_PORT:-3306})/?parseTime=true&loc=UTC"
export REDIS_INTEGRATION=1
cd backend
go test -race ./internal/product/service ./internal/product/cache ./internal/product/repository ./internal/product/handler ./internal/gateway -count=1
go test ./...
```

测试子进程仅用于失效 Worker 领取/ACK/恢复，不绑定业务 RPC 端口。商品查询多实例测试为同进程独立服务实例；真实独立服务的初步 gRPC 启用验证记录见缓存设计，不能将两者合称为多进程完整 gRPC 故障矩阵。

## 结论与剩余边界

本地开发环境的缓存命中、业务自动失效、持久化重试、旧值/空值并发保护及独立 Worker 强杀恢复已通过上述验证，可保持当前详情缓存启用。缓存仍为最终一致性，提交到失效之间有窗口，故障时依赖任务重试与绝对 TTL；不保证即时权限撤销。

本轮未停止/重启真实 Redis 服务，断连测试不等同于 Redis 服务端重启、持久化恢复、主从切换或网络半开；未验证 Redis Cluster、生产吞吐/延迟目标、容量上限压测，也未逐个业务写入口注入任务写失败。SQL 计数只统计商品读取，不代表整条调用链零 SQL（管理员核实可能访问 User 数据库）。这些是明确保留的部署/生产验证项，不宣称“企业级故障全部覆盖”。

## Day 3 联调补充

后续联调已在独立 Compose 项目真实停启 Redis，验证查询降级、失效任务持久化及AOF重启后失效恢复。上文“未停止真实Redis”是原计划6的历史边界；本轮新增结果、复验与仍未覆盖的切主范围见 [Day 3验收](../testing/day3-acceptance.md)。
