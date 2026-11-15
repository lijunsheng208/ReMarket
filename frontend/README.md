# ReMarket 前端

React + TypeScript + Vite，参考二手商城的信息布局：白底蓝色强调、紧凑顶部、独立分类栏、商品图片卡片（桌面最多六列、手机双列）；商品管理继续使用表格。运行时直接调用 Gateway，无假商品数据。

## 启动联调

1. 启动 MySQL 并迁移 User/Product 表及分类种子。
2. 订单功能还需应用 Order 000001/000002、Product 000003 迁移（已执行的版本不要重跑），为 User、Product、Order、Gateway 导出各自数据库和一致 JWT 配置。Order/Product 配置相同的独立 ORDER_PRODUCT_SERVICE_TOKEN，Gateway 配置 ORDER_GRPC_TARGET；分别执行 make run-user、make run-product、make run-order、make run-gateway。
3. 依赖未安装时执行 make install，然后 make dev，打开 Vite 打印的地址（默认 http://127.0.0.1:5173/）。

Vite 将 /api 代理至 http://127.0.0.1:8080，不需要后端 CORS。可在 frontend/.env.local 设置 GATEWAY_PROXY_TARGET 后重启 Vite。不要将数据库密码或 JWT_SECRET 放入前端。

## 已接入

- 注册、登录、当前用户与退出本地会话。
- 商品广场、分类筛选、分页和详情。
- 草稿创建、编辑、发布、下架与重新上架。
- 加载、空状态、错误重试及版本冲突后刷新。

Token 仅存 sessionStorage；退出不撤销已签发的 JWT。商品管理调用服务器的 /api/v1/products/mine 分页接口，按当前登录账号展示全部状态商品，不再依赖本机编号记录。图片上传和图片 URL 尚未接入，分类视觉为占位而非商品照片。

ID、版本、金额使用字符串，元与分使用 BigInt 精确转换，写操作不自动重试。

## 验证与边界

npm run build、npm run lint 已通过，金额精度/输入边界/API 客户端检查通过。本地预览 HTTP 响应正常。当前浏览器宿主不可用，未完成截图、移动端视觉或点击验证，真实后端联调留待执行。

Vite 开发代理不会包含在生产产物中，生产需 Nginx 将 /api 代理至 Gateway。本次不发布网站，不执行业务库迁移。

顶部查找框按商品编号查询，不提供全文搜索。没有收藏、消息等无后端支持的假入口。图片字段当前只有对象键，未当作可访问 URL 使用；商品市场明确显示暂无图片占位，后续图片 URL 能力完成后可替换。

## 订单页面（本轮）

新增商品详情购买入口、确认下单、我买到的/我卖出的列表、订单详情、取消与双方确认。下单键跨重试保留，PENDING 不当作成功预占；页面查询真实最终状态。详情与实现边界见 [订单前端说明](../docs/api/order-web.md)。

build、lint（无警告）、test:orders 四个 Node API/结算键测试及差异检查通过。浏览器宿主不可用且 Python Playwright 未安装，未验收截图、点击及真实后端联调。运行 `npm run test:orders`，本轮验证使用 Node 26.4.0 的原生 TypeScript 类型擦除与模块钩子，无新增 npm 依赖。

本地启动复核（2026-11-13）：缺失 Order 000001/000002 与 Product 000003 已应用到本地业务库并记录版本，请勿再次执行；Order/Product/Gateway 已加载当前代码，健康、商品及买卖订单列表实测 200。订单标题辅助小字已移除，前端 build/lint 通过。尚未进行完整浏览器交易验收。

## Day 5 聊天前端（2026-11-15）

已接入消息页、商品联系卖家、会话/历史分页、自动重连、按账号保存的稳定键待发箱、连续序号补拉及可见窗口已读；Vite /api 代理已开启 WebSocket。本节覆盖前文早期“没有消息入口”和“浏览器宿主不可用”的历史状态，当前准确边界见 [聊天前端说明](../docs/api/chat-web.md)。

Gateway 的 CHAT_ALLOWED_ORIGINS 需包含实际 Vite 来源；跨 Gateway 模式沿用 Redis 配置并确保票据签发与握手同 Node。聊天 ID 始终为字符串，正文只按文本渲染。消息持久化 ACK 不表示对方收到或已读。

本轮 build、无警告 lint、test:chat 十一个测试、test:orders 四个回归均通过。Chrome 桌面/手机协议夹具验收及真实 Vite→双 Chat Node→鉴权 gRPC→临时 MySQL/Redis 的浏览器 race 接线通过；后者辅助 User/Product 仍为夹具，不能视为独立完整业务服务验收。隔离资源和临时服务已清理，业务数据未修改。
