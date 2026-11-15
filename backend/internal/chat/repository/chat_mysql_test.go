package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"os"
	"remarket/backend/internal/chat/model"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/database"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestChatMySQL 验证临时库迁移、并发复用、成员权限、分页和事务回滚。
func TestChatMySQL(t *testing.T) {
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
	name := fmt.Sprintf("remarket_chat_test_%d", time.Now().UnixNano())
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
	data, err := os.ReadFile("../../../../migrations/chat/000001_chat.up.sql")
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

	r := New(db)
	const count = 12
	ids := make(chan int64, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, e := r.Create(ctx, 10, 1, 2)
			if e != nil {
				errs <- e
				return
			}
			ids <- c.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	var id int64
	for n := range ids {
		if id != 0 && id != n {
			t.Fatal("并发创建产生重复会话")
		}
		id = n
	}
	var members int64
	if e := db.Model(&model.Member{}).Count(&members).Error; e != nil || members != 2 {
		t.Fatal("成员数量错误", e, members)
	}
	rows, e := r.List(ctx, 2, nil, 20)
	if e != nil || len(rows) != 1 {
		t.Fatal("卖家未看到会话", e)
	}
	rows, e = r.List(ctx, 3, nil, 20)
	if e != nil || len(rows) != 0 {
		t.Fatal("非成员看到会话", e)
	}
	for i := int64(1); i <= 4; i++ {
		m := model.Message{ConversationID: id, SenderID: 1, ClientMsgID: fmt.Sprint(i), Seq: i, Content: "hello", MessageType: "TEXT"}
		if e := db.Create(&m).Error; e != nil {
			t.Fatal(e)
		}
	}
	messages, e := r.Messages(ctx, 2, id, 4, 0, 2)
	if e != nil || len(messages) != 2 || messages[0].Seq != 3 || messages[1].Seq != 2 {
		t.Fatal("历史分页错误", e)
	}
	messages, e = r.Messages(ctx, 2, id, 0, 2, 10)
	if e != nil || len(messages) != 2 || messages[0].Seq != 3 {
		t.Fatal("增量分页错误", e)
	}
	_, e = r.Messages(ctx, 3, id, 0, 0, 20)
	var app *apperror.Error
	if !errors.As(e, &app) || app.Kind != apperror.Forbidden {
		t.Fatal("非成员未拒绝", e)
	}
	// 第二成员插入失败时，会话与第一成员均不得残留。
	if e := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM conversations").Scan(&members); e != nil {
		t.Fatal(e)
	}
	if _, e := pool.ExecContext(ctx, "CREATE TRIGGER reject_member BEFORE INSERT ON conversation_members FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'test failure'"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Create(ctx, 11, 4, 5); e == nil {
		t.Fatal("故障未传播")
	}
	var total int64
	if e := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM conversations").Scan(&total); e != nil || total != members {
		t.Fatal("会话未回滚", e)
	}
}
