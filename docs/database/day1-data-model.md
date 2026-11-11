# Day 1 数据模型设计

状态：设计稿及建表 SQL 已完成，未执行迁移，未实现 Go 模型或业务代码。

## 统一约定

- 使用 MySQL 8.4、InnoDB、utf8mb4；时间使用 UTC `DATETIME(6)`，连接会话及应用统一 UTC。
- 主键使用正数有符号 BIGINT、自增。ID 在所属实体内唯一，跨服务携带明确类型（如 product_id），不依赖不同表之间的全局唯一性。HTTP JSON 层建议将 ID 序列化为字符串，避免 JavaScript 精度丢失。
- 价格为正整数 `price_cent BIGINT`，单位为人民币分，当前不支持零价商品及多币种。
- 同库使用外键保证引用完整性，删除默认 RESTRICT；不建立跨库外键。用户、商品不物理删除，分别使用禁用、下架状态。
- 使用显式版本化 SQL，不用 GORM AutoMigrate 作为建表事实来源。DDL 非事务性，执行失败应检查已完成语句后恢复，不可盲目重跑。

## 表与关系

| 所属库 | 表 | 核心字段与职责 | Day 1 范围 |
| --- | --- | --- | --- |
| remarket_user | users | 用户名、密码哈希、昵称、头像对象键、角色、账号状态 | 完整数据结构 |
| remarket_user | follows | 关注者、被关注者、建立时间 | 建表与约束，业务留至 Day 4 |
| remarket_product | categories | 稳定 code、展示名称、排序、启用标记 | 建表与初始化分类 |
| remarket_product | products | 卖家、分类、标题、描述、价格、状态、聚合版本、首次发布时间 | 完整数据结构与状态规则 |
| remarket_product | product_images | 商品、对象键、排序 | 建表，Day 4 接入 MinIO |
| remarket_product | product_reservations | 商品、订单、买家、状态、过期及结束时间 | 建表，Day 2 实现预占 |
| remarket_product | outbox_events | 事件 ID、类型、聚合版本、JSON 载荷、发布状态及租约 | 定义事务写入规则，未实现代码 |

准确字段类型、默认值、约束以 `migrations/user/000001_core.up.sql` 和 `migrations/product/000001_core.up.sql` 为准。

### users 与 follows

`username` 唯一，默认排序规则不区分大小写和重音；Service 应限制为规范化的小写 ASCII 用户名，避免注册与登录规则不一致。密码只保存安全哈希，不存原文；具体哈希算法由认证模块确定。角色 USER/ADMIN，账号状态 ACTIVE/DISABLED。

`follows(follower_id, followee_id)` 唯一且禁止关注自己。双向用户引用均在 User 库内建立外键。唯一索引支持读取关注对象，`(followee_id,id)` 支持按游标读取粉丝。取消关注删除关系，重新关注创建新记录；暂不实现关注业务。

### categories、products 与 images

分类以 `code` 唯一标识，初始化六个平级分类；当前不引入分类树。分类禁用后不允许新商品使用，历史商品引用保留。

商品状态和事件发生时使用递增 `aggregate_version`。`first_published_at` 首次发布时赋值，后续不覆盖，作为首次发布判定和稳定列表排序依据。商品列表索引覆盖状态、分类、卖家三类常用过滤；全文检索交给后续 ES。

图片保存对象键，不保存临时签名 URL。`(product_id,sort_order)` 唯一，按 sort_order 最小的图片作为封面，不另设易产生不一致的封面标志。图片调序需在事务内使用临时位置避免唯一键碰撞。对象是否存在、类型、大小及上传权限由 Day 4 业务校验。

`seller_id` 是 User 服务的外部引用。发布时由可信身份上下文确定，不能直接信任客户端提交；不建立跨库外键。

### product_reservations

`order_id` 唯一，作为预占幂等键；重复请求必须校验 product_id、buyer_id 一致，并返回既有结果。已释放记录不能被同一订单重新激活，重新购买使用新订单。

预占状态为 ACTIVE、CONFIRMED、RELEASED。生成列 `active_product_id` 仅在 ACTIVE 时取商品 ID，否则为 NULL；其唯一索引保证一个商品最多有一个 ACTIVE 预占。NULL 可重复，因此保留多条历史预占。

预占和商品必须在 Product 本地事务内一起变更，唯一约束不能替代商品状态条件更新。过期索引用于扫描，但 `expires_at` 到达并不自动改变状态；过期释放必须结合订单结果及跨服务协议判定。确认和释放竞争同一商品及预占行，统一先锁商品、再锁预占，避免相反锁序。

`order_id`、`buyer_id` 不建立跨库外键。禁止本人购买、订单归属检查等由 Service 与服务契约校验。预占期限由配置决定，写入绝对过期时间。

## 商品状态机

| 当前状态 | 目标状态 | 操作与前提 | 事件 |
| --- | --- | --- | --- |
| DRAFT | ON_SALE | 卖家发布，内容、价格、分类有效，设置首次发布时间 | product.published.v1 |
| ON_SALE | OFF_SHELF | 卖家或管理员下架 | product.off_shelf.v1 |
| OFF_SHELF | ON_SALE | 重新上架，重新校验商品 | product.updated.v1 |
| ON_SALE | RESERVED | 订单预占成功，同时创建 ACTIVE 预占 | product.reserved.v1 |
| RESERVED | ON_SALE | 取消或安全过期释放，同时预占转 RELEASED | product.released.v1 |
| RESERVED | SOLD | 成交确认，同时预占转 CONFIRMED | product.sold.v1 |

SOLD 为终态；禁止其他转换。草稿和下架商品可编辑，在售商品可编辑但需要版本校验；RESERVED、SOLD 不允许修改价格、卖家或商品内容。草稿放弃保留草稿，当前不引入删除状态。

所有转换只能由 Service 执行，Repository 不提供不受限制的状态更新接口。事务内锁定商品或以 `id + status + aggregate_version` 条件更新，影响行数必须为 1。每次实际业务变更递增版本，相同状态重复调用不生成新版本和事件。

数据库 CHECK 约束仅校验状态取值及行内字段，不能证明历史转换合法。预占记录与商品状态的一致性也必须由事务保证。首次发布之后重新上架发送 updated，避免重复 Feed；Feed 消费者恢复展示和状态过滤需遵循这一事件约定。新增 released 事件用于恢复搜索中的可售状态。

## Outbox 事件与事务规则

`event_id` 使用 UUID，唯一；聚合标识为 `aggregate_type + aggregate_id`。`aggregate_version` 对应本次商品版本，`schema_version` 表示事件结构版本，两者用途不同。

`(aggregate_type,aggregate_id,aggregate_version,event_type)` 唯一，避免同一状态版本产生同类重复事件，同时允许未来同一版本产生不同类型事件。Outbox 不对商品建立外键，以便保留事件历史。

一次商品事务：读取并校验商品 → 更新商品并递增版本 → 按需更新图片或预占 → 插入事件 → 提交。任一步失败全部回滚。草稿编辑不产生对外事件，首次发布、在售编辑及可见状态变更产生相应事件。

事件载荷应是可独立消费的商品快照，包含 product_id、seller_id、category_id、标题、价格、状态、首次发布时间等投影字段；信封包含 event_id、event_type、aggregate_version、schema_version、occurred_at。大整数在 JSON 中按字符串编码。

发布状态：PENDING → PROCESSING → PUBLISHED；失败返回 PENDING 并增加 attempts、设置退避时间，达到上限进入 FAILED，人工处理后可重投。PROCESSING 使用 locked_until 和 lock_token 租约；进程退出后可重新领取过期租约，完成更新必须匹配 token，防止旧发布者覆盖新租约。

RabbitMQ publisher confirm 成功后才标记 PUBLISHED；发布成功但数据库标记失败可能重复投递。消费者仍需按 event_id 去重，并校验聚合版本，不能宣称 exactly-once。租约、发布器、消费者代码均不在本次范围。

## SQL 应用范围与验收

SQL 已生成但未执行：先在 remarket_user 应用 `000001_core.up.sql`，再在 remarket_product 应用 `000001_core.up.sql`、`000002_seed_categories.up.sql`。需通过后续迁移工具记录版本，业务账号没有 DDL 权限。种子迁移仅执行一次，不手动重复运行。

后续实测应覆盖：重复用户名及关注失败、自关注失败、负价与非法状态失败、一个商品不能拥有两个 ACTIVE 预占、同订单重复预占失败、事务回滚不残留 Outbox、并发版本更新只有一个成功。当前没有完成这些数据库运行验证，也没有实现用户/商品接口。
