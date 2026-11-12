// 本进程提供注册及登录 RPC，其他用户业务方法暂未实现。
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/internal/user/handler"
	"remarket/backend/internal/user/repository"
	"remarket/backend/internal/user/service"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/database"
	"remarket/backend/pkg/grpcx"
	"syscall"
	"time"
)

// main 组装并启动 用户服务骨架，监听退出信号并交由 grpcx 执行优雅关闭。
func main() {
	if err := run(); err != nil {
		slog.Error("用户服务运行失败", "error", err)
		os.Exit(1)
	}
}

// run 组装数据库与注册调用链，返回前通过 defer 关闭连接池及监听器。
func run() error {
	// Ctrl+C 或 SIGTERM 触发根上下文取消，通知服务停止。
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// JWT 配置无效时启动失败，禁止使用默认密钥签发令牌。
	jwtConfig, err := auth.JWTConfigFromEnv()
	if err != nil {
		return err
	}
	tokens, err := auth.NewJWTManager(jwtConfig)
	if err != nil {
		return err
	}
	// 连接配置来自已导出的环境变量，不自动加载 .env，不自动建表。
	dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
	db, pool, err := database.OpenMySQL(dbCtx, database.MySQLConfig{Address: os.Getenv("USER_MYSQL_ADDR"), Username: os.Getenv("USER_MYSQL_USERNAME"), Password: os.Getenv("MYSQL_USER_PASSWORD"), Database: os.Getenv("USER_MYSQL_DATABASE")})
	dbCancel()
	if err != nil {
		return err
	}
	defer pool.Close()
	address := os.Getenv("USER_GRPC_ADDR")
	if address == "" {
		address = "127.0.0.1:50051"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	// 依赖方向为 Handler → Service → Repository，接入 Register 和 Login 方法。
	server := grpcx.NewServer(slog.Default())
	users := repository.NewUserRepository(db)
	login, err := service.NewLogin(users, tokens)
	if err != nil {
		return err
	}
	userv1.RegisterUserServiceServer(server, handler.NewWithLogin(service.New(users), login))
	slog.Info("用户服务已启动，注册及登录 RPC 已接入", "address", listener.Addr().String())
	if err := grpcx.Serve(ctx, listener, server); err != nil {
		return err
	}
	return nil
}
