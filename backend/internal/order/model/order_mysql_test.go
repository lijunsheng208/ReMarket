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

// TestOrderSchemaMySQL 在随机临时库验证迁移、模型映射和约束，不修改业务库。
func TestOrderSchemaMySQL(t *testing.T) {
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
	name := fmt.Sprintf("remarket_order_test_%d", time.Now().UnixNano())
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
	data, err := os.ReadFile("../../../../migrations/order/000001_core.up.sql")
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
	order := Order{BuyerID: 1, SellerID: 2, ProductID: 3, PriceCent: 100, Status: StatusPending, IdempotencyKey: "Key", RequestHash: strings.Repeat("a", 64), AggregateVersion: 1, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := db.WithContext(ctx).Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	var got Order
	if err := db.WithContext(ctx).First(&got, order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.PriceCent != 100 || got.Status != StatusPending || got.ClosedAt != nil || !got.ExpiresAt.Equal(order.ExpiresAt) {
		t.Fatal("订单字段映射不正确")
	}
	insert := "INSERT INTO orders (buyer_id,seller_id,product_id,price_cent,idempotency_key,request_hash,expires_at) VALUES (1,2,3,100,?,REPEAT('a',64),DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR))"
	if _, err := pool.ExecContext(ctx, insert, "Key"); err == nil {
		t.Fatal("重复幂等键未拒绝")
	}
	if _, err := pool.ExecContext(ctx, insert, "key"); err != nil {
		t.Fatal("大小写不同的幂等键或同商品历史订单被拒绝")
	}
	invalid := []string{
		"UPDATE orders SET buyer_id=seller_id WHERE id=1",
		"UPDATE orders SET price_cent=0 WHERE id=1",
		"UPDATE orders SET status='UNKNOWN' WHERE id=1",
		"UPDATE orders SET buyer_confirmed=2 WHERE id=1",
		"UPDATE orders SET status='COMPLETED',closed_at=UTC_TIMESTAMP(6) WHERE id=1",
		"UPDATE orders SET closed_at=UTC_TIMESTAMP(6) WHERE id=1",
		"UPDATE orders SET expires_at=created_at WHERE id=1",
		"INSERT INTO order_operations (operation_id,order_id,operation_type) VALUES (UUID(),999999,'RESERVE')",
		"INSERT INTO order_operations (operation_id,order_id,operation_type,status) VALUES (UUID(),1,'RESERVE','PROCESSING')",
		"INSERT INTO order_operations (operation_id,order_id,operation_type,status) VALUES (UUID(),1,'RESERVE','SUCCEEDED')",
	}
	for _, stmt := range invalid {
		if _, err := pool.ExecContext(ctx, stmt); err == nil {
			t.Errorf("非法数据未拒绝：%s", stmt)
		}
	}
	op := OrderOperation{OperationID: "00000000-0000-0000-0000-000000000001", OrderID: order.ID, OperationType: OperationReserve, Status: OperationPending, NextRetryAt: now, CreatedAt: now, UpdatedAt: now}
	if err := db.WithContext(ctx).Create(&op).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO order_operations (operation_id,order_id,operation_type) VALUES (UUID(),?,'RESERVE')", order.ID); err == nil {
		t.Fatal("重复操作未拒绝")
	}
	event := OutboxEvent{EventID: "00000000-0000-0000-0000-000000000002", EventType: "order.created.v1", AggregateType: "order", AggregateID: order.ID, AggregateVersion: 1, SchemaVersion: 1, Payload: []byte(`{"order_id":"1"}`), OccurredAt: now, PublishStatus: PublishPending, NextAttemptAt: now, CreatedAt: now}
	if err := db.WithContext(ctx).Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE outbox_events SET payload='invalid' WHERE id=?", event.ID); err == nil {
		t.Fatal("非法 JSON 未拒绝")
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO outbox_events (event_id,event_type,aggregate_type,aggregate_id,aggregate_version,payload,occurred_at) SELECT UUID(),event_type,aggregate_type,aggregate_id,aggregate_version,payload,occurred_at FROM outbox_events WHERE id=?", event.ID); err == nil {
		t.Fatal("重复聚合事件未拒绝")
	}
	// 直接 SQL 只验证行内约束，不能代表 Service 已执行合法历史转换。
	if _, err := pool.ExecContext(ctx, "UPDATE orders SET status='COMPLETED',buyer_confirmed=1,seller_confirmed=1,closed_at=? WHERE id=?", now, order.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE order_operations SET status='PROCESSING',locked_until=?,lock_token=? WHERE id=?", now.Add(time.Minute), "00000000-0000-0000-0000-000000000003", op.ID); err != nil {
		t.Fatal(err)
	}
	var storedOp OrderOperation
	if err := db.WithContext(ctx).First(&storedOp, op.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedOp.OrderID != order.ID || storedOp.OperationType != OperationReserve || storedOp.Status != OperationProcessing || storedOp.LockedUntil == nil || storedOp.LockToken == nil || storedOp.FinishedAt != nil {
		t.Fatal("操作字段映射不正确")
	}
	if _, err := pool.ExecContext(ctx, "UPDATE order_operations SET status='SUCCEEDED',locked_until=NULL,lock_token=NULL,finished_at=? WHERE id=?", now, op.ID); err != nil {
		t.Fatal(err)
	}
	var storedEvent OutboxEvent
	if err := db.WithContext(ctx).First(&storedEvent, event.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedEvent.EventID != event.EventID || storedEvent.AggregateID != order.ID || storedEvent.PublishStatus != PublishPending || len(storedEvent.Payload) == 0 || storedEvent.PublishedAt != nil {
		t.Fatal("Outbox 字段映射不正确")
	}
	if _, err := pool.ExecContext(ctx, "UPDATE outbox_events SET publish_status='PROCESSING' WHERE id=?", event.ID); err == nil {
		t.Fatal("缺少租约的事件领取未拒绝")
	}
	if _, err := pool.ExecContext(ctx, "UPDATE outbox_events SET publish_status='PUBLISHED' WHERE id=?", event.ID); err == nil {
		t.Fatal("缺少发布时间未拒绝")
	}
	if _, err := pool.ExecContext(ctx, "UPDATE outbox_events SET publish_status='PUBLISHED',published_at=? WHERE id=?", now, event.ID); err != nil {
		t.Fatal(err)
	}

}
