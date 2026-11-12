// 商品进程提供创建草稿 RPC，发布等业务暂未实现。
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	productv1 "remarket/backend/gen/product/v1"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/internal/product/client"
	"remarket/backend/internal/product/handler"
	"remarket/backend/internal/product/repository"
	"remarket/backend/internal/product/service"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/database"
	"remarket/backend/pkg/grpcx"
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
	// 商品写接口默认鉴权，当前仅匿名放行健康检查；公开读取在对应业务阶段增加。
	server := grpcx.NewServer(slog.Default(), grpcx.OptionalAuthInterceptor(tokens, []string{productv1.ProductService_GetProduct_FullMethodName, productv1.ProductService_ListProducts_FullMethodName, productv1.ProductService_ListCategories_FullMethodName}, "/grpc.health.v1.Health/Check"))
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
	query := service.NewQuery(products, adminVerifier)
	productv1.RegisterProductServiceServer(server, handler.NewComplete(service.New(products), service.NewPublisher(products), query, service.NewMutation(products, adminVerifier)).WithCategories(service.NewCategories(products)).WithOwnList(query))
	slog.Info("商品服务已启动，创建草稿 RPC 已接入", "address", listener.Addr().String())
	return grpcx.Serve(ctx, listener, server)
}
