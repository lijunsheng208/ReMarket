package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"os"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/database"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestUserRepositoryMySQL 使用独立临时 MySQL 库验证真实约束，不接触业务数据。
func TestUserRepositoryMySQL(t *testing.T) {
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
	name := fmt.Sprintf("remarket_user_test_%d", time.Now().UnixNano())
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
	migration, err := os.ReadFile("../../../../migrations/user/000001_core.up.sql")
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
	repo := NewUserRepository(db)
	hash, err := auth.HashPassword("password123")
	if err != nil {
		t.Fatal(err)
	}
	user, err := repo.Create(context.Background(), CreateUserParams{Username: "review_user", PasswordHash: hash, Nickname: "测试用户"})
	if err != nil {
		t.Fatal(err)
	}
	if user.ID <= 0 || user.Role != "USER" || user.Status != "ACTIVE" || user.CreatedAt.IsZero() {
		t.Fatal("默认字段或自增 ID 不正确")
	}
	byID, err := repo.GetByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	byName, err := repo.GetByUsername(context.Background(), "REVIEW_USER")
	if err != nil {
		t.Fatal(err)
	}
	if byName.ID != user.ID || byID.PasswordHash != hash || byID.Nickname != "测试用户" {
		t.Fatal("查询映射不一致")
	}
	if err := auth.VerifyPassword(byID.PasswordHash, "password123"); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(byID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), hash) {
		t.Fatal("JSON 泄漏密码哈希")
	}
	_, err = repo.GetByID(context.Background(), 0)
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("零 ID 查询: %v", err)
	}
	_, err = repo.GetByUsername(context.Background(), "absent")
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("不存在用户: %v", err)
	}
	// 相同用户名并发创建只允许一个成功，另一个必须返回明确冲突。
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := repo.Create(context.Background(), CreateUserParams{Username: "concurrent", PasswordHash: hash, Nickname: "并发用户"})
			results <- err
		}()
	}
	workers.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrUsernameExists) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("并发唯一约束未生效")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.GetByID(canceled, user.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("查询取消未保留: %v", err)
	}
	if _, err := repo.Create(canceled, CreateUserParams{Username: "canceled", PasswordHash: hash}); !errors.Is(err, context.Canceled) {
		t.Fatalf("创建取消未保留: %v", err)
	}
	expired, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	if _, err := repo.GetByID(expired, user.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时未保留: %v", err)
	}
	pool.Close()
	if _, err := repo.GetByID(context.Background(), user.ID); err == nil || errors.Is(err, ErrUserNotFound) {
		t.Fatal("内部数据库故障未区分")
	}
}
