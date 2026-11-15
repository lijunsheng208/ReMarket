# Feed 基础功能

独立 Feed 服务提供 MySQL 权威查询版本。Gateway → Feed → User / Product，Feed 不连接其他服务数据库。Redis 时间线与 Kafka 推拉结合已通过显式开关接入，详见 [阶段 3](feed-hybrid.md)。

## 接口

- `GET /api/v1/feed/home`：匿名可用，返回全部在售商品。
- `GET /api/v1/feed/following`：要求 Bearer JWT，只返回当前用户关注卖家的在售商品。不能通过用户 ID 参数或头指定他人身份。

两个接口均接受 `page_size`（默认 20，最大 100）和 `cursor`。响应为 `products` 与 `next_cursor`，下一页原样传回游标。空字段遵循 Proto JSON 省略规则，空结果可能为 `{}`；BIGINT 与金额为字符串。

排序为 `(created_at DESC, id DESC)`，以不可变创建时间及主键打破平局。这里的“最新”指商品创建顺序，重新上架不改变排序。游标不是跨请求快照，新插入商品需刷新首页获取；商品下架、售出、预占后下一次查询即过滤。当前请求内跨服务数据也不是分布式事务快照；请求期间发生的关注变更在下一次读取生效。

关注流逐页读取 User 关注列表，将卖家 ID 条件交给 Product 查询；无关注时返回空，不能退化成首页。基础版最大支持每次查询 10000 个关注卖家，超限明确失败，不静默截断。大规模关注优化交由后续 Feed 索引实现。下游失败整体返回错误，不返回伪造成功或部分结果。

Gateway、Feed、User 独立验证 JWT。Feed 仅转发原始凭据，Product 仅返回公开在售商品，卖家过滤不授予额外权限。超时沿上游派生，Feed 完整编排最多三秒，Request ID 经现有 grpcx 拦截器传递。

## 运行

按项目现有方式导出 `.env` 配置后，分别启动 `make run-user`、`make run-product`、`make run-feed`、`make run-gateway`。Feed 使用相同 JWT 配置，以及 `USER_GRPC_TARGET`、`PRODUCT_GRPC_TARGET`；默认监听 `127.0.0.1:50055`，可通过 `FEED_GRPC_ADDR` 调整。Gateway 使用 `FEED_GRPC_TARGET`。

新增 Product 迁移 `000005_feed_index.up.sql` 为创建时间排序增加索引；按版本化迁移流程执行，不自动迁移。本次未在现有业务库执行迁移。

## 验证

2026-11-15：`TestFeedHTTPMySQL` 在两个独立临时库，通过 HTTP 路由及三服务真实 gRPC 链路验证：匿名首页、关注鉴权、无关注空流、关注后仅展示目标卖家、同微秒游标不重复、取消后消失、非在售过滤、下架后消失、非法游标及页大小拒绝。测试后清理临时库，不修改业务数据。

最终检查：配置测试 MySQL 后 `cd backend && go test ./...` 通过；`go test -race ./internal/gateway -run TestFeedHTTPMySQL` 通过；`git diff --check` 通过。

前端已接入：首页无筛选时请求 `/feed/home`；提交关键词、分类、价格或非默认排序后请求 ES `/search/products`；“关注动态”请求 `/feed/following`，未登录时打开登录框。切换来源、分类或账号会重置游标，旧请求通过 AbortController 取消。重置搜索返回最新商品。关注动态隐藏全站搜索栏与外层分类栏，仅提供动态列表和分页；返回“最新商品”后恢复搜索入口。前端 lint 与 build 通过；当前环境未安装 Playwright，浏览器交互及常驻服务端到端验收待完成。

Kafka 消费、Redis 时间线、热门卖家合并及按需窗口回填已实现，详见 [阶段 3](feed-hybrid.md)；离线全量时间线重建任务尚未实现。
