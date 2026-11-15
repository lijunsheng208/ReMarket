# Feed 阶段 3：Kafka 与 Redis 推拉结合

## 实现范围

普通卖家：商品事件 → Feed Consumer → 批量流水线写粉丝 `feed:user:{id}` 收件箱，同时维护作者流。热门卖家：只维护 `feed:author:{id}`，读取时合并作者流，不逐粉丝写入。粉丝数大于等于配置阈值时按热门卖家处理，每次商品事件重新判断。默认阈值 1000，允许配置 1～10000。

首页继续使用 Product MySQL 查询；混合时间线用于关注流，不改变前端接口、排序或游标格式。

## 数据与一致性

- Feed Consumer 使用独立消费组 `remarket-feed-v1`，消费现有商品主 Topic。普通卖家先通过 User 内部 RPC 分页取得粉丝 ID；热门卖家只取第一页及总数。没有跨库读写。
- User 的 `ListFeedFollowers` 仅接受独立 `FEED_USER_SERVICE_TOKEN`，不接受普通 JWT，也不对 Gateway 暴露。至少 32 字节，不得复用 JWT 密钥。User 与 Feed Consumer 都需要配置。
- 每个 ZSet 以固定宽度的 `(created_at 微秒, product_id)` 字典序成员、分数 0 排序，避免 Redis double 对 BIGINT 和组合排序造成精度损失。
- Lua 原子比较商品版本、更新成员、裁剪长度和设置 TTL。版本按十进制字符串长度及字典序比较，不转换为 Lua number。相同版本允许重复执行，修复部分批次完成后重试；旧版本不修改时间线。每批最多 200 个目标键，默认每条时间线上限 1000，TTL 一小时。版本保护键保留七天。
- 所有商品视图事件携带可选 `created_at`；Schema、示例与生产代码同步。旧事件缺少该字段时从 Product 公开详情补齐；非公开商品不恢复展示。旧非公开快照无法取得创建时间时仅作版本保护，残留候选由读取过滤。
- 读取重新获取当前关注集合，指纹包含关注关系主键：取消后重新关注也会触发历史窗口回填。Product 对候选执行当前卖家集合、在售状态和游标条件过滤，缓存不能授予访问权限。
- 缓存首次缺失、指纹变化、热门作者流缺失、候选不足、深页越过完整窗口或 Redis 故障时，回退 Product MySQL。第一页回退成功后回填至多 100 条权威记录；窗口标记保留 30 秒，定期与 MySQL 对齐。正常缓存读取最多合并 32 个热门作者源、2000 个候选，边界不足时继续回退，不能误报已无下一页。
- Redis 探测及回填各有 200ms 子超时，完整编排仍受上游截止时间及 Feed 三秒上限约束；Redis 不可用不阻止查询进程启动。
- 商品事件是最终一致的：Kafka 消费延迟可能暂时遗漏新商品；窗口过期回填可恢复。状态隐藏和取消关注在下一次读取经过 MySQL/当前关注关系过滤。跨请求分页不提供分布式快照。

## 消费失败与恢复

合法事件写索引或 RPC 失败时不提交 Offset，消费者退出当前实例、一秒后重建并重新处理；Redis 写入完成而提交失败也会重投。处理串行、逐条提交，不越过失败消息。普通卖家的全部分批写入完成后才返回成功。

非法信封或载荷先发送到独立 `remarket.feed.product.dlt.v1`，收到 Broker ACK 才提交原 Offset。毒消息不会阻塞主流；临时故障不会自动移入 DLT。DLT 保留原 Kafka 消息位置、Key 和 Value，不打印业务载荷。当前没有自动 DLT 重放命令；修复原因后由操作员按原 Key 重发合法事件。

Redis 数据丢失后，关注流可从 MySQL 回退并按需回填。消费者继续维护后续增量，作者/收件箱视图都是可丢弃数据。深页始终允许 MySQL 回退；此阶段没有离线全量时间线重建任务，不能将按需窗口回填当作全量 Redis 重建完成。

## 启动

1. 在本地配置中设置 `FEED_REDIS_ENABLED=true`，设置 `FEED_HOT_SELLER_THRESHOLD` 和 `FEED_TIMELINE_LIMIT`（100～10000）。配置 Redis 及 Kafka 现有连接变量。
2. 为 User 与 Feed Consumer 配置相同的独立 `FEED_USER_SERVICE_TOKEN`。不要将凭据提交到仓库。
3. 执行 `make kafka-topics` 补建 Feed DLT；该脚本不删除已有 Topic 或消息。
4. 重启 User、Product、Feed、Gateway，再运行 `make run-feed-consumer`。Product 更新后才会在新事件中写入创建时间，历史事件仍兼容。
5. 关闭 `FEED_REDIS_ENABLED` 可恢复基础 MySQL 查询路径。Consumer 在开关关闭时拒绝启动。

本次未更改用户 `.env`，未启动常驻消费者，也未操作现有消费组 Offset。验证仅使用唯一临时库、Topic、Group 和 Redis 前缀，测试结束清理。

## 验证入口

- `go test ./...`：普通回归；未配置环境的外部集成测试会跳过。
- 配置 `FEED_INTEGRATION=1`：启用真实 Redis 时间线测试。
- 配置 `KAFKA_INTEGRATION=1`：启用真实 Kafka 重投测试。
- 配置 `REMARKET_TEST_MYSQL_DSN` 和 `FEED_INTEGRATION=1`：启用真实 MySQL + Redis HTTP→三服务查询测试。
- `go test -race ./internal/feed/... ./internal/user/handler ./internal/gateway -run 'TestFeed|TestConsumer|TestRedisTimeline|TestKafkaFeed'`：相关并发验证。

集成测试不替代浏览器手动联调和高粉丝量压测；当前未声称完成这两项。

## 本次验证结果（2026-11-15）

配置真实 MySQL、Redis、Kafka 后，全量 `go test ./...` 通过；相关 race 检查通过。验证包括普通/热门分流、唯一临时 Topic 上写 Redis 后未提交的重复投递、Redis 字符串版本防乱序及大整数精度、批量写入和长度裁剪、两源合并去重、关系指纹失效、缓存丢失检测、真实 HTTP 查询命中候选复核及游标/取消关注过滤。Compose 配置检查和 `git diff --check` 通过。
