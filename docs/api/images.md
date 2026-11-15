# 商品图片：阿里云 OSS

当前实现使用官方 OSS Go SDK v2，不依赖 MinIO。复用 product_images，新增版本化迁移 `migrations/product/000006_image_uploads.up.sql`，增加上传会话与图片类型、大小、宽高。迁移不会自动执行。

## 接口

| 方法 | 路径 | 功能 |
| --- | --- | --- |
| POST | /api/v1/products/:id/images/uploads | 申请临时对象 PUT URL |
| POST | /api/v1/products/:id/images/complete | 校验对象并关联商品 |
| GET | /api/v1/products/:id/images | 按商品可见性签发图片 GET URL |

申请上传请求：`{"content_type":"image/png","size_bytes":1024}`。响应包含 `upload_id`、`upload_url`、`expires_at`；将原始文件作为 PUT body 上传，完成后提交 `{"upload_id":"..."}`。完成接口返回最新商品版本与图片元数据，后续发布必须使用新版本。所有 BIGINT 按 Proto JSON 输出字符串，声明大小是受限 Number。

上传与完成必须是商品卖家。草稿、在售和下架状态可添加图片，预占和成交状态禁止。最多 9 张 JPEG/PNG，每张原文件和重新编码后均不超过 5MiB。单边最长 8192，像素总数最多 1600 万。不允许客户端自选对象键。

服务端重新读取对象并检查声明大小、MIME、图像解码和像素，再重新编码至独立随机正式对象。客户端持有的临时 PUT URL 不能覆盖正式图片。并发完成同一会话只关联一张图片、只增加一次商品版本；图片、版本、在售 updated Outbox 与缓存失效任务同本地事务提交。SDK/网络错误统一映射 Internal，不返回签名或凭据详情。

预签名 PUT 有效期及会话有效期均为五分钟。PUT 链接本身不保证云端提前阻止超大对象，大小约束在完成接口执行，非法对象不会关联或展示。签名 GET 有效期五分钟，访问前重新校验商品当前可见性；已经发出的链接在有效期内仍可使用，不声称下架能即时撤销已有签名。

前端支持选图后保存或发布：先创建/保存商品，再逐张直传和确认，最后使用最新版本发布。已有成功保存的图片在后续失败时保留，需要重新选择失败的文件。首页、关注流、ES 搜索卡片、我的商品和详情通过图片访问接口显示封面或图库。每张卡片会独立请求图片地址，当前未实现批量图片 URL 查询及 CDN。

## OSS 配置

参见 [OSS 本地配置](../storage/oss.md)。默认开关关闭，未配置时图片 RPC 返回 Unimplemented。开启后 Product 启动检查 Bucket ACL 必须为 private，不创建 Bucket，也不改写已有云端策略。

## 验证与边界

已通过真实临时 MySQL/Redis 测试：并发完成幂等、版本只递增一次、草稿匿名不可见、已完成会话不能被临时对象覆盖、在售图片变更与 Outbox 一致。对象层使用测试替身，此项不是实际 OSS 上传验收。图片内容测试覆盖大小、MIME、非图片和重新编码移除尾随脚本。

后续已配置真实 OSS：私有访问、预签名直传、服务端校验、幂等及临时对象覆盖隔离验收通过；本地 CORS 预检通过。浏览器页面手动交互仍未验收。

临时对象及失败/并发完成产生的未引用正式对象可能残留。建议为 `staging/` 设置一天生命周期；未引用正式对象需后续审计清理，不能按时间直接删除可能已被数据库引用的对象。当前没有自动对象回收任务，也未实现图片删除及手动调序。

标准 `go test ./...`、图片相关 race/MySQL 测试及前端 lint/build 已通过。额外开启全部 Redis 集成时，已有 `TestRedisProtocol` 的空值 TTL 上界断言出现失败；该测试单独连续三次通过，未将这轮 Redis 全量结果记为通过。没有为图片功能修改该缓存测试或缓存寿命逻辑。

## 实际启用记录（2026-11-15）

已启用本地 `OSS_IMAGES_ENABLED=true` 并应用 Product `000006` 迁移；Product 与 Gateway 已运行当前版本。真实 OSS Bucket `remarket` 位于 `cn-wuhan-lr`，ACL 检查为 private。通过显式 `go run ./cmd/oss-cors` 合并本地 localhost:5173 与 127.0.0.1:5173 的 PUT/GET/HEAD 规则，保留其他规则。该命令需独立的 GetBucketCORS/PutBucketCORS 配置权限；运行中的 Product 不需要这两个权限。

`python3 backend/scripts/images_acceptance.py` 实际验证通过：声明与签名申请、PNG 直传、重新编码及元数据保存、幂等完成、签名图片可读、无签名访问 403、临时对象覆盖不影响正式图片、草稿匿名拒绝及发布后公开展示地址。验收商品 25 已下架；排查签名期间产生的验收草稿及云端临时对象保留，不删除既有业务数据。首次签名未绑定 Content-Type 的问题已修复。

CORS OPTIONS 预检返回 200 并限定允许的本地 origin。尚未将此项记为浏览器选图交互通过。
