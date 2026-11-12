# 首次发布与 Outbox 事务

PublishProduct 请求携带正数 id、expected_version，要求有效 Bearer Token。Product 入口已注入 PublishService。当前仅卖家可发布自己的商品，管理员不能代发布。

Repository 开启本地事务，先 SELECT FOR UPDATE 锁商品，再对分类加共享锁。Service 回调在锁内依次检查所有权、预期版本和状态，确认分类启用，执行 DRAFT → ON_SALE，版本加一并设置 UTC 微秒首次发布时间；构造 product.published.v1。Repository 使用同一事务更新商品并插入 PENDING Outbox，任一步失败全部回滚。

相同当前版本的已在售商品重复发布直接返回，不加版本、不发事件。旧版本重试返回 FailedPrecondition；此行为不等同于请求幂等键重放。当前只允许首次上架，下架后重新上架仍待实现，不会误发 published 事件。版本达到 int64 上限拒绝递增。

事务更新只显式修改状态、版本及时间，不覆盖标题或价格。事务提交成功后才返回商品。未知数据库错误由通用拦截器隐藏为 Internal，失败返回不包含未提交商品。暂不自动重试事务；RPC 超时仍可能发生提交结果未知，调用方不能将超时视为数据库一定回滚。

事件 Payload 仅为发布后快照，信封元信息独立存储。RabbitMQ Publisher、消费者、事件重试和业务 HTTP 接口不在本次范围。

## 验证

真实 MySQL 独立临时库配合真实内存 gRPC、JWT、Service、Repository 的 race 测试已通过：

- 草稿创建后发布，商品 ON_SALE/版本2，存在唯一 PENDING published 事件。
- 匿名及其他用户（包括非所有者管理员）拒绝发布。
- 两个版本1请求并发，仅一个成功，另一个版本冲突。
- 同当前版本重复发布不增加事件。
- 临时库触发器注入 Outbox INSERT 故障，商品状态、版本和首次发布时间全部回滚。
- 分类停用拒绝首次发布。

测试仅修改并清理 remarket_publish_test_ 临时库，不迁移业务库。提供 REMARKET_TEST_MYSQL_DSN 后执行 `cd backend && go test -race ./internal/product/handler -v`；未提供时跳过真实数据库用例。验证不包含独立进程网络或 RabbitMQ 投递。
