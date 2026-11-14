// 商品进程提供商品业务 RPC，并可运行本服务缓存失效恢复循环。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	productv1 "remarket/backend/gen/product/v1"
	userv1 "remarket/backend/gen/user/v1"
	pc "remarket/backend/internal/product/cache"
	"remarket/backend/internal/product/client"
	"remarket/backend/internal/product/handler"
	"remarket/backend/internal/product/repository"
	"remarket/backend/internal/product/service"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/database"
	"remarket/backend/pkg/grpcx"
	redisx "remarket/backend/pkg/redis"
	"strconv"
	"syscall"
	"time"
)

// main 启动商品服务，异常退出时只记录运行错误，不记录请求凭据。
func main() {
	if err := run(); err != nil {
		slog.Error("商品服务运行失败", "error", err)
		os.Exit(1)
	}
}

// run 组装鉴权、数据库及草稿业务，退出前关闭监听器和连接池。
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
	productAuth, err := handler.NewAuthInterceptor(tokens, os.Getenv("ORDER_PRODUCT_SERVICE_TOKEN"), config.Secret)
	if err != nil {
		return err
	}
	dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
	db, pool, err := database.OpenMySQL(dbCtx, database.MySQLConfig{Address: os.Getenv("PRODUCT_MYSQL_ADDR"), Username: os.Getenv("PRODUCT_MYSQL_USERNAME"), Password: os.Getenv("MYSQL_PRODUCT_PASSWORD"), Database: os.Getenv("PRODUCT_MYSQL_DATABASE")})
	dbCancel()
	if err != nil {
		return err
	}
	defer pool.Close()
	address := os.Getenv("PRODUCT_GRPC_ADDR")
	if address == "" {
		address = "127.0.0.1:50052"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	// 预占方法使用独立服务凭据；其他方法保留用户鉴权和公开读取规则。
	server := grpcx.NewServer(slog.Default(), productAuth)
	products := repository.NewProductRepository(db)
	target := os.Getenv("USER_GRPC_TARGET")
	if target == "" {
		target = "127.0.0.1:50051"
	}
	userConn, err := grpcx.Dial(target, nil, 3*time.Second)
	if err != nil {
		return err
	}
	defer userConn.Close()
	adminVerifier := client.NewUserVerifier(userv1.NewUserServiceClient(userConn))
	// 仅显式启用失效恢复；详情缓存仍待组合故障验收后另行开启。
	var hook service.PostCommit
	enabled := false
	if raw := os.Getenv("PRODUCT_CACHE_INVALIDATION_ENABLED"); raw != "" {
		enabled, err = strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("PRODUCT_CACHE_INVALIDATION_ENABLED 必须为布尔值")
		}
	}
	if enabled {
		cfg, err := redisx.LoadConfig()
		if err != nil {
			return err
		}
		redisClient, err := redisx.Open(ctx, cfg)
		if err != nil {
			return err
		}
		defer redisClient.Close()
		store, err := pc.New(redisClient, pc.DefaultConfig())
		if err != nil {
			return err
		}
		runner := service.NewInvalidationRunner(products, store, func(event string) { slog.Warn("缓存失效恢复", "event", event) })
		hook = runner
		workerCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); runner.Run(workerCtx) }()
		// 先停止并等待恢复循环，再由前面注册的 defer 关闭 Redis 和数据库。
		defer func() { stop(); <-done }()
	}
	query := service.NewQuery(products, adminVerifier)
	productv1.RegisterProductServiceServer(server, handler.NewComplete(service.New(products).WithInvalidation(hook), service.NewPublisher(products).WithInvalidation(hook), query, service.NewMutation(products, adminVerifier).WithInvalidation(hook)).WithCategories(service.NewCategories(products)).WithOwnList(query).WithReservations(service.NewReservations(products).WithInvalidation(hook)))
	slog.Info("商品服务已启动，创建草稿 RPC 已接入", "address", listener.Addr().String())
	return grpcx.Serve(ctx, listener, server)
}
