# Gateway 商品 API

Gateway 新增 PRODUCT_GRPC_TARGET，默认 127.0.0.1:50052。与 User/Product 使用一致 JWT 配置；只读/写操作均继续传递原 Context、Request ID 和超时。Token 在 Gateway 及 Product 独立验签，管理员操作由 Product 通过 User Service 核实。

| HTTP | 用途 | 身份 |
| --- | --- | --- |
| GET /api/v1/categories | 启用分类 | 可选 |
| GET /api/v1/products | 在售分页 | 可选 |
| GET /api/v1/products/:id | 详情及图片 | 可选，可见性由 Product 决定 |
| POST /api/v1/products | 创建草稿 | 必须登录 |
| PATCH /api/v1/products/:id | 编辑 | 卖家 |
| POST /api/v1/products/:id/publish | 首次发布/重新上架 | 卖家 |
| POST /api/v1/products/:id/off-shelf | 下架 | 卖家/当前管理员 |

可选身份表示无 Token 可匿名，提供无效 Token 仍返回401。客户端用户 ID 和角色头不转发为可信身份。创建成功201，其他成功200；错误沿用 Gateway 的统一结构及映射。

列表参数：category_id、page、page_size，默认全部分类/第一页/每页20条，最多100；重复参数、非整数或溢出拒绝。无权限查看非在售商品返回404。

## JSON 输入

所有 BIGINT 字段（分类 ID、价格分、预期版本）必须使用十进制字符串，避免前端 Number 精度丢失。查询和路径天然是文本；page/page_size 为32位整数。

创建：

```json
{"category_id":"1","title":"机械键盘","description":"功能正常","price_cent":"15000"}
```

编辑：

```json
{"expected_version":"2","price_cent":"16000","description":""}
```

发布/下架：

```json
{"expected_version":"3"}
```

请求需 application/json，沿用16 KiB请求体上限和未知字段拒绝。编辑未提供字段不变，description 空字符串清空；JSON null 对 optional 字段按未提供处理。由于整体请求体限制，较大的描述可能需要进一步缩短后提交。

响应采用 Proto JSON、snake_case，ID/金额/版本为字符串。未自动重试写入，版本冲突409时先刷新详情。

## 验证

TestProductHTTPMySQL 使用真实 HTTP、内存 gRPC、真实 User/Product Handler/Service/Repository、JWT/bcrypt、独立 MySQL 双库。覆盖注册登录、分类、草稿、公开与卖家详情、发布、分页、编辑及旧版本冲突、管理员下架、重新上架、管理员降权后旧 Token 拒绝、卖家下架、非法参数及 Outbox 数量。

测试管理员核实实际调用 User RPC，不再使用模拟管理员适配器。测试库创建后清理，不迁移业务库；不代表独立进程、TCP gRPC、Nginx、MQ 或前端部署验收。

配置 REMARKET_TEST_MYSQL_DSN 后在 backend 执行 `go test -race ./internal/gateway -run TestProductHTTPMySQL -v`。普通 go test 在未提供 DSN 时跳过真实数据库用例。
