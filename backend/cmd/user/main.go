// 本进程暴露 gRPC 契约，业务方法当前返回 Unimplemented。
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/pkg/grpcx"
	"syscall"
)

// main 组装并启动 用户服务骨架，监听退出信号并交由 grpcx 执行优雅关闭。
func main() {
	// Ctrl+C 或 SIGTERM 触发根上下文取消，通知服务停止。
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	address := os.Getenv("USER_GRPC_ADDR")
	if address == "" {
		address = "127.0.0.1:50051"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		slog.Error("listen failed", "error", err)
		os.Exit(1)
	}
	// 仅注册协议骨架，不将尚未实现的方法伪装成业务成功响应。
	server := grpcx.NewServer(slog.Default())
	userv1.RegisterUserServiceServer(server, &userv1.UnimplementedUserServiceServer{})
	slog.Info("RPC scaffold listening; business methods are unimplemented", "address", listener.Addr().String())
	if err := grpcx.Serve(ctx, listener, server); err != nil {
		slog.Error("serve failed", "error", err)
		os.Exit(1)
	}
}
