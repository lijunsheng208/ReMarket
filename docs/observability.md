# Day 6：流量入口与可观测性

## 可 review 批次

| 批次 | 交付 | 当前状态 |
| --- | --- | --- |
| 1 | Nginx、静态页面、HTTP/WS 代理、双 Gateway 亲和及安全边界 | 代码及真实 Nginx 容器验收完成，见下文 |
| 2 | Gateway HTTP、gRPC 请求指标、WS 连接及 Prometheus 采集 | 代码及真实 Prometheus 隔离采集验收完成，见第二批 |
| 3 | Grafana 数据源、QPS/错误率/P95/P99/在线状态面板及告警 | 待实现 |
| 4 | OTel、跨进程 Trace Context、关键数据库/Redis Span、Jaeger | 待实现 |
| 5 | JSON 日志、service/request_id/trace_id、采集器与 Loki | 待实现 |
| 6 | 延迟演练、指标→Trace→日志定位、扩容与 WS 联调 | 待验收 |

沿用 backend/cmd、backend/internal、backend/pkg 目录，不照搬计划中的示意目录重构业务。当前 Request ID 转发、已有日志或 traceparent 字符串转发不等于 OTel Trace 已实现。Prometheus 已在第二批接入并通过隔离采集验收；Grafana、Jaeger、Loki 尚未实现，不以配置目录存在代表监控可用。

## 第一批：Nginx 统一入口

### 部署模型

现有 Go 服务运行于宿主机，因此本批提供独立的 deployments/docker-compose.edge.yml，只启动 Nginx，不容器化或重启现有数据库、Kafka、Redis、ES 和业务服务。项目名固定 remarket-edge，与 remarket 基础设施分离。

Nginx 默认发布 127.0.0.1:8088，服务 frontend/dist 静态产物，API 转发宿主机 Gateway 8080、8081。两个 Gateway 使用同一 Chat RPC 与 Redis 命名空间。新的 8088 浏览器 Origin 与 Vite 5173 的 sessionStorage 独立，需要在入口页面重新登录。

配置文件：

- nginx/remarket.conf.template：上游、路径、JSON 访问日志、静态入口。
- nginx/remarket-proxy.conf：共同代理边界与禁止自动重放。
- nginx/19-resolve-gateway.envsh：envsubst 前将上游主机名固定为 IPv4。

启动脚本针对实际发现的 Docker Desktop 双栈问题：host.docker.internal 同时返回可用 IPv4 与不可达 IPv6，使未经处理的静态 upstream 部分请求返回 502。脚本通过 getent ahostsv4 解析一个 IPv4，失败即停止启动；地址变更后需重建容器。本批面向监听 IPv4 的本机开发布局，不宣称支持纯 IPv6 部署。

### 代理及亲和

普通 /api/ 不携带 Upgrade/Connection 升级语义，只有精确 /api/v1/chat/ws 配置 HTTP/1.1、Upgrade、Connection、关闭缓冲和 75 秒读取超时。业务 Node 每二十秒控制帧心跳续活，代理并不自动迁移已建立连接。Host 使用浏览器原始 Host（包含端口），保留 Origin 与 Authorization，继续由 Gateway/业务服务独立验签。

上游采用 `hash $remote_addr consistent`，按完整来源 IP 维持亲和；没有采用默认逐请求轮询，因为票据当前存于 Node 本地。没有采用只取 IPv4 前三段的 ip_hash，避免同一 Docker 子网中的所有验收客户端必然落在同一节点。相同来源的票据申请和握手保持一致，多用户共用 NAT 时可能集中在同一 Gateway，不能承诺每个请求均匀分配。

Gateway 不可用时当前请求失败，不向另一节点自动重放；调用方重新获取票据并重连。服务成员、地址、可用性变化可能改变亲和，票据仍需重新申请。共享票据或 Cookie 路由属于后续演进，不在本批假装已完成。

HTTP 连接超时两秒、读写超时十五秒，WS 读超时七十五秒。proxy_next_upstream off 禁止代理自动重放，避免写入结果未知后再次提交；业务幂等与客户端原键重试继续有效。未增加自动重试非幂等请求的配置。

### 安全与日志边界

入口覆盖 X-Forwarded-For，不信任外部传入来源链；清除 X-User-ID、X-Role，用 Nginx request_id 覆盖外部请求编号并转发，Gateway 响应返回同一编号。不启用基于任意客户端头的 real_ip。

当前 Gateway 的 SetTrustedProxies(nil) 保持不信任转发头，因此匿名 IP 限流在代理后可能按 Nginx 地址聚合；尚未实现可信代理 CIDR 配置及其限流联调，不能把本批描述为已经正确恢复每个终端真实 IP。后续治理需显式限定可信代理网段，不能直接信任所有 XFF。

访问日志为 JSON，包含 service、request_id、method、path、status、耗时和上游地址/状态/耗时。只记录 $uri，不记录 $request_uri、query、Authorization、Cookie 或正文。Nginx 原生错误日志不能逐字段脱敏，所以 WS 路径关闭原生错误日志，诊断使用对应结构化访问日志的状态和上游耗时；避免异常日志泄露一次性 ticket。该访问日志尚未送入 Loki，也没有 trace_id。

/metrics 不随统一业务入口公开；后续需内网采集方案。/edge-healthz 只表示 Nginx 存活；/healthz 代理 Gateway 存活检查，均不代表所有下游业务可用。当前没有 TLS、生产域名、认证监控面板或生产高可用验收。

### 运行方式

先按已有说明启动 User/Product/Order/Search/Feed/Chat。导出 .env 后，在两个终端分别启动：

```sh
CHAT_REDIS_ENABLED=true GATEWAY_HTTP_ADDR=127.0.0.1:8080 make run-gateway
CHAT_REDIS_ENABLED=true GATEWAY_HTTP_ADDR=127.0.0.1:8081 make run-gateway
```

如果某个端口已有 Gateway，不要再启动重复进程；使用当前代码的实例，并确认两端连接同一 Redis。宿主机已有前端 Origin 白名单可保留，同源入口 Host/Origin 会直接匹配，反向代理保留 Host 端口。

```sh
npm --prefix frontend run build
make infra-config
make edge-config
make edge-check
make edge-up
```

打开 http://127.0.0.1:8088。edge-up 会重建前端静态产物；修改 React 代码后需重新构建，Nginx 不提供 Vite 热更新。edge-check 绑定 frontend/dist，所以首次检查前需先构建。

配置项见 .env.example：EDGE_HTTP_PORT、EDGE_GATEWAY_HOST、EDGE_GATEWAY_PRIMARY_PORT、EDGE_GATEWAY_SECONDARY_PORT。Docker Desktop 默认 host.docker.internal；Linux 配置 host-gateway 额外主机映射，具体环境仍需自行实测。NGINX_ENVSUBST_FILTER 限定 EDGE_ 环境变量，避免替换 Nginx 自身的 $uri 等变量。

`make edge-logs` 查看入口日志；`make edge-down` 仅停止独立入口项目，不带 -v，不关闭已有基础设施。

### 实际验收（2026-11-15）

已通过：

- `make infra-config` 与 `make edge-config`。
- `sh -n deployments/nginx/19-resolve-gateway.envsh`。
- 官方 nginx:1.28-alpine 镜像实际 nginx -t，运行时版本 1.28.3。
- `NGINX_EDGE_INTEGRATION=1 go test -race ./internal/gateway -run TestNginxEdge -count=1 -v`：真实 Nginx 容器、两个独立 Gin/Chat Node 监听器；多个 Docker 客户端真实来源 IP 实际覆盖两个上游；普通 HTTP 去除升级语义和伪造身份头、重设来源链/Request ID、保留 Host；票据与 WS 同源亲和，真实 101 与 pong；前端 HTML 与 /metrics 隔离；记录完成的 101 访问日志，验证 ticket、查询探针和 JWT 未出现在日志。
- 上游已接收 POST 后主动关闭连接，Nginx 返回 502 且接收次数恰好一次，未向另一个 Node 重放。
- `cd backend && go test ./...` 和 `git diff --check`。

复现真实容器验收前需 frontend/dist 与已下载镜像，Docker 可运行。普通 Go 测试不设置 NGINX_EDGE_INTEGRATION 时跳过该集成测试。

测试的两个 Gin Node 位于同一 Go 测试进程，用户 RPC 与聊天业务客户端为受控夹具；实际 WS 处理、JWT 验签、HTTP 代理和 Docker 网络均真实。不是独立生产 Gateway 进程、真实账号下单/聊天业务或长时间 WS 压测验收。测试容器/客户端/网络均按随机名称隔离并清理，不修改业务库与业务 Redis，不替换用户运行中的 Gateway，不常驻开启入口。

参考：[Nginx WebSocket 代理](https://nginx.org/en/docs/http/websocket.html)、[upstream 哈希](https://nginx.org/en/docs/http/ngx_http_upstream_module.html#hash)、[代理重试边界](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_next_upstream)。以上文档用于配置依据，实际通过项以本项目测试记录为准。

## 第二批：Prometheus 指标采集（2026-11-15）

### 实现与命名

新增 backend/pkg/observability/metrics，使用官方 client_golang v1.23.2。每个进程独立 Registry，所有自定义指标及 Go/process 采集器带固定 service 标签，不使用全局默认 Registry，不把多个实例的计数混在一起。

| 指标 | 标签 | 语义 |
| --- | --- | --- |
| http_requests_total | service、method、route、status | 完成的普通 HTTP 请求数 |
| http_request_duration_seconds | service、method、route | 普通 HTTP 耗时直方图 |
| http_requests_in_flight | service | 正在处理的普通 HTTP |
| grpc_server_handled_total | service、grpc_service、grpc_method、code | 已注册一元 RPC 的完成次数 |
| grpc_server_handling_seconds | service、grpc_service、grpc_method | 一元 RPC 耗时直方图 |
| grpc_server_requests_in_flight | service | 当前一元 RPC 数 |
| websocket_handshakes_total | service、result | accepted/rejected 握手次数 |
| websocket_handshake_duration_seconds | service、result | 仅握手阶段耗时，含账号验证 |
| websocket_connections | service | 当前 Node 注册的 WS 连接 |
| websocket_connection_duration_seconds | service | 已关闭连接寿命 |
| chat_send_rpc_total | service、code | WS Node 调用发送 RPC 的结果，含重试 |

Gateway 的 HTTP 标签取 Gin FullPath 注册模板，例如 /api/v1/products/:id；404 未注册路径统一 unmatched，任意非标准方法统一 OTHER。route 不读取 URL 或 query，不把 user_id、order_id、商品 ID、Request ID、Token、票据、IP 或正文放进标签。

HTTP /metrics 不自计入 QPS；整个 /api/v1/chat/ws 路径使用独立 WS 指标，不计入普通 HTTP 延迟或活跃数，防止长连接寿命污染 P95/P99。WS 成功 Upgrade 与实际注册在线连接是两个阶段，容量拒绝可能已完成 Upgrade，但不增加在线数。连接关闭钩子幂等，在读写清理后回收 Gauge。

发送指标只统计已经发起并取得结果的 Chat RPC 尝试，不包含 RPC 前的 JSON/参数拒绝；一次消息的幂等重试会增加次数，不能当作唯一落库消息数或接收方已读数。本批没有伪造 Outbox 积压、数据库耗时、Kafka Lag、唯一发送量等尚未采集的业务指标。

六个 gRPC 服务在现有通用拦截器之后、鉴权之前接入指标，因此认证失败也计数。错误通过现有 grpcx.Error 映射成对外 code，未知错误与 panic 计为 Internal；panic 仍由原统一恢复层处理，业务错误语义、身份验证和上游 Deadline 不变。未注册方法与流式 RPC 不经过这一元拦截器，不计入该指标，不能扩写成所有 gRPC 网络流量均已覆盖。

HTTP/gRPC 直方图桶为 5ms、10ms、25ms、50ms、100ms、250ms、500ms、1s、2.5s、5s、10s、30s，加自动 +Inf；分位数是桶估计值。使用已有正常请求的计数计算速率，不把空指标或模拟面板当作真实流量。

### 开关及端口

METRICS_ENABLED 默认 false。Gateway 关闭时不注册 /metrics，其余服务不启动采集监听器，也不创建 Registry。开启后：

| 进程 | 本机采集端点 | 地址配置 |
| --- | --- | --- |
| Gateway 1/2 | http://127.0.0.1:8080/metrics、:8081/metrics | 沿用 GATEWAY_HTTP_ADDR |
| User | http://127.0.0.1:9101/metrics | USER_METRICS_ADDR |
| Product | http://127.0.0.1:9102/metrics | PRODUCT_METRICS_ADDR |
| Order | http://127.0.0.1:9103/metrics | ORDER_METRICS_ADDR |
| Search | http://127.0.0.1:9104/metrics | SEARCH_METRICS_ADDR |
| Feed | http://127.0.0.1:9105/metrics | FEED_METRICS_ADDR |
| Chat | http://127.0.0.1:9106/metrics | CHAT_METRICS_ADDR |

服务在业务依赖组装完成后启动采集监听器，绑定失败会终止启动并释放资源，不伪装成可采集。取消根 Context 或正常退出会有界关闭 HTTP 监听器与后台协程。独立采集 HTTP 仅提供 GET/HEAD /metrics，限制读写时间与 Header 大小；导出限制同时两次采集、处理预算三秒，不连接业务库。

开发默认采集端口只绑定 loopback，不通过 Nginx 统一入口公开，不增加业务 JWT 旁路。本批没有提供采集 TLS 或认证，部署到非本机环境必须另外规划受限网络；Linux 容器对宿主机 loopback 的可达性仍须实测，不能照搬 Docker Desktop 的验收结论。

开启方式是为每个需要采集的进程导出 METRICS_ENABLED=true 后重启，例如：

```sh
METRICS_ENABLED=true make run-user
METRICS_ENABLED=true make run-product
METRICS_ENABLED=true make run-order
METRICS_ENABLED=true make run-search
METRICS_ENABLED=true make run-feed
METRICS_ENABLED=true make run-chat
METRICS_ENABLED=true CHAT_REDIS_ENABLED=true GATEWAY_HTTP_ADDR=127.0.0.1:8080 make run-gateway
METRICS_ENABLED=true CHAT_REDIS_ENABLED=true GATEWAY_HTTP_ADDR=127.0.0.1:8081 make run-gateway
```

以上是不同终端的启动示例，先导出原有环境配置；已有服务占用端口时不能直接启动重复实例。当前运行中的旧二进制不会因文件变化自动启用指标。本批没有改写 .env、替换用户正在运行的服务或开启业务消费者。

### Prometheus 配置与运行

新增独立 deployments/docker-compose.observability.yml，项目名 remarket-observability，镜像 prom/prometheus:v3.5.0。UI 仅发布 127.0.0.1:19090；指标数据保存在该项目命名卷，保留策略七天或 512MB 的块大小限制，容器内存限制 512MB。停止命令不带 -v，避免删除已有指标数据。

prometheus.yml 配置 Gateway 两个地址、六个服务独立端口及 Prometheus 自身；十五秒抓取，三秒超时。job 区分服务类型，instance 区分实例，不覆盖 exporter 的 service 常量标签。

```sh
make observability-config
make prometheus-check
make prometheus-up
make prometheus-logs
# 停止监控项目，保留指标卷：
make prometheus-down
```

启动后访问 http://127.0.0.1:19090/targets。没有启动或没有启用指标的目标会显示 DOWN，这不等于这些业务服务必然已停止；up 只表示最近一次采集是否成功，不是端到端业务健康检查。Gauge、HTTP/gRPC 请求结果需要与 up 联合判断。当前生产配置八个业务采集地址尚未逐一常驻启用，不能称为已达到 8/8 全业务服务验收。

可用于后续 Grafana 的 PromQL：

```promql
# 普通业务 API QPS
sum by (service, route) (rate(http_requests_total{route=~"/api/.*"}[5m]))

# HTTP 5xx 比例
sum(rate(http_requests_total{service="gateway",status=~"5.."}[5m]))
/
clamp_min(sum(rate(http_requests_total{service="gateway"}[5m])), 1e-9)

# Gateway 普通 API P95；P99 将 0.95 替换为 0.99
histogram_quantile(0.95,
  sum by (le) (rate(http_request_duration_seconds_bucket{service="gateway",route=~"/api/.*"}[5m]))
)

# 各服务 RPC P95
histogram_quantile(0.95,
  sum by (service, le) (rate(grpc_server_handling_seconds_bucket[5m]))
)

sum(websocket_connections{service="gateway"})
up{job=~"gateway|user|product|order|search|feed|chat"}
```

没有足够样本时分位数可能为空或 NaN；不填入假延迟。Grafana 数据源、面板与告警留到第三批，当前不宣称已有可用仪表盘。

### 实际验收与边界

已通过：

- 全量 `cd backend && go test ./...`。
- `go test -race ./pkg/observability/metrics ./internal/gateway ./internal/chat/node -count=1`。
- 指标单元与真实内存 gRPC 测试：401/429/panic、业务 InvalidArgument、未知 Internal、认证拒绝、真实取消、活跃 Gauge 回收、Registry 隔离、端口冲突/停止释放、未知 URL/方法路径不增殖标签、采集不混入 QPS。
- 真实 Gin/WebSocket 测试：建立后 Gauge=1，Node 关闭后 Gauge=0，握手成功/拒绝、重复发送 RPC 尝试及错误码计数准确；长期 WS 与采集路径不进入普通 HTTP 直方图，导出中无 JWT、ticket、client_msg_id、实体 ID。
- `make infra-config`、`make edge-config`、`make observability-config`。
- `make prometheus-check`：真实官方镜像 promtool 校验配置通过。
- `PROMETHEUS_INTEGRATION=1 go test -race ./pkg/observability/metrics -run TestPrometheusScrape -count=1 -v`：真实 Prometheus 容器从宿主机抓取隔离 HTTP/gRPC Exporter，查询实际 up=1、HTTP/RPC 计数及数值有效 P95；关闭采集监听器后实际查询 up=0。
- `git diff --check`。

Prometheus 集成使用真实 Gin HTTP→真实健康 RPC，gRPC 在内存传输中运行，没有真实 User/Product 业务数据库请求。证明采集链路与指标数据有效，不等同于所有独立业务进程、业务慢 SQL 或故障演练验收。临时容器与监听器已清理；不修改业务表或 Redis 业务键，不开启常驻 Prometheus，不删除持久化指标卷。第一批未提交改动保留，未创建 Git 提交。

依据：[官方 Go 客户端接入](https://prometheus.io/docs/guides/go-application/)、[标签基数实践](https://prometheus.io/docs/practices/instrumentation/)、[独立 Registry 的 HandlerFor](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus/promhttp#HandlerFor)。
