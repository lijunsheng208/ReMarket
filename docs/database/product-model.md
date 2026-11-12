# 商品 Go 模型与首次发布事件

模型阶段实现数据结构及事件构造；后续已完成 [Repository 基础](product-repository.md) 并在临时库执行 SQL 验证。业务库未迁移，状态机 Service 与业务 RPC 尚未实现。

`backend/internal/product/model/product.go` 映射 categories、products、product_images、product_reservations、outbox_events 五张表。价格、ID、版本使用 int64；可空时间、对象及错误字段使用指针；ProductStatus、ReservationStatus、PublishStatus 使用字符串类型，与 SQL 取值一致。预占生成列 active_product_id 标记为 GORM 只读。

显式指定 TableName，不添加 AutoMigrate。模型只定义结构，不自动添加价格、状态或启用默认值，后续创建路径需显式赋值；状态转换必须通过 Service。

首次发布契约位于 [事件文档](../../api/events/product/README.md)，Go 结构与构造位于 `backend/internal/product/events/published.go`。Outbox 初始 PENDING，attempts=0，不持有租约；商品快照与信封元信息分开存储。

后续实现基线：

- 卖家从可信身份 Context 获取，Product Service 独立验签；管理员下架需要核实当前管理员权限，不能仅信任旧 Token 角色。
- 编辑、发布、下架带 expected_version，先查所有权再检查版本和状态；版本冲突返回 FailedPrecondition。
- 已在售且版本匹配的重复发布返回原商品，不加版本、不发事件；重新上架发送 updated，首次发布时间不变。
- 公开列表仅 ON_SALE，按 first_published_at DESC、id DESC；page=0 默认 1，page_size=0 默认 20，范围 1～100，负值拒绝。
- 公开详情匿名查看在售；其他状态仅卖家或管理员可见，无权限返回不存在。有无效 Token 时不降级为匿名。
- 编辑允许 DRAFT、ON_SALE、OFF_SHELF；RESERVED、SOLD 禁止编辑或下架。空编辑拒绝，字段无实际变化不加版本或写事件。

测试覆盖事件 UUID、超大 ID 字符串精度、UTC 微秒精度、Outbox 初始状态、非法快照及 JSON 示例往返；不代表真实 MySQL 映射或商品事务验收通过。
