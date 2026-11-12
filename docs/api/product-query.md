# 商品列表、详情与可见性

新增 ListProducts、GetProduct RPC。生产 Product 进程采用可选鉴权：两个读取方法无 Token 可匿名访问，有凭据则必须验签，缺失/无效/重复凭据不会混为一谈；写方法仍强制鉴权。

ListProducts 只查 ON_SALE。category_id=0 表示全部，负值拒绝；page=0 默认 1，page_size=0 默认 20，负数或 size>100 拒绝。按 first_published_at DESC、id DESC 稳定排序。一次响应使用 MySQL REPEATABLE READ 只读事务保证 COUNT 和列表共享快照及过滤条件；不同翻页请求间仍可能因数据变更出现重复/遗漏。列表不加载图片，避免每条记录额外查询；详情加载按 sort_order、id 排序的图片对象键，不生成 MinIO 签名 URL。

GetProduct 要求正数 ID。在售商品公开；DRAFT、RESERVED、SOLD、OFF_SHELF 仅卖家或经核实的管理员可见，其他调用者统一 NotFound。卖家 ID 来自可信 Context。管理员候选 Token 角色必须为 ADMIN，再调用 User Service GetCurrentUser 核实同一用户的当前 ACTIVE/ADMIN 状态；故障或认证失败不降级放行。角色已降为普通用户则返回 NotFound。

Product 进程新增对 USER_GRPC_TARGET 的使用，默认 127.0.0.1:50051。通过标准 Client 拦截器转发原 Token，User Service 再次验签，不直接读取 User 数据库。管理员核实调用超时三秒且不延长上游 Deadline。普通公开读取和卖家查看自身商品无需此调用。卖家身份仍为 JWT 签发快照，不提供账号全局即时撤销能力。

详情商品状态在读取该行时决定，图片在后续查询读取，不承诺响应返回时状态未变化。当前图片写业务尚未接入。

测试使用真实 MySQL 临时库与内存 gRPC 验证列表过滤、分页总数、时间相同按 ID 排序、分类过滤、参数边界、详情图片顺序、匿名/卖家/其他用户可见性、伪造身份、管理员降权及无效 Token。管理员检查在该集成测试中使用模拟适配器，未将独立 User Service 与 Product 双进程串联验收。业务库不执行迁移，HTTP 商品路由留到后续阶段。
