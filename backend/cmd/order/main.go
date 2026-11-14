// Order 进程提供独立鉴权的订单查询 RPC，支持幂等创建 PENDING 意图；已接入预占恢复执行器，支持取消和双方确认的不可逆决定及恢复。
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	orderv1 "remarket/backend/gen/order/v1"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/order/client"
	"remarket/backend/internal/order/handler"
	"remarket/backend/internal/order/repository"
	"remarket/backend/internal/order/service"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/database"
	"remarket/backend/pkg/grpcx"
	"remarket/backend/pkg/outbox"
	"syscall"
	"time"
)

// main 启动订单服务，失败时不回显数据库或认证凭据。
func main() {
	if err := run(); err != nil {
		slog.Error("订单服务运行失败", "error", err)
		os.Exit(1)
	}
}

// run 组装鉴权、独立数据库和查询服务，并在信号取消时释放连接与监听器。
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	config, err := auth.JWTConfigFromEnv()
	if err != nil {
		return err
	}
	tokens, err := auth.NewJWTManager(config)
	if err != nil {
		return err
	}
	dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
	db, pool, err := database.OpenMySQL(dbCtx, database.MySQLConfig{Address: os.Getenv("ORDER_MYSQL_ADDR"), Username: os.Getenv("ORDER_MYSQL_USERNAME"), Password: os.Getenv("MYSQL_ORDER_PASSWORD"), Database: os.Getenv("ORDER_MYSQL_DATABASE")})
	dbCancel()
	if err != nil {
		return err
	}
	defer pool.Close()
	stopOutbox, err := outbox.Start(ctx, pool, "order", func(event string) { slog.Warn("Outbox 发布", "event", event) })
	if err != nil {
		return err
	}
	defer stopOutbox()

	address := os.Getenv("ORDER_GRPC_ADDR")
	if address == "" {
		address = "127.0.0.1:50053"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := grpcx.NewServer(slog.Default(), grpcx.AuthInterceptor(tokens, "/grpc.health.v1.Health/Check"))
	target := os.Getenv("PRODUCT_GRPC_TARGET")
	if target == "" {
		target = "127.0.0.1:50052"
	}
	productConn, err := grpcx.Dial(target, nil, 3*time.Second)
	if err != nil {
		return err
	}
	defer productConn.Close()
	orders := repository.New(db)
	productClient := productv1.NewProductServiceClient(productConn)
	reservations, err := client.NewReservations(productClient, os.Getenv("ORDER_PRODUCT_SERVICE_TOKEN"), config.Secret)
	if err != nil {
		return err
	}
	creator := service.NewCreation(orders, client.NewProductReader(productClient))
	orderv1.RegisterOrderServiceServer(server, handler.New(service.NewQuery(orders)).WithCreator(creator).WithDecisions(service.NewDecision(orders)))
	workerCtx, stopWorker := context.WithCancel(ctx)
	done := make(chan struct{})
	expiryDone := make(chan struct{})
	go func() { defer close(expiryDone); service.NewExpiry(orders).Run(workerCtx) }()
	go func() { defer close(done); service.NewRecovery(orders, reservations).Run(workerCtx) }()
	// 先取消并等待工作者退出，再关闭 gRPC Client 与数据库连接池。
	defer func() { stopWorker(); <-done; <-expiryDone }()
	slog.Info("订单查询及预占恢复服务已启动", "address", listener.Addr().String())
	return grpcx.Serve(ctx, listener, server)
}
