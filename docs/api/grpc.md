# User 与 Product gRPC 基础

已定义 User、Product v1 契约并生成 Go 代码。Order、Feed 契约与商品预占 RPC 按当前范围延后。

## 接口

User：Register、Login、GetUser、UpdateProfile。返回的 User 不含密码哈希。UpdateProfile 以认证后的调用者为目标，不接受任意用户 ID。

Product：CreateProduct、GetProduct、ListProducts、UpdateProduct、PublishProduct、OffShelfProduct、ListCategories。创建默认草稿；修改与状态转换携带 expected_version；没有任意状态修改 RPC。状态枚举与数据库五种状态对应，UNSPECIFIED 不可写入数据库。列表约定 page 默认 1、page_size 默认 20、最大 100，只展示在售商品；这些规则需后续业务实现校验。

部分更新使用 proto3 optional 区分未设置与空值。金额、ID、版本均使用 int64。时间使用 protobuf Timestamp；JSON 转换使用 protojson，int64 将输出字符串。

## 生成与启动

生成使用 protoc 35.1、protoc-gen-go 1.36.11、protoc-gen-go-grpc 1.6.2。插件需在 PATH 中，可通过以下方式安装固定版本：

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
make proto
```

生成代码提交在 `backend/gen/`，不要手工编辑。执行 `make run-user`、`make run-product` 分别启动服务。默认监听 `127.0.0.1:50051`、`127.0.0.1:50052`，通过 `USER_GRPC_ADDR`、`PRODUCT_GRPC_ADDR` 配置。

Go 进程读取已导出的环境变量，不会自动加载 `.env`。例如执行 `USER_GRPC_ADDR=127.0.0.1:51051 make run-user`。

当前服务注册生成的 Unimplemented 实现，所有业务 RPC 返回 Unimplemented；尚未实现注册登录、JWT、数据库读写及商品事务。标准 gRPC health 仅用于进程健康，未将业务服务标记为 SERVING。支持信号取消和最多五秒的优雅关闭。

## Client 与 Context

```go
conn, err := grpcx.Dial("127.0.0.1:50051", nil, 3*time.Second)
if err != nil { return err }
defer conn.Close()
client := userv1.NewUserServiceClient(conn)
ctx := grpcx.WithRequestID(parentContext, "request-123")
response, err := client.GetUser(ctx, &userv1.GetUserRequest{Id: 1})
```

连接采用惰性连接语义，Dial 成功不代表服务可达。nil credentials 表示仅本地开发使用的明文连接；部署环境通过参数传入 TLS credentials。当前服务端示例为明文，不应直接公开。

客户端默认施加调用超时（由 Dial timeout 参数指定），服务端上限五秒；上游更短的 Deadline 不被延长。取消与超时使用原 Context 传递，不替换为 Background。暂不自动重试写请求。

Request ID 经校验后在服务端 Context 和响应 metadata 中提供；客户端拦截器传递 Request ID，以及 authorization、traceparent、tracestate。Trace 目前仅传递头，尚未创建 OTel span。认证凭据传递不等于认证：不信任 x-user-id/x-role，未来业务必须校验 JWT 并建立可信身份上下文。日志不记录请求正文及认证凭据。

## 错误映射与测试

业务错误定义在 `pkg/apperror`，不依赖 gRPC。统一映射为 InvalidArgument、NotFound、AlreadyExists、Unauthenticated、PermissionDenied、FailedPrecondition、Unavailable。已存在的 gRPC 错误保留；取消/超时映射对应状态；未知错误和 panic 返回通用 Internal，不向调用方暴露内部信息。HTTP 映射待 Gateway 实现。

`pkg/grpcx` 使用真实 gRPC 协议和 bufconn 测试服务注册、请求及响应 Request ID、认证与 Trace metadata、拒绝隐式传递身份头、Deadline、错误映射与 panic 恢复。运行 `cd backend && go test ./...`。
