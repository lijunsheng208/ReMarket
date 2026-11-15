// feed 提供独立的首页与关注商品流查询进程。
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	chatv1 "remarket/backend/gen/chat/v1"
	productv1 "remarket/backend/gen/product/v1"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/internal/chat/handler"
	"remarket/backend/internal/chat/repository"
	"remarket/backend/internal/chat/service"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/database"
	"remarket/backend/pkg/grpcx"
	"syscall"
	"time"
)

// main 启动 Chat，失败时不输出凭据。
func main() {
	if err := run(); err != nil {
		slog.Error("Chat 服务退出", "error", err)
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
	listener, err := net.Listen("tcp", env("CHAT_GRPC_ADDR", "127.0.0.1:50056"))
	if err != nil {
		return err
	}
	defer listener.Close()
	server := grpcx.NewServer(slog.Default(), grpcx.AuthInterceptor(tokens, "/grpc.health.v1.Health/Check"))
	dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
	db, pool, err := database.OpenMySQL(dbCtx, database.MySQLConfig{Address: os.Getenv("CHAT_MYSQL_ADDR"), Username: os.Getenv("CHAT_MYSQL_USERNAME"), Password: os.Getenv("MYSQL_CHAT_PASSWORD"), Database: os.Getenv("CHAT_MYSQL_DATABASE")})
	dbCancel()
	if err != nil {
		return err
	}
	defer pool.Close()
	query := service.New(repository.New(db), userv1.NewUserServiceClient(users), productv1.NewProductServiceClient(products))
	chatv1.RegisterChatServiceServer(server, handler.New(query))
	return grpcx.Serve(ctx, listener, server)
}
