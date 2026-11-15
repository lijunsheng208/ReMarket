# ReMarket 阿里云 OSS 配置

用户指定 Bucket 为 `remarket`，华中1（武汉-本地地域）。根据阿里云官方地域表：Region 为 `cn-wuhan-lr`，公网 Endpoint 为 `https://oss-cn-wuhan-lr.aliyuncs.com`。本地开发不要配置 internal Endpoint，浏览器无法通过公网访问内网签名地址。

官方说明：https://help.aliyun.com/zh/oss/user-guide/regions-and-endpoints

本地 `.env` 已预留：

```dotenv
OSS_IMAGES_ENABLED=false
OSS_REGION=cn-wuhan-lr
OSS_BUCKET=remarket
OSS_ENDPOINT=https://oss-cn-wuhan-lr.aliyuncs.com
OSS_ACCESS_KEY_ID=
OSS_ACCESS_KEY_SECRET=
OSS_SESSION_TOKEN=
```

将 RAM 凭据填入本地文件，不提交 Git，也不要发送聊天；STS 临时凭据还需 OSS_SESSION_TOKEN。SDK 从这三个 OSS 环境变量读取凭据。RAM 权限至少包括目标 Bucket 的 `oss:GetBucketAcl`，以及 `staging/*`、`images/*` 对象的 `oss:GetObject`、`oss:PutObject`，不需要给应用 Bucket 删除权限。

Bucket 设置：

- ACL 为私有，开启阻止公共访问；不要通过 Bucket Policy 或对象 ACL 另行授予匿名访问。
- CORS 来源限定实际前端域名。本地通常是 `http://localhost:5173` 和 `http://127.0.0.1:5173`，其他开发端口需另加完整 origin。
- CORS 方法允许 PUT、GET、HEAD，允许头 `Content-Type`，可暴露 `ETag`。不需要允许携带 Cookie。
- 可为 `staging/` 配置一天过期生命周期。不要对正式 `images/` 设置会删除在用图片的生命周期。

配置完成后，先执行 Product `000006` 迁移，再将 `OSS_IMAGES_ENABLED=true`，重启 Product 和 Gateway；前端由开发服务器自动更新。业务代码不会自动创建 Bucket、改变云端 ACL/CORS 或执行迁移。

代码校验和私有 URL 无法替代云端的实际权限验证；未填凭据前不应标记 OSS 验收完成。

## 已启用状态（2026-11-15）

上述模板的本地实际开关已开启；新凭据仅存在本地 `.env`，未输出到文档。已验证 Bucket 私有 ACL，已应用迁移并重启 Product/Gateway。CORS 配置命令为 `cd backend && go run ./cmd/oss-cors`（需先导出本地配置），用于显式补充两个本地开发来源。真实 HTTP→OSS 直传与校验记录见 [图片接口验收](../api/images.md)。
