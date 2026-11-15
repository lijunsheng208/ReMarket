// search 提供只读搜索 gRPC 进程，索引消费由独立进程负责。
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	searchv1 "remarket/backend/gen/search/v1"
	"remarket/backend/internal/search/handler"
	"remarket/backend/internal/search/repository"
	"remarket/backend/internal/search/service"
	"remarket/backend/pkg/grpcx"
	"syscall"
	"time"
)

// main 只记录稳定退出分类，不输出配置凭据或搜索游标。
func main() {
	if err := run(); err != nil {
		slog.Error("Search 服务退出", "category", "startup_or_serve_failed")
		os.Exit(1)
	}
}

// run 校验密钥、组装 ES 与业务并启动 gRPC，取消后释放监听器与连接。
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	endpoint := os.Getenv("ELASTICSEARCH_URL")
	if endpoint == "" {
		port := os.Getenv("ELASTICSEARCH_PORT")
		if port == "" {
			port = "9200"
		}
		endpoint = "http://127.0.0.1:" + port
	}
	target := os.Getenv("SEARCH_WRITE_INDEX")
	if target == "" {
		target = "products_v1"
	}
	repo, err := repository.New(repository.Config{Endpoint: endpoint, Index: target, Timeout: 3 * time.Second, Username: os.Getenv("ELASTICSEARCH_USERNAME"), Password: os.Getenv("ELASTICSEARCH_PASSWORD")})
	if err != nil {
		return err
	}
	defer repo.Close()
	if err := repo.CheckServer(ctx); err != nil {
		return err
	}
	query, err := service.New(repo, []byte(os.Getenv("SEARCH_CURSOR_SECRET")))
	if err != nil {
		return err
	}
	address := os.Getenv("SEARCH_GRPC_ADDR")
	if address == "" {
		address = "127.0.0.1:50054"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := grpcx.NewServer(slog.Default())
	searchv1.RegisterSearchServiceServer(server, handler.New(query))
	return grpcx.Serve(ctx, listener, server)
}
