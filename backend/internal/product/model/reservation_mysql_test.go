package model

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"remarket/backend/pkg/database"
)

// TestReservationSnapshotMySQL 验证预占快照增量迁移保留历史记录并约束新快照，不修改业务库。
func TestReservationSnapshotMySQL(t *testing.T) {
	dsn := os.Getenv("REMARKET_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未配置 REMARKET_TEST_MYSQL_DSN，跳过真实 MySQL 验证")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("测试 DSN 无效")
	}
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("无法创建测试数据库连接")
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := fmt.Sprintf("remarket_reservation_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name+" CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal("创建临时数据库失败")
	}
	// 清理仅针对本测试生成的库，使用独立超时以免主测试取消后残留。
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := admin.ExecContext(cleanup, "DROP DATABASE "+name); err != nil {
			t.Error("清理临时数据库失败：", name)
		}
	}()
	db, pool, err := database.OpenMySQL(ctx, database.MySQLConfig{Address: cfg.Addr, Username: cfg.User, Password: cfg.Passwd, Database: name})
	if err != nil {
		t.Fatal("连接临时数据库失败")
	}
	defer pool.Close()
	data, err := os.ReadFile("../../../../migrations/product/000001_core.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}
	for _, stmt := range strings.Split(strings.Join(lines, "\n"), ";") {
		if strings.TrimSpace(stmt) != "" {
			if _, err := pool.ExecContext(ctx, stmt); err != nil {
				t.Fatal("迁移失败：", err)
			}
		}
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := pool.ExecContext(ctx, "INSERT INTO categories (code,name) VALUES ('test','测试')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO products (seller_id,category_id,title,description,price_cent) VALUES (2,1,'测试','测试',100)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO product_reservations (product_id,order_id,buyer_id,expires_at) VALUES (1,1,3,?)", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../../../migrations/product/000003_reservation_snapshot.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	var legacy ProductReservation
	if err := db.WithContext(ctx).First(&legacy, 1).Error; err != nil {
		t.Fatal(err)
	}
	if legacy.OrderID != 1 || legacy.ExpectedSellerID != nil || legacy.ExpectedPriceCent != nil || legacy.ExpectedProductVersion != nil {
		t.Fatal("历史记录未保留或被错误回填")
	}
	for _, stmt := range []string{
		"UPDATE product_reservations SET expected_seller_id=2 WHERE id=1",
		"UPDATE product_reservations SET expected_seller_id=2,expected_price_cent=0,expected_product_version=1 WHERE id=1",
		"UPDATE product_reservations SET expected_seller_id=3,expected_price_cent=100,expected_product_version=1 WHERE id=1",
		"UPDATE product_reservations SET expected_seller_id=2,expected_price_cent=100,expected_product_version=0 WHERE id=1",
	} {
		if _, err := pool.ExecContext(ctx, stmt); err == nil {
			t.Errorf("非法快照未拒绝：%s", stmt)
		}
	}
	if _, err := pool.ExecContext(ctx, "UPDATE product_reservations SET status='RELEASED',released_at=? WHERE id=1", now); err != nil {
		t.Fatal(err)
	}
	seller, price, version := int64(2), int64(100), int64(1)
	reservation := ProductReservation{ProductID: 1, OrderID: 2, BuyerID: 3, Status: ReservationActive, ExpiresAt: now.Add(time.Hour), ExpectedSellerID: &seller, ExpectedPriceCent: &price, ExpectedProductVersion: &version, CreatedAt: now, UpdatedAt: now}
	if err := db.WithContext(ctx).Create(&reservation).Error; err != nil {
		t.Fatal(err)
	}
	var got ProductReservation
	if err := db.WithContext(ctx).First(&got, reservation.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.ExpectedSellerID == nil || *got.ExpectedSellerID != seller || got.ExpectedPriceCent == nil || *got.ExpectedPriceCent != price || got.ExpectedProductVersion == nil || *got.ExpectedProductVersion != version || got.ActiveProductID == nil || *got.ActiveProductID != 1 {
		t.Fatal("新快照或生成列映射不正确")
	}
}
