package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"os"
	"remarket/backend/internal/product/model"
	"remarket/backend/pkg/database"
	"strings"
	"testing"
	"time"
)

// TestProductRepositoryMySQL 使用独立临时库验证草稿读写、分类及数据库错误，不操作业务库。
func TestProductRepositoryMySQL(t *testing.T) {
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
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("remarket_product_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name + " CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	// 只清理本测试生成的独立库，绝不删除用户提供的现有库。
	defer func() {
		if _, err := admin.Exec("DROP DATABASE " + name); err != nil {
			t.Error(err)
		}
	}()
	db, pool, err := database.OpenMySQL(context.Background(), database.MySQLConfig{Address: cfg.Addr, Username: cfg.User, Password: cfg.Passwd, Database: name})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	migration, err := os.ReadFile("../../../../migrations/product/000001_core.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	// 先移除整行 SQL 注释，避免注释中的分号被误当作语句分隔符。
	var statements []string
	for _, line := range strings.Split(string(migration), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			statements = append(statements, line)
		}
	}
	for _, statement := range strings.Split(strings.Join(statements, "\n"), ";") {
		if strings.TrimSpace(statement) != "" {
			if _, err := pool.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}

	seed, err := os.ReadFile("../../../../migrations/product/000002_seed_categories.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(string(seed)); err != nil {
		t.Fatal(err)
	}
	repo := NewProductRepository(db)
	ctx := context.Background()
	categories, err := repo.ListEnabledCategories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(categories) != 6 {
		t.Fatal("初始化分类数量不正确")
	}
	category, err := repo.GetCategoryByID(ctx, categories[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !category.Enabled || category.Code != "digital" {
		t.Fatal("分类映射不正确")
	}
	params := CreateDraftParams{SellerID: 7, CategoryID: category.ID, Title: "机械键盘", Description: "功能正常", PriceCent: 15000}
	draft, err := repo.CreateDraft(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetByID(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID <= 0 || stored.SellerID != 7 || stored.PriceCent != 15000 || stored.Title != params.Title || stored.Description != params.Description || stored.Status != model.StatusDraft || stored.AggregateVersion != 1 || stored.FirstPublishedAt != nil || stored.CreatedAt.IsZero() || stored.UpdatedAt.IsZero() {
		t.Fatal("草稿字段不正确")
	}
	var count int
	if err := pool.QueryRow("SELECT COUNT(*) FROM outbox_events").Scan(&count); err != nil || count != 0 {
		t.Fatal("创建草稿不应产生 Outbox")
	}
	if _, err := repo.GetByID(ctx, 0); !errors.Is(err, ErrProductNotFound) {
		t.Fatal("零 ID 不应查到首条商品")
	}
	if _, err := repo.GetCategoryByID(ctx, 0); !errors.Is(err, ErrCategoryNotFound) {
		t.Fatal("零 ID 不应查到首条分类")
	}
	invalid := params
	invalid.CategoryID = 999999
	if _, err := repo.CreateDraft(ctx, invalid); !errors.Is(err, ErrCategoryReference) {
		t.Fatalf("分类外键错误未转换: %v", err)
	}
	invalid = params
	invalid.PriceCent = 0
	if _, err := repo.CreateDraft(ctx, invalid); err == nil {
		t.Fatal("数据库未拒绝零价")
	}
	// 相同排序值时按 ID 稳定排序，禁用分类不出现在公开分类列表。
	if _, err := pool.Exec("UPDATE categories SET sort_order=1, enabled=(code <> 'other')"); err != nil {
		t.Fatal(err)
	}
	categories, err = repo.ListEnabledCategories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(categories) != 5 {
		t.Fatal("禁用分类仍在列表")
	}
	for i := 1; i < len(categories); i++ {
		if categories[i-1].ID >= categories[i].ID {
			t.Fatal("分类排序不稳定")
		}
	}
	var disabledID int64
	if err := pool.QueryRow("SELECT id FROM categories WHERE code='other'").Scan(&disabledID); err != nil {
		t.Fatal(err)
	}
	disabled, err := repo.GetCategoryByID(ctx, disabledID)
	if err != nil || disabled.Enabled {
		t.Fatal("内部分类查询应保留启用状态")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repo.GetByID(canceled, draft.ID); !errors.Is(err, context.Canceled) {
		t.Fatal("查询取消丢失")
	}
	if _, err := repo.CreateDraft(canceled, params); !errors.Is(err, context.Canceled) {
		t.Fatal("创建取消丢失")
	}
	if _, err := repo.ListEnabledCategories(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("分类取消丢失")
	}
	expired, cancelDeadline := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancelDeadline()
	if _, err := repo.GetCategoryByID(expired, category.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("超时根因丢失")
	}
	if _, err := pool.Exec("UPDATE categories SET enabled=false"); err != nil {
		t.Fatal(err)
	}
	categories, err = repo.ListEnabledCategories(ctx)
	if err != nil || categories == nil || len(categories) != 0 {
		t.Fatal("空列表语义错误")
	}
	pool.Close()
	if _, err := repo.GetByID(ctx, draft.ID); err == nil || errors.Is(err, ErrProductNotFound) {
		t.Fatal("内部故障误映射")
	}
}
