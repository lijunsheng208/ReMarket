# 聊天前端与第六批验收

## 已接入

商品详情的“联系卖家”创建或复用服务器会话，成功后进入消息页；本人商品不显示自聊入口。消息页提供会话列表、真实最后消息预览、关联商品入口、历史消息、文本发送及未读/已读反馈。桌面双栏，手机在会话列表与消息间切换。商品不可见或加载失败会显示错误，不伪造商品资料。

会话列表新增 last_message_content Proto 字段，由 Chat 本库按 last_message_id 左关联查询，不新增表列或数据库迁移，不逐会话请求最后一条消息。昵称与商品标题分别调用已有公开资料/商品 API，读取失败保留默认名称或查看入口。导航未读徽标只代表已加载会话，存在后续页时显示加号，不声称为账号全部会话未读总数。

## 消息与恢复规则

- 登录期间保持一条账号级 WebSocket，离开消息页仍可收到通知；账号或 Token 变化释放旧实例，取消 HTTP 与定时器，迟到响应不得写入新账号。
- 通过 HTTP Bearer 取得短期票据，同源 WebSocket URL 仅带 ticket。Vite /api 代理已开启 ws，浏览器本身不持有服务端密钥。
- 握手后等待应用 pong 再认为就绪；每十秒校验心跳，四十秒无 pong 则断开。每次重连重新申请票据，退避从一秒增长至三十秒；网络/页面恢复主动同步，认证失效停止并退出本地登录。
- 待发箱保存在当前浏览器 sessionStorage，键按账号隔离，不保存 JWT。消息发出前保存 client_msg_id、会话、原文及创建时间；最多一百条待发，最多五条同时在途。会话存储不可用时拒绝新增发送，不先发出再丢失原键。
- 断线、ACK 超时和刷新恢复沿用原 client_msg_id；在途八秒无 ACK 只能视为结果未知，可安全重试。明确业务错误保留原键和错误供手动处理，不自动创建第二个意图。
- ACK 仅显示“已发送”。补拉/推送找到同发送者、会话、键及正文的已保存消息，也能消除待发项。重复通知按消息 ID/seq 去重，所有 BIGINT 均为字符串并用 BigInt 比较。
- “移除”只删除本地待发项，不是撤回。未知请求可能已经保存，之后仍可从历史查询到。
- 已读通知按用户与会话取最大游标，仅观察到对方真实已读事件后才显示“对方已读”，不把 ACK 当送达或阅读。刷新前的对方阅读回执没有额外权威查询接口，因此可能暂时仍显示“已发送”。

首次打开会话读取最近二十条，建立本次历史窗口的明确下界；更早消息通过 before_seq 向前分页，加载后保持可见消息滚动锚点。实时同步位置只覆盖这段已持有的连续窗口，不把最高乱序推送或账号 last_read_seq 当作本设备同步位置。刷新不恢复缺少对应消息缓存的旧同步游标，而是重新建立最近窗口基线。

重连、十秒周期、页面重新可见和网络恢复均按 after_seq 与固定 sync_until_seq 补拉，填补乱序缺口，覆盖长连接仍在但 Pub/Sub 遗漏的情况。单轮最多一百页，持续积压留给下一轮。补拉响应必须连续、推进且不越过本轮上界，空页不能回退位置。

只有消息页可读、浏览器有焦点且可见、聊天窗口处于底部并完成渲染时，才通过 HTTP 提交连续位置；后台接收、补拉或查看旧历史不自动清空未读。迟到的会话列表若跨越了本地消息/已读变更，会被丢弃并重查，避免旧快照覆盖新状态。正文由 React 文本节点渲染，不插入 HTML。

未发送输入草稿在同一消息页内按会话保留；页面卸载前尚未点击发送的草稿不持久化。已入待发箱的消息会保留至确认或显式移除，退出登录后不展示给其他账号。

## 运行

沿用 [Chat 配置](chat.md)。启动真实 User、Product、Chat 与 Gateway，显式应用 Chat 已有迁移。Gateway 的 CHAT_ALLOWED_ORIGINS 必须包含实际前端来源，例如 http://127.0.0.1:5173。Vite 运行 `npm --prefix frontend run dev`，GATEWAY_PROXY_TARGET 默认 http://127.0.0.1:8080，可通过前端环境文件覆盖；修改代理配置后需重启 Vite。

多 Gateway 时仍需让票据签发与握手落在同一 Node，并启用同一 Redis 命名空间；本批不实现共享票据、Nginx 或生产部署。

## 验证（2026-11-15）

已通过：

- `npm --prefix frontend run build`、`npm --prefix frontend run lint`（无警告）。
- `npm --prefix frontend run test:chat`：十一个 Node 原生 TypeScript 测试，覆盖连续账本、大整数、离线先保存、断线/刷新稳定键、ACK/推送去重、固定上界缺口补拉、显式已读、账号生命周期、存储失败、迟到列表、五条在途与超时重试、损坏记录不静默覆盖。
- `npm --prefix frontend run test:orders`：四个订单回归测试。
- `cd backend && go test ./...`，以及配置测试 MySQL/Redis 后的 `go test -race ./internal/chat/... ./internal/gateway -count=1`。
- 真实 Chrome 桌面 1365px 与手机 390px：联系卖家、消息去重、自动重连、通知遗漏补拉、后台未读、文本安全、前向历史分页保留滚动锚点、退出与无横向溢出。此组使用 HTTP/WS 协议夹具，不计为业务后端联调。
- 额外真实 Chrome → Vite WebSocket 代理 → 两个 Gateway Chat Node → 真实鉴权 Chat gRPC → 真实 MySQL/Redis 的 race 测试，验证跨 Node 回复、真实 ACK 已保存后在浏览器丢弃并断线恢复、直接持久化不通知后的主动补拉；数据库逐项确认没有重复保存。
- `git diff --check` 通过。

真实接线测试的卖家为另一个 Node 上的 Go WebSocket 客户端；两个 Node 与 Chat 服务在同一 Go 测试进程，gRPC 使用真实编解码与独立 JWT 验签的内存传输。User 状态 RPC 和辅助用户/商品展示数据仍为夹具，不代表独立 User/Product 进程验收。测试仅使用随机临时库、Redis 前缀和短期测试 JWT，未修改业务数据或本机业务服务启用状态。最终测试直接管理 Vite 进程并自动释放测试端口；未发布网站或创建 Git 提交。

### 浏览器测试复现

需先在临时 Python 虚拟环境安装 Playwright，主机已有可运行的 Chrome。没有增加 npm 生产依赖。Node API 测试沿用项目的 Node 26 原生 TS 类型擦除方式。

纯界面夹具：启动 Vite 的 15173 端口后，使用安装了 Playwright 的 Python 执行 `frontend/tests/chat-browser.py`，可通过 CHAT_WEB_ORIGIN 改端口。

真实接线测试：导出隔离测试管理员 DSN、Redis 环境配置，以及：

```sh
export CHAT_BROWSER_CHECK=1
export CHAT_BROWSER_PYTHON=/path/to/playwright-venv/bin/python
cd backend
go test -race ./internal/gateway -run TestChatBrowserLive -count=1 -v
```

需空闲的本机 15174 端口。测试会自动启动 Vite 与隔离 Gateway，完成后关闭，不需要启动业务服务。Chrome 截图写入 /private/tmp/remarket-chat-browser；纯界面截图为 chat-desktop.png、chat-mobile.png，真实接线截图为 chat-live.png，已实际查看验证。

剩余验收：独立业务服务进程与真实账号/商品闭环、Node 独立进程强杀、Redis 整体停机恢复、大规模压测、真实移动设备与输入法/软键盘兼容性。不得将本批通过情况扩写成这些场景已验收。
