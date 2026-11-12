// Gateway 进程提供 HTTP 入口，通过 gRPC 访问 User Service。
package main

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	productv1 "remarket/backend/gen/product/v1"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/internal/gateway"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/grpcx"
	"syscall"
	"time"
)

// main 运行 Gateway，异常退出时不记录请求凭据。
func main() {
	if err := run(); err != nil {
		slog.Error("Gateway 运行失败", "error", err)
		os.Exit(1)
	}
}

// run 组装 gRPC Client 和 HTTP 服务，退出时关闭监听器及连接。
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	address := os.Getenv("GATEWAY_HTTP_ADDR")
	if address == "" {
		address = "127.0.0.1:8080"
	}
	target := os.Getenv("USER_GRPC_TARGET")
	if target == "" {
		target = "127.0.0.1:50051"
	}
	timeout := 3 * time.Second
	if value := os.Getenv("GATEWAY_RPC_TIMEOUT"); value != "" {
		var err error
		timeout, err = time.ParseDuration(value)
		if err != nil || timeout <= 0 || timeout > 30*time.Second {
			return errors.New("Gateway RPC 超时必须大于零且不超过 30 秒")
		}
	}
	// 与 User Service 使用相同 JWT 配置，但双方独立验证签名及声明。
	jwtConfig, err := auth.JWTConfigFromEnv()
	if err != nil {
		return err
	}
	tokens, err := auth.NewJWTManager(jwtConfig)
	if err != nil {
		return err
	}
	conn, err := grpcx.Dial(target, nil, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	productTarget := os.Getenv("PRODUCT_GRPC_TARGET")
	if productTarget == "" {
		productTarget = "127.0.0.1:50052"
	}
	productConn, err := grpcx.Dial(productTarget, nil, timeout)
	if err != nil {
		return err
	}
	defer productConn.Close()
	gin.SetMode(gin.ReleaseMode)
	router := gateway.NewAuthenticatedRouter(userv1.NewUserServiceClient(conn), userv1.NewUserServiceClient(conn), tokens, timeout)
	gateway.AddProductRoutes(router, productv1.NewProductServiceClient(productConn), tokens, timeout)
	server := &http.Server{Addr: address, Handler: router, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	slog.Info("Gateway 已启动", "address", listener.Addr().String())
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	// 给予在途 HTTP 请求五秒完成，超时后关闭连接。
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdown); err != nil {
		_ = server.Close()
		return err
	}
	return nil
}
