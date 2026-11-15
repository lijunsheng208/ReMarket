# Day 4 搜索契约与设计（第 1 步，待 review）

## 范围与状态

本步新增 Search Proto、生成代码及本设计，不启动 Search 进程、不注册 Gateway 路由、不创建 ES 索引。契约可编译不代表业务实现或联调通过。关注、Feed、图片仍为 Day 4 核心功能，搜索先独立推进；不新增 Order 或 Feed 契约及商品预占能力。

链路：HTTP Gateway → Search Handler → Service → ES Repository。Product MySQL 是事实来源，ES 是可重建视图。Search 不读取 Product 业务表，不共享 GORM 模型。现有商品事件包含完整搜索字段和 aggregate_version，可复用 Kafka 总线；实际索引消费仍待实现。

## 查询接口

拟新增匿名 `GET /api/v1/search/products`，对应 `remarket.search.v1.SearchService/SearchProducts`。HTTP 查询字段与 Proto 相同，sort 使用 `relevance`、`newest`、`price_asc`、`price_desc`，缺省由 Service 决定；非法枚举值拒绝。

| 参数 | 规则 |
|---|---|
| query | 合法 UTF-8，去首尾空白，最多 100 个 Unicode 字符；允许为空 |
| category_id | 可缺省；存在时为正 int64，仅精确匹配分类，不隐含子分类展开 |
| min_price_cent / max_price_cent | 可缺省；存在时为非负 int64，闭区间且 min ≤ max |
| sort | 有关键词默认 relevance，否则 newest；空关键词显式 relevance 拒绝 |
| page_size | 缺省或 0 为 20，允许 1～100，其余拒绝 |
| cursor | 首次为空；后续使用不透明游标，最大 16 KiB |

不支持用户传 ES DSL、正则、通配符、模糊查询、页码及任意排序字段。未知或重复 HTTP 查询参数拒绝。HTTP 数字参数仅接受十进制整数并检查 int64 溢出；空值不是缺省。不存在的分类返回空结果。

响应使用现有 Gateway protojson snake_case 约定，ID、金额、版本输出十进制字符串，时间为 Timestamp JSON；空 products/next_cursor 可能按现有 protojson 默认行为省略，前端按空数组/结束处理。不返回精确 total、评分、内部索引名或 PIT ID 独立字段。

示例：`GET /api/v1/search/products?query=iPhone&min_price_cent=10000&sort=price_asc&page_size=20`。

```json
{"products":[{"id":"42","seller_id":"7","category_id":"2","title":"iPhone 旧机","description":"正常使用","price_cent":"12000","aggregate_version":"3","first_published_at":"2026-11-14T02:00:00Z"}],"next_cursor":"<opaque-token>"}
```

## 索引与检索规则

初始物理索引 `products_v1`，读别名 `products_search`；消费者写入明确的物理目标，重建不能通过切换别名意外改变消费目标。Mapping 使用 dynamic=strict；实际 JSON Mapping 在第 2 步落地。

| 字段 | ES 类型与用途 |
|---|---|
| product_id / seller_id / category_id | long，product_id 同时用于排序，_id 为商品 ID 十进制字符串 |
| title / description | text，内置 standard analyzer；title 权重 3，description 权重 1 |
| price_cent / aggregate_version | long，过滤/排序及审计版本 |
| status | keyword，强制过滤 ON_SALE |
| first_published_at | date_nanos，保留源事件 UTC 微秒精度 |

内置 standard 无额外插件依赖；中文按其内置分词行为检索，不承诺中文词语识别、拼音或同义词。使用 multi_match best_fields、operator=and，用户关键词作为纯文本。中文相关性需以“苹果手机”“二手自行车”等真实样本验收；若不满足再单独 review analyzer 变更及重建。首版不引入 IK 插件。

排序：relevance 为 `_score DESC, product_id DESC`；newest 为 `first_published_at DESC, product_id DESC`；价格为 `price_cent ASC/DESC, product_id DESC`。PIT 自动附加的 `_shard_doc` 也完整保存在游标中。

## 游标与一致性

使用 PIT + search_after，首次打开 PIT，单次 keep_alive 60 秒，游标绝对寿命最多 5 分钟；空闲超过 60 秒 PIT 可能提前失效，5 分钟不是可用性保证。每次请求使用最新 PIT ID，查询多取 1 条判断是否还有下一页，游标取最后一条实际返回结果的 sort 值。结束或首次失败后尽力关闭 PIT；用户放弃的 PIT 靠超时释放。限制并发 PIT 和请求速率，具体配置及资源测试在查询实现阶段落地。

游标是版本化 JSON payload 的 base64url 加 HMAC-SHA256 签名，包含 PIT ID、完整排序值、查询参数摘要、签发时间和固定过期时间。服务端专用密钥通过环境配置，多实例共享；密钥缺失时拒绝启动，不使用示例默认值。验证签名、长度、结构、有效期及查询摘要后使用，不能把客户端数据直接当 ES 请求。后续 query、筛选、规范化 sort 和 page_size 必须与首请求一致。数字排序值保留精确整数，禁止经 float64 丢失 BIGINT 精度。游标无保密保证，日志不记录游标或 PIT ID。

PIT 保证同一搜索会话的快照分页，更新不会立即改变该快照；新搜索也存在 Kafka 与 refresh 延迟。因此下架、预占或售出后，旧 PIT 仍可能短暂展示旧商品，最长受会话寿命限制。点击详情和交易必须重新由 Product 校验，搜索不承诺强一致可购买性。索引切换时已有 PIT 仍读旧快照。

## 事件同步与失败边界（第 3 步实现）

独立消费组 `remarket-search-v1` 订阅现有商品事件主 Topic，具体 Topic 复用现有部署配置。支持 published、updated、off_shelf、reserved、released、sold 的 v1 事件；校验聚合类型、Kafka Key、版本、字段及状态一致性。草稿不进入搜索。字段以事件快照为准，不查询其他服务数据库。

使用 aggregate_version 作为 ES external version，严格较大版本才写入；重复或旧版本冲突可视为已处理，其他冲突不得笼统吞掉。非 ON_SALE 保留完整文档与版本并过滤，不物理删除，避免删除版本墓碑过期后旧发布事件复活商品。同版本内容必须由生产端保证不可变。

ES 成功写入（或确认旧版本冲突）后才能提交 Offset；暂时故障重试，取消不提交。重试耗尽和非法消息进入 Search 独立 retry/DLT 链路，转移获 Kafka ACK 后才能提交原 Offset；失败阻塞并告警。具体重试存储与配置在第 3 步 review，不直接复用 eventtest 测试业务存储。bulk 要逐项判断，不能仅以 HTTP 成功当作整批成功；refresh 不逐条强制，验收等待最终可见。

## 全量重建（第 5 步实现）

Product 需新增仅服务身份可调用的搜索快照导出接口，按 ID 游标分批导出所有曾发布商品，包含当前状态及版本；当前 ListProducts 只含在售且为页码分页，不适合重建。此 RPC 本步不提前新增。Search 运维命令使用独立凭据调用，不直接读 Product 表，也不暴露公共 HTTP 重建接口。

初版采用维护窗口：暂停所有商品写入（包括 Order 触发的预占、释放、确认和后台任务），并等待在途事务、Outbox 与消费积压排空，同时核查 retry/DLT 中未应用的有效事件：未确认已被快照覆盖前不得切换，后续重放仍须版本保护；记录各分区边界；创建新物理索引并通过 Product 导出快照。校验数量、状态与抽样内容后，暂停消费者并将明确写目标改为新索引，原子切换读别名，恢复消费者及商品写入。任何中途失败保持写入暂停，先恢复旧目标与旧别名再解除维护；切换后已有新写入时不可直接回滚旧索引，必须先补齐增量。此方案有写维护时间，不宣称零停机；双写在线重建另行设计。

旧索引至少保留到所有旧 PIT 过期并完成观察，不自动删除。事件保留期不足时必须依赖快照重建，不能仅承诺从 Kafka 回放恢复。

## 错误、安全与验证计划

参数、篡改或不匹配游标：InvalidArgument / HTTP 400；合法但过期或 PIT 丢失：FailedPrecondition / 409，前端重新搜索；ES 不可用或分片不完整：Unavailable / 503；超时：DeadlineExceeded / 504；取消沿用现有 499 映射；未知故障 Internal / 500。拒绝返回 ES 部分结果或把故障伪装成空列表。索引缺失为服务不可用。

Handler 只适配协议；业务校验位于 Service；ES 请求由 Repository 构造。超时从上游 Context 派生，传递 Request ID，不信任 x-user-id/x-role，不透传 ES 内部响应、凭据或连接信息。Search 尚无运行服务；后续未实现方法使用生成的 Unimplemented，不伪造成功。

后续检查覆盖：筛选边界和溢出、中文/英文匹配、排序同值分页、超大 ID、游标篡改和过期、PIT 释放、乱序和重复、不可售旧事件回放、部分 bulk 失败与 Offset、重建失败恢复。真实链路验收为发布→可搜、编辑→更新、下架/预占/售出→新搜索不可见、释放/重新上架→恢复可见；单元测试与真实 ES/Kafka 验收分别记录。

依据：[ES 8.19 分页](https://www.elastic.co/guide/en/elasticsearch/reference/8.19/paginate-search-results.html)、[external version](https://www.elastic.co/guide/en/elasticsearch/reference/8.19/docs-index_.html)。以上是方案选择，尚无运行验收结果。

## 本步验证记录

2026-11-14：`make proto`、`cd backend && go test ./...`、`git diff --check` 通过。首次测试因沙箱无法访问 Go 构建缓存失败，获准在沙箱外重跑后通过。未启动或验收 ES/Kafka；集成测试通过环境开关启用的部分不代表本次实际执行。


## 第 2 步：ES 基础访问与索引管理

代码位于 `backend/internal/search/repository/`：使用标准库 net/http 访问 ES 8 REST API，暂不引入额外 SDK。连接参数通过 Config 显式传入；环境变量及运行入口留待后续接线。New 不联网、不建索引；CreateIndex 显式创建物理索引，已存在返回 ErrExists，不覆盖、不删除、不自动切换别名。固定 Mapping 使用 1 分片、0 副本，仅面向本地单节点；生产拓扑另行配置。

Put 写入完整 Search 自有 Document，使用 external 严格版本，返回 true 表示已确认写入，false/nil 表示明确的版本冲突。只有 version_conflict_engine_exception 可作为旧事件处理；其他 HTTP 错误、损坏成功响应、超大响应均返回通用存储错误，不泄露 ES reason。请求从上游派生超时，关闭响应 Body，禁用重定向，退出需调用 Close。HTTPS 使用系统 CA 验证，不提供跳过证书验证开关；Basic Auth 仅允许 HTTPS。

此阶段不提供自动建索引回退、自动请求重试、别名切换、bulk、环境变量读取及进程入口。调用方须先显式创建索引；部署时应通过 ES 权限禁止消费账号 auto_configure/create_index，避免写错目标时自动创建默认 Mapping。现有本地 ES 关闭安全认证，尚不具备该权限隔离保证；运维及真实环境验证待后续完成。此时也未实现 ES 错误到业务/gRPC 的映射。

2026-11-14：新增 httptest 协议测试覆盖固定 Mapping、建索引部分确认、重复创建、BIGINT 精度、external 版本、旧事件及其他冲突、无效成功响应、地址校验和上游取消。真实 ES 建索引和版本行为未验收。

第 2 步验证结果：`gofmt`、`cd backend && go test ./...`、`git diff --check` 通过。首次新增 HTTP 测试因沙箱禁止监听临时端口失败，获准沙箱外重跑后通过；未连接真实 Elasticsearch。

## 第 3 步：Search 商品事件消费者

新增 `backend/internal/search/consumer/` 与 `backend/cmd/search-consumer/`。不复用 eventtest 业务存储；通过 Search 自有 Document 和 Indexer 接口同步 published/updated/off_shelf/reserved/released/sold 六类事件，校验 Key、商品 ID、状态、时间和版本。非在售事件仍写完整快照保留版本；重复或乱序交给 ES external version 处理。

运行组固定 `remarket-search-v1`，主 Topic 使用 `KAFKA_PRODUCT_TOPIC`（默认现有商品主 Topic）；独立失败 Topic 固定为 `remarket.search.product.retry.v1` 和 `remarket.search.product.dlt.v1`，已加入 Kafka 初始化脚本，需显式执行 `make kafka-topics` 补建。本步没有执行该命令或操作容器。

重试使用自有 Packet 保存原始 Key/Value、源分区/Offset、次数与就绪时间；首次 ES 失败转 retry，最多 3 次重试，每次退避 1 秒，耗尽进入 DLT。损坏事件直接进入 DLT，损坏 retry 包以当前消息为原始载荷隔离。只有 ES 已确认或旧版本、转移获得 Broker ACK 时 Handle 才返回成功。公共 Kafka Consumer 禁用自动提交、逐条处理；失败立即退出，不跨越未处理记录。入口等待 1 秒后重建消费者；取消不继续提交。ES 内部操作超时可进入重试，上游处理预算取消则保留原 Offset。

这里没有跨 ES/Kafka 的分布式事务或持久化 transfer 决定：ACK 后提交前崩溃可能重复转移，重试包和死信也可能重复；ES 版本保证商品视图效果幂等，不保证死信记录唯一。每条包的尝试次数有界，重复转移可能增加总处理次数。重放后仍须依赖版本保护。DLT 转移故障会阻塞原记录并由入口输出稳定告警分类，不吞掉失败。无自动 DLT 重放或自动清理。超出 Broker 消息大小限制的隔离包可能无法转移，此时保持阻塞，由运维处理，不能跳过 Offset。

启动前应显式创建并检查 `products_v1` 的 Mapping（第 2 步 API，不自动创建）。启动执行 HEAD 检查目标存在；这不替代运行期间禁止自动建索引的权限控制。入口配置 `ELASTICSEARCH_URL`（默认 http://127.0.0.1:9200）、`SEARCH_WRITE_INDEX`（默认 products_v1）、可选 `ELASTICSEARCH_USERNAME/PASSWORD`，Kafka 沿用现有环境配置；Go 不自动加载 .env。

```sh
# 导出已核验的环境变量并准备索引和 Topic 后启动
make run-search-consumer
# 操作员从 Search DLT 导出 Packet，修复原始契约后显式重放
cd backend && go run ./cmd/search-consumer replay /path/to/packet.json
```

重放读取单个不超过 512 KiB 的 Packet JSON，验证原事件和来源，将尝试次数重置为 1 发到 Search retry 并等待 ACK。正常 DLT 包可直接重放；invalid_retry_packet 隔离包需要先人工提取并修复内层原事件，不能自动多层解包。此入口需要 Kafka 写权限和 ES 可用索引，仅在受控运维环境使用。消费成功不代表 ES 已 refresh，真实验收需等待可见。

新增模拟测试覆盖六种状态、重复/旧版本、Key 及状态不匹配、重试次数、转移 ACK 失败、取消、坏包与重放。仍未进行真实 Kafka/ES 联调或进程重启故障验收；不据此声称步骤 6 集成验收完成。

第 3 步验证记录（2026-11-14）：`gofmt`、`cd backend && go test ./...`、`bash -n deployments/kafka/init-topics.sh`、`make infra-config`、`git diff --check` 通过。Go 首次因沙箱缓存权限失败，获准在沙箱外重跑通过。Compose 仅校验配置，未启动容器或创建 Topic。

## 第 4 步：查询、Search Service 与 Gateway

已实现 `GET /api/v1/search/products` → Search gRPC → Service → ES Repository。查询支持关键词、分类、价格闭区间和四类排序；参数规范沿用前述契约。Gateway 严格解析原始 query string，拒绝重复、未知、空数字、非十进制数字和整数溢出，业务范围仍在 Service 校验。匿名搜索不依赖身份头；不转发用户身份凭据。int64 响应继续使用 protojson 字符串。

ES 查询固定使用读别名 `products_search`，筛选 ON_SALE，title^3/description AND multi_match，三项排序显式包含 _shard_doc。首请求打开 PIT，后续使用返回的最新 PIT ID 和最后一条实际返回结果的完整 sort 数组；多取一条判断后续页，不返回 total。sort 使用 json.RawMessage 保持大整数精度；新版响应上限为 4 MiB，适配最多 100 个带描述的结果，超限仍拒绝。ES timed_out、失败分片、不完整确认或损坏结果返回存储不可用，不返回部分成功。

游标 HMAC-SHA256 验签，绑定规范化 query/filter/sort/page_size，绝对寿命 300 秒；ES 空闲寿命 60 秒。密钥至少 32 字节，通过 `SEARCH_CURSOR_SECRET` 传入，无默认密钥。结果结束或查询失败时沿用原 Context 尽力关闭 PIT；取消后清理可能失败，依赖 ES TTL 回收。会话绝对过期时拒绝翻页，不延长会话。更换密钥会使旧游标无效，多实例须共享密钥。

首版单实例限制最多 10 个同时执行查询及 100 个本实例打开的活跃会话；额度占位与 PIT ID 更新由锁保护，结束释放，过期懒清理。该额度是应用保护，不是 ES 全局 PIT 硬上限：重启丢失计数、跨实例翻页、游标重复使用及清理失败都需依赖 ES TTL/集群配额。Gateway 现有可选限流中间件覆盖搜索路由；生产需显式启用并配置规则。尚未进行资源压测，不宣称全局或分布式配额。

PIT 缺失（search_context_missing_exception/resource_not_found_exception）映射 FailedPrecondition/409；索引缺失、ES 其他 HTTP 错误或部分结果映射 Unavailable/503；无效参数或游标 400；超时 504、取消 499；未知错误由公共拦截器屏蔽为 Internal/500。游标错误与签名密钥、ES 内部响应均不写入日志。

### 启动与索引前提

`make run-search` 启动 Search，`SEARCH_GRPC_ADDR` 默认 `127.0.0.1:50054`；Gateway 的 `SEARCH_GRPC_TARGET` 默认相同。ES 连接使用 `ELASTICSEARCH_URL`（默认 `http://127.0.0.1:9200`）、可选 `ELASTICSEARCH_USERNAME/PASSWORD`。物理写目标 `SEARCH_WRITE_INDEX` 默认 products_v1，查询不通过该设置改变读别名。明文连接仅用于本地开发；当前 gRPC 进程未接入 TLS。

首次运维初始化须先用第 2 步固定 index.json 显式创建 products_v1，再确认别名尚不存在且目标正确，显式绑定 products_search（此处为操作示例，本步未执行）：

```http
PUT /products_v1
<backend/internal/search/repository/index.json 的内容>

POST /_aliases
{"actions":[{"add":{"index":"products_v1","alias":"products_search"}}]}
```

不要将该 add 操作当成已有别名的重建切换命令，避免别名指向多个版本；重建及切换仍留第 5 步。查询进程不自动创建或绑定索引；未准备读别名时搜索返回 503。进程健康只表示存活，不表示 ES 已就绪。导出专用签名密钥及连接环境变量后分别启动 Search 与 Gateway。前端保持现有行为，本步不接入 UI。

新增 Service 模拟测试及 ES HTTP 协议、Gateway 测试，覆盖分页 sort 精度、PIT ID 更新、游标篡改/过期/参数变化、业务校验、资源关闭、部分结果、HTTP 参数和错误映射。真实搜索相关性、索引 refresh、跨进程请求与 ES/Kafka 全链路仍待实际验收。

第 4 步验证记录（2026-11-14）：`gofmt`、`cd backend && go test ./...`、`go test -race ./internal/search/... ./internal/gateway`、`git diff --check` 通过。首次全量测试遭遇沙箱 Go 缓存访问限制，获准在沙箱外完成验证。未启动 Search/Gateway 进程，未操作真实 ES 或读别名；模拟测试通过与真实验收分开记录。

## 第 5 步：维护窗口全量重建与恢复

新增内部 Product RPC `ExportSearchSnapshot`，由 Product Handler → SearchExporter Service → Product Repository 查询本库，按 ID 递增、固定 upper_id 分页，每批最多 100、默认 50；只导出 first_published_at 非空商品，保留在售/预占/售出/下架快照及当前版本，不读取其他服务库。Search 不共享 Product GORM 模型。RPC 无 HTTP 路由。

独立凭据 `SEARCH_PRODUCT_EXPORT_TOKEN` 至少 32 字节、无空白，不得与 JWT Secret 或 Order 服务 Token 相同；Product 和重建工具配置相同值。普通用户 JWT、伪造身份头及 Order Token 不授予导出权限。凭据为空关闭导出。导出还要求 Product 启动时 `PRODUCT_WRITE_MAINTENANCE=true`；该拦截器在用户/Order 认证前暂停商品写 RPC，包含预占、释放、确认，保留读取与健康检查。维护模式只保护当前进程 RPC，不替代全部副本切换及外部任务排空。维护中 Order 可能重试，需同步暂停 Order 写入口与过期任务，恢复后继续推进，不能按墙钟到期自行认定已释放。

新增运维命令 `backend/cmd/search-rebuild`，分两阶段显式运行：

```sh
# 以下均在 backend 目录，先按操作规程进入维护窗口，导出环境变量
# 构建全新索引；已有目标直接失败，不清空、不覆盖、不自动删除
 go run ./cmd/search-rebuild -target products_v2 -maintenance-confirmed
# 重新导出、逐条校验完整字段/版本、刷新核对总数后，才切换读别名
 go run ./cmd/search-rebuild -target products_v2 -switch-from products_v1 -maintenance-confirmed
```

重建工具沿用 ELASTICSEARCH_URL、可选用户名密码，PRODUCT_GRPC_TARGET 默认 127.0.0.1:50052。导出凭据仅通过 metadata 传到 Product，不放入命令行。构建逐条写入并实时 GET 核对全部字段和 _version；保留不可售文档。切换阶段不写商品，重新扫描 Product，对每条目标文档和目标总数重新核对，防止构建后遗漏或混入数据。任何失败立即停止；未完成校验绝不切换。

### 必须串行执行的操作规程

1. 阻止新的商品/订单写入并停止会修改商品的后台执行器，将所有 Product 副本重启至写维护模式，等待旧副本在途请求和本地事务退出。只有维护态 Product 可导出。
2. 等待 Outbox 处理完成，核验商品主 Topic 各分区消费边界；核查 Search retry/DLT 中尚未应用的有效事件，确认已处理或将由维护快照覆盖并记录后续重放计划。停止所有 Search 消费实例。不要通过跳 Offset 假装排空。
3. 同一运维负责人独占执行构建；目标必须全新且不是当前读/写目标。构建失败保留目标供诊断，下一次用新版本名重建。当前工具不提供断点续跑或自动删索引。
4. 构建完成后，在消费者仍停止、商品仍维护期间，将所有 Search 消费进程的 SEARCH_WRITE_INDEX 配置准备为新目标，但暂不启动。执行第二阶段重新核验与别名切换。切换仅接受 products_search 当前唯一指向指定旧索引；使用原子 remove(must_exist=true)+add，不删除旧索引。别名读取检查与写入之间不是 CAS，必须禁止其他操作者并发改别名。
5. 确认别名真实指向新目标，再启动配置了新物理写目标的消费者，核查事件消费恢复后解除 Product 维护并恢复 Order/后台写入。重新搜索核验新结果；旧 PIT 可继续读旧索引，至少等最大 5 分钟会话寿命和观察期结束，不自动删除旧索引。

维护声明参数是操作员确认，不自动证明 Outbox、Kafka、retry/DLT 已排空，也不会自动控制其他进程。固定 ID 上界不是跨请求数据库快照；正确性依赖整个导出和切换期间持续暂停所有写入。本工具未实现在线双写重建或自动化维护编排。

### 失败恢复

- 构建/校验失败且未切换：保留旧别名与旧消费配置；维持维护状态，确认旧链路仍完整后可恢复旧消费者和业务，或用新目标重建。
- 别名切换请求超时/ACK 丢失：结果未知，先 GET /_alias/products_search 核查，不自动重试反向切换。保持商品维护与消费者停止直到明确读写目标一致。
- 已切换但未恢复新写入：可将旧索引作为 target、新索引作为 switch-from，再执行同一校验后切换命令；旧内容不一致则拒绝回滚，先修复。工具不会未经校验直接回切。
- 恢复新写入后：不能直接回旧索引。先重新进入维护并排空事件，再以当前 Product 快照构建另一个新索引、验证并切换，或明确补齐旧索引后重新验证。不得把旧快照当成当前事实。

依据：[ES 原子别名操作](https://www.elastic.co/guide/en/elasticsearch/reference/8.19/aliases.html)。本步没有执行维护、创建真实索引、改别名或暂停用户现有服务。

第 5 步验证记录（2026-11-14）：`make proto`、`gofmt`、`cd backend && go test ./...`、`git diff --check` 通过。新增测试覆盖维护写阻断、导出独立凭据、快照分页、不可售快照保留、构建/切换分离、校验失败不切换及别名原子动作。首次全量测试因沙箱 Go 缓存权限失败，获准重跑通过。未连接真实数据库或执行真实维护重建，MySQL 扫描、跨进程鉴权、Kafka 排空、ES 重建/切换及故障恢复仍需集成验收。

## 第 6 步：前端与真实环境增量

商城已接入服务器搜索，包含关键词、分类、元价格边界、排序、游标下一页、返回第一页、新快照刷新及错误提示；商品管理沿用原接口。Search 三个运行命令现在优先 ELASTICSEARCH_URL，缺省读取 ELASTICSEARCH_PORT 后拼接本地 URL；启动前识别 Elasticsearch 8，误连 OpenSearch 会拒绝。SEARCH_CONSUMER_GROUP 可覆盖默认组以隔离验收。

2026-11-14 已完成真实发布/修改/下架/重上架、订单预占/释放/售出到 ES 的同步及查询验收，详见 [实测记录及限制](../testing/search-acceptance.md)。前端 build、lint、现有订单 API 测试及后端完整测试通过；浏览器宿主不可用，UI 点击与视觉验收仍待完成。未完成真实故障注入或重建回滚验收。
