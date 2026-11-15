// search-rebuild 是显式维护工具，不自动改写消费目标或恢复业务写入。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/search/rebuild"
	"remarket/backend/internal/search/repository"
	"remarket/backend/pkg/grpcx"
	"syscall"
	"time"
)

// main 输出稳定错误分类，成功记录数量，不记录凭据、商品正文或 RPC 内部错误。
func main() {
	if err := run(); err != nil {
		slog.Error("搜索重建停止", "category", "rebuild_or_switch_failed")
		os.Exit(1)
	}
}

// run 要求显式目标与维护声明；构建和切换为两次独立操作，失败不删索引。
func run() error {
	target := flag.String("target", "", "全新物理索引，如 products_v2")
	old := flag.String("switch-from", "", "非空时仅重新校验并从旧索引切换别名")
	maintenance := flag.Bool("maintenance-confirmed", false, "已暂停所有商品写入、排空事件并停止消费者")
	flag.Parse()
	if !*maintenance || *target == "" || flag.NArg() != 0 {
		return fmt.Errorf("必须指定 target 与维护声明")
	}
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
	repo, err := repository.New(repository.Config{Endpoint: endpoint, Index: *target, Timeout: 3 * time.Second, Username: os.Getenv("ELASTICSEARCH_USERNAME"), Password: os.Getenv("ELASTICSEARCH_PASSWORD")})
	if err != nil {
		return err
	}
	defer repo.Close()
	if err := repo.CheckServer(ctx); err != nil {
		return err
	}
	product := os.Getenv("PRODUCT_GRPC_TARGET")
	if product == "" {
		product = "127.0.0.1:50052"
	}
	conn, err := grpcx.Dial(product, nil, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	count, err := rebuild.Run(ctx, productv1.NewProductServiceClient(conn), repo, os.Getenv("SEARCH_PRODUCT_EXPORT_TOKEN"), *old)
	if err != nil {
		return err
	}
	slog.Info("搜索重建阶段完成", "target", *target, "products", count, "alias_switched", *old != "")
	return nil
}
