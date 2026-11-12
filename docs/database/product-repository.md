# 商品 Repository 基础

实现文件：`backend/internal/product/repository/product.go`。

| 方法 | 行为 |
| --- | --- |
| CreateDraft | 创建草稿，显式设置 DRAFT、aggregate_version=1，首次发布时间为空，不写 Outbox |
| GetByID | 查询完整商品行；零 ID 不会退化为查询首条记录 |
| GetCategoryByID | 返回分类及 enabled 状态，包含禁用分类供业务判断 |
| ListEnabledCategories | 仅返回启用分类，按 sort_order ASC、id ASC 排序，无结果返回空切片 |

创建参数不接受商品主键、版本或状态。SellerID 由后续 Service 从可信身份取得；本层不处理鉴权、商品可见性、输入规范化或分类启用校验。GetByID 不能未经 Service 权限判断直接暴露为公共接口。

未找到分别返回 ErrProductNotFound、ErrCategoryNotFound；创建引用不存在分类返回 ErrCategoryReference。其他数据库错误保留原始根因，支持 errors.Is 判断 Context 取消和 Deadline；对外仍应经过统一错误映射。所有方法均使用 WithContext，无 AutoMigrate。

## 实测结果

真实 MySQL 临时库 race 测试已通过，应用现有 Product 建表和分类种子 SQL，覆盖：

- 六个分类初始化、分类字段映射。
- 草稿创建与读取、初始状态/版本、可空首次发布时间及时间字段。
- 创建草稿不写 Outbox。
- 商品和分类不存在、零 ID、分类外键错误及价格 CHECK 约束。
- 禁用分类过滤、相同排序值下按 ID 稳定排序、空列表。
- 取消、超时及关闭连接后的内部故障。

测试创建并清理 remarket_product_test_ 前缀临时库，不操作业务库。运行：配置 REMARKET_TEST_MYSQL_DSN 后，在 backend 执行 `go test -race ./internal/product/repository -v`。账号须拥有测试库 DDL 权限，仅使用开发实例；未配置变量时明确跳过数据库用例。

全量 Go 测试与 go vet 也已通过。本阶段不包含发布事务、状态变更、图片查询或业务 RPC。
