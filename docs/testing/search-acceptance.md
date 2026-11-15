# Day 4 搜索真实环境验收（2026-11-14）

## 本次已验证

本次实际检查 Compose：MySQL、Kafka、Redis 正在运行；显式启动项目 Elasticsearch 后 healthy。确认项目 Elasticsearch 为 **8.19.25，127.0.0.1:19200**，显式初始化 products_v1 和 products_search，补建 Search 专用 retry/DLT。未删除已有容器或业务数据。

运行验收进程：Search 50054；Search Consumer 使用独立组 remarket-search-acceptance-20261114；启用 PRODUCT_OUTBOX_ENABLED 的 Product 15052；Gateway 18081（接入该 Product）及 18080；前端 15173 代理 18080。原有 50051/50052/50053、8080、5173 进程未替换。新增进程及项目 ES 保留供本次 review 使用，不代表标准端口旧进程已加载新代码。日常使用原有 5173/8080 时需重启更新后的 Gateway，并启用 Product Outbox、Search 和消费者。

通过 `backend/scripts/search_acceptance.py`，HTTP → Gateway → Product/MySQL/Outbox → Kafka → Search Consumer → Elasticsearch → Search gRPC → Gateway 搜索真实验收：

| 场景 | 实测结果 |
|---|---|
| 独立账号发布 13 个商品 | 45 秒预算内搜索可见，成功 |
| 每页 12、price_asc 翻页 | 共 13 条，ID 无重复，价格有序，终页无游标 |
| 分类 + 价格闭区间 10003～10005 分 | 精确返回 3 条 |
| 在售修改标题与价格 | 新搜索快照更新为 12345 分 |
| 下架 | 新搜索不再出现 |
| 重新上架 | 新搜索恢复可见 |
| 清理本轮商品 | 13 个自建商品均下架，搜索无结果；保留 MySQL/事件/ES 状态记录 |

成功测试批次标记 `srch14a5c9f2c5`。没有复用旧 PIT 观察状态变化，因为旧 PIT 按契约保留历史快照。

通过 `backend/scripts/search_order_acceptance.py`，另建独立买卖账号与商品，实测：订单 RESERVED → 搜索不可见；取消到 CANCELLED → 商品释放并恢复可见；第二次预占后买卖双方确认到 COMPLETED → Product SOLD、搜索不可见。成功批次 `state6ac80f3af9`，商品 17 保留 SOLD；无在线支付。第一次脚本错误地向空体接口发送 `{}`，被 400 拒绝，已修正为真正空体；该失败批次的预占记录保留，须按原 Order 过期任务正常推进，未手工改表，也未声称清理完成。

经前端同源代理 15173 实测：合法搜索和相关性排序 200；min>max 和非法游标 400。浏览器 CUA 宿主不可用，Python 环境也没有 Playwright；**页面点击、截图、响应式布局、输入竞争与过期提示的浏览器验收尚未通过**，不能将代理 HTTP 测试当成 UI 自动化通过。

## 本轮联调发现与修复

本机 9200 实际属于其他 OpenSearch 2.19.1。最初默认 URL 误连，创建了空 products_v1/读别名，消费者随后向其写入 1 条 ReMarket 事件。已停止错误连接的自建进程，获准精确删除本次创建的索引及其别名，未触碰其他索引。正确 ES 19200 重新初始化后用独立消费组从保留事件恢复，未跳过或重置原消费组 Offset。

修复三个 Search 命令：ELASTICSEARCH_URL 未设置时读取 ELASTICSEARCH_PORT；启动前 GET / 验证 ES 8 类型与 tagline，拒绝 OpenSearch，防止在错误实例继续建索引/写入；新增协议回归测试。SEARCH_CONSUMER_GROUP 可显式指定，默认仍 remarket-search-v1。

## 前端实现边界

商城改为真实服务器关键词、分类、价格和排序查询；管理页保留当前用户商品接口及原有本页筛选。取消旧请求以避免过时响应覆盖，筛选提交重置分页；金额使用 BigInt 转分；下一页使用签名游标；“返回第一页”和“刷新结果”开启新快照，不把已关闭的末页 PIT 用作上一页。409 显示重新搜索入口，故障显示错误，不回退本页伪搜索。详情和购买仍读取 Product 最新状态。无精确总数，不显示虚构总件数；无图片服务时保留缺图占位。

## 复现与待办

脚本默认请求 http://127.0.0.1:18081，可用 SEARCH_ACCEPTANCE_URL 指定其他独立测试 Gateway。脚本会创建实际账号与商品/订单记录，请仅用于本地验收环境；不输出或保存密码/Token，不自动删除历史数据。脚本通过轮询判断最终状态，每项预算 45 秒，不能据此推断性能 SLA。

尚未验收：浏览器点击与视觉、中文相关性质量、真实 DLT/重启/乱序故障注入、PIT 实际超时、维护窗口全量重建和回滚、生产权限/TLS与容量。上述内容仍为待办，不将模拟测试替代真实验收。
