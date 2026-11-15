// feed 提供独立的首页与关注商品流查询进程。
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	feedv1 "remarket/backend/gen/feed/v1"
	productv1 "remarket/backend/gen/product/v1"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/internal/feed/handler"
	"remarket/backend/internal/feed/service"
	"remarket/backend/internal/feed/timeline"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/grpcx"
	"syscall"
	"time"
)

// main 启动 Feed，失败时不输出凭据。
func main() {
	if err := run(); err != nil {
		slog.Error("Feed 服务退出", "error", err)
		os.Exit(1)
	}
}

// env 使用明确配置或本地开发默认值。
func env(key, value string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return value
}

// run 组装 RPC 依赖与独立验签组件，退出时释放连接和监听器。
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	config, err := auth.JWTConfigFromEnv()
	if err != nil {
		return err
	}
	tokens, err := auth.NewJWTManager(config)
	if err != nil {
		return err
	}
	users, err := grpcx.Dial(env("USER_GRPC_TARGET", "127.0.0.1:50051"), nil, 3*time.Second)
	if err != nil {
		return err
	}
	defer users.Close()
	products, err := grpcx.Dial(env("PRODUCT_GRPC_TARGET", "127.0.0.1:50052"), nil, 3*time.Second)
	if err != nil {
		return err
	}
	defer products.Close()
	listener, err := net.Listen("tcp", env("FEED_GRPC_ADDR", "127.0.0.1:50055"))
	if err != nil {
		return err
	}
	defer listener.Close()
	server := grpcx.NewServer(slog.Default(), grpcx.OptionalAuthInterceptor(tokens, []string{feedv1.FeedService_Home_FullMethodName}, "/grpc.health.v1.Health/Check"))
	query := service.New(userv1.NewUserServiceClient(users), productv1.NewProductServiceClient(products))
	cacheConfig, err := timeline.LoadConfig()
	if err != nil {
		return err
	}
	if cacheConfig.Enabled {
		store, client, e := timeline.OpenLazy(cacheConfig)
		if e != nil {
			return e
		}
		defer client.Close()
		query.WithTimeline(store)
	}
	feedv1.RegisterFeedServiceServer(server, handler.New(query))
	return grpcx.Serve(ctx, listener, server)
}
