# 创建商品草稿

CreateProduct RPC 已接入 Handler → Service → Repository。请求不包含 seller_id，Product Service 独立验证 Bearer JWT，从可信 Context 获取卖家。该身份代表签发时账号，不额外调用 User Service 检查实时禁用状态；不能据此声称实现了账号全局即时撤销。

标题去首尾空白，要求合法 UTF-8、1～128 个 Unicode 字符；描述保持原样，合法 UTF-8 且不超过 16 KiB，允许为空。分类 ID 和价格须为正整数，价格单位为分。

分类不存在返回 InvalidArgument，停用返回 FailedPrecondition；数据库故障统一对外 Internal。分类启用检查是查询时快照，本阶段不保证与并发分类停用原子互斥；发布阶段还需再次校验。取消和超时继续传递。

成功创建 DRAFT、版本 1、首次发布时间为空，不生成 Outbox。商品 ID、卖家、状态和版本由服务决定。没有创建幂等键，不自动重试创建请求。

Product 入口现在要求已导出的 PRODUCT_MYSQL_ADDR、PRODUCT_MYSQL_USERNAME、PRODUCT_MYSQL_DATABASE、MYSQL_PRODUCT_PASSWORD 和有效 JWT 配置（与 User/Gateway 一致）。不会自动读取 .env 或建表，需预先迁移 Product SQL 和分类种子，然后执行 `make run-product`。默认端口 50052。

本次只开放 CreateProduct 业务；其他商品 RPC 仍返回 Unimplemented，写接口要求认证，仅健康检查匿名放行。未接 Gateway 商品 HTTP 路由。

测试使用真实内存 gRPC、JWT、真实 Service 和模拟 Repository，覆盖匿名拒绝、伪造用户头、字段规范化、输入错误、分类状态和错误映射；上一步 Repository 已通过真实 MySQL 测试，本次没有 RPC 到 MySQL 的端到端实测。
