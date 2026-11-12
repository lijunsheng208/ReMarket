# 商品编辑、下架与重新上架

已接入 UpdateProduct、OffShelfProduct，PublishProduct 扩展支持 OFF_SHELF → ON_SALE。均要求有效 Token、正数 id 和 expected_version，不允许修改卖家。

| 操作 | 权限 | 状态 | 事件 |
| --- | --- | --- | --- |
| 编辑 | 仅卖家 | DRAFT、OFF_SHELF | 无 |
| 编辑 | 仅卖家 | ON_SALE | product.updated.v1 |
| 下架 | 卖家或经实时核实的管理员 | ON_SALE → OFF_SHELF | product.off_shelf.v1 |
| 重新上架 | 仅卖家 | OFF_SHELF → ON_SALE | product.updated.v1 |

RESERVED、SOLD 禁止编辑或下架；管理员不能代编辑或发布。下架不要求分类启用，编辑目标分类和重新上架分类必须启用。

编辑保留 Proto optional 语义，未提供字段不变，description 空字符串可清空。标题去首尾空白、1～128 字符，描述合法 UTF-8 且最多 16 KiB，分类 ID 和价格为正整数。空编辑拒绝，无实际变化不加版本或事件，但仍检查权限、版本、状态和分类。旧版本返回 FailedPrecondition。

重复下架已下架商品且当前版本匹配时返回原数据。重新上架保留首次发布时间，不重复产生 published。实际变更才递增版本，达到 int64 上限拒绝更新。

事务先锁商品，再锁目标分类。Service 回调决定状态转换及事件；Repository 显式保存允许字段。在售编辑、下架及重新上架与事件同事务，插入事件失败则商品回滚。草稿和下架状态的编辑无事件但仍事务保存。

管理员核实在事务前调用 User Service，避免持有锁期间外部 RPC；核实和执行之间仍有权限变更时间窗口，不提供跨服务原子一致性。卖家身份仍依赖 JWT 快照，无全局即时撤销。

事件 Schema 与示例在 api/events/product/，updated/off_shelf 复用完整快照字段，分别要求 ON_SALE/OFF_SHELF；Payload 只保存 data。RabbitMQ 投递和消费者尚未实现。

真实 MySQL 临时库及 gRPC race 测试已通过：草稿/在售编辑、空编辑和无变化、并发版本冲突、下架/重新上架、首次事件不重复、管理员权限、交易状态保护、Outbox 故障下编辑和下架完整回滚。管理员实时核实在测试中为模拟，未完成独立双服务联调。仅创建清理临时库，业务库未修改。
