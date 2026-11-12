# ReMarket 前端

React + TypeScript + Vite，企业系统风格的蓝灰配色、分类筛选和商品表格，响应式布局（手机端表格横向滚动）。运行时直接调用 Gateway，无假商品数据。

## 启动联调

1. 启动 MySQL 并迁移 User/Product 表及分类种子。
2. 为 User、Product、Gateway 导出数据库及一致的 JWT 配置，分别执行根目录 make run-user、make run-product、make run-gateway。
3. 依赖未安装时执行 make install，然后 make dev，打开 Vite 打印的地址（默认 http://127.0.0.1:5173/）。

Vite 将 /api 代理至 http://127.0.0.1:8080，不需要后端 CORS。可在 frontend/.env.local 设置 GATEWAY_PROXY_TARGET 后重启 Vite。不要将数据库密码或 JWT_SECRET 放入前端。

## 已接入

- 注册、登录、当前用户与退出本地会话。
- 商品广场、分类筛选、分页和详情。
- 草稿创建、编辑、发布、下架与重新上架。
- 加载、空状态、错误重试及版本冲突后刷新。

Token 仅存 sessionStorage；退出不撤销已签发的 JWT。后端没有我的全部商品接口，“我的商品”仅展示本浏览器按用户记住的最近100个商品 ID，再查服务端真实详情；也支持输入 ID 查找。图片上传和图片 URL 尚未接入，分类视觉为占位而非商品照片。

ID、版本、金额使用字符串，元与分使用 BigInt 精确转换，写操作不自动重试。

## 验证与边界

npm run build、npm run lint 已通过，金额精度/输入边界/API 客户端检查通过。本地预览 HTTP 响应正常。当前浏览器宿主不可用，未完成截图、移动端视觉或点击验证，真实后端联调留待执行。

Vite 开发代理不会包含在生产产物中，生产需 Nginx 将 /api 代理至 Gateway。本次不发布网站，不执行业务库迁移。
