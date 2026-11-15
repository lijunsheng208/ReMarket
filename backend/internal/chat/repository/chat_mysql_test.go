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

	// 新会话用于发送测试，不使用上面的手工历史数据。
	sentConversation, err := r.Create(ctx, 20, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	var sendWG sync.WaitGroup
	sendErrors := make(chan error, 20)
	for i := 0; i < 20; i++ {
		sendWG.Add(1)
		go func() {
			defer sendWG.Done()
			_, e := r.Send(ctx, 1, sentConversation.ID, "retry-key", "same content")
			sendErrors <- e
		}()
	}
	sendWG.Wait()
	close(sendErrors)
	for e := range sendErrors {
		if e != nil {
			t.Fatal("同键并发发送失败", e)
		}
	}
	first, err := r.Send(ctx, 1, sentConversation.ID, "retry-key", "same content")
	if err != nil || first.Message.Seq != 1 {
		t.Fatal("幂等未返回原序号", err)
	}
	if _, err := r.Send(ctx, 1, sentConversation.ID, "retry-key", "changed"); err == nil {
		t.Fatal("同键不同正文被接受")
	}
	if _, err := r.Send(ctx, 3, sentConversation.ID, "intruder", "hello"); err == nil {
		t.Fatal("非成员可以发送")
	}
	// 不同键竞争同一会话须得到无重复、无空洞的递增序号。
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		sendWG.Add(1)
		go func(i int) {
			defer sendWG.Done()
			_, e := r.Send(ctx, 2, sentConversation.ID, fmt.Sprintf("ordered-%d", i), "next")
			results <- e
		}(i)
	}
	sendWG.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	var saved model.Conversation
	if e := db.First(&saved, sentConversation.ID).Error; e != nil || saved.LastSeq != 13 {
		t.Fatal("序号未连续分配", e, saved.LastSeq)
	}
	var actual []model.Message
	if e := db.Where("conversation_id = ?", saved.ID).Order("seq ASC").Find(&actual).Error; e != nil {
		t.Fatal(e)
	}
	for i, m := range actual {
		if m.Seq != int64(i+1) {
			t.Fatal("序号存在空洞")
		}
	}
	if len(actual) != 13 || saved.LastMessageID == nil || *saved.LastMessageID != actual[12].ID {
		t.Fatal("消息元数据不一致")
	}
	// 元数据更新失败时插入消息与序号必须一并回滚。
	if _, e := pool.ExecContext(ctx, "CREATE TRIGGER reject_chat_update BEFORE UPDATE ON conversations FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'test failure'"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Send(ctx, 1, saved.ID, "rollback-key", "rollback"); e == nil {
		t.Fatal("故障未传播")
	}
	var failed int64
	if e := db.Model(&model.Message{}).Where("client_msg_id = ?", "rollback-key").Count(&failed).Error; e != nil || failed != 0 {
		t.Fatal("消息未回滚", e)
	}
	if _, e := pool.ExecContext(ctx, "DROP TRIGGER reject_chat_update"); e != nil {
		t.Fatal(e)
	}
	recovered, e := r.Send(ctx, 1, saved.ID, "rollback-key", "rollback")
	if e != nil || recovered.Message.Seq != 14 {
		t.Fatal("回滚消耗序号", e)
	}
	other, e := r.Create(ctx, 21, 1, 2)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := r.Send(ctx, 1, other.ID, "retry-key", "same content"); e == nil {
		t.Fatal("跨会话复用键被接受")
	}

	// 未读只统计对方消息：序号中包含自己发送的消息，不能直接相减。
	buyerRows, e := r.List(ctx, 1, nil, 100)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, row := range buyerRows {
		if row.ID == saved.ID {
			found = true
			if row.UnreadCount != 12 || row.LastReadSeq != 0 {
				t.Fatal("未读包含本人消息", row)
			}
		}
	}
	if !found {
		t.Fatal("列表缺少会话")
	}
	state, e := r.Read(ctx, 1, saved.ID, 8)
	if e != nil || state.State.LastReadSeq != 8 || state.State.UnreadCount != 5 {
		t.Fatal("已读统计错误", e, state)
	}
	staleRead, e := r.Read(ctx, 1, saved.ID, 4)
	if e != nil || staleRead.State.LastReadSeq != 8 {
		t.Fatal("旧请求回退游标", e)
	}
	if _, e := r.Read(ctx, 1, saved.ID, 15); e == nil {
		t.Fatal("接受未来游标")
	}
	if _, e := r.Read(ctx, 3, saved.ID, 0); e == nil {
		t.Fatal("非成员修改已读")
	}
	empty, e := r.Read(ctx, 1, other.ID, 0)
	if e != nil || empty.State.LastSeq != 0 || empty.State.UnreadCount != 0 {
		t.Fatal("空会话已读错误", e)
	}
	page, e := r.Sync(ctx, 1, saved.ID, 0, 0, 3)
	if e != nil || len(page.Messages) != 3 || page.Messages[0].Seq != 1 || page.Until != 14 {
		t.Fatal("从零补拉错误", e)
	}
	// 多设备乱序请求最终取最大游标；另一成员的已读位置不受影响。
	readErrors := make(chan error, 6)
	for _, seq := range []int64{9, 13, 11, 14, 2, 0} {
		sendWG.Add(1)
		go func(seq int64) { defer sendWG.Done(); _, e := r.Read(ctx, 1, saved.ID, seq); readErrors <- e }(seq)
	}
	sendWG.Wait()
	close(readErrors)
	for e := range readErrors {
		if e != nil {
			t.Fatal(e)
		}
	}
	finalRead, e := r.Read(ctx, 1, saved.ID, 0)
	if e != nil || finalRead.State.LastReadSeq != 14 || finalRead.State.UnreadCount != 0 {
		t.Fatal("并发已读未单调", e)
	}
	sellerRead, e := r.Read(ctx, 2, saved.ID, 0)
	if e != nil || sellerRead.State.LastReadSeq != 0 || sellerRead.State.UnreadCount != 2 {
		t.Fatal("污染另一成员状态", e)
	}
	// 新消息和旧已读位置竞争时，不得把新到消息误标为已读。
	concurrent := make(chan error, 20)
	for i := 0; i < 10; i++ {
		sendWG.Add(2)
		go func(i int) {
			defer sendWG.Done()
			_, e := r.Send(ctx, 2, saved.ID, fmt.Sprintf("new-unread-%d", i), "new")
			concurrent <- e
		}(i)
		go func() { defer sendWG.Done(); _, e := r.Read(ctx, 1, saved.ID, 14); concurrent <- e }()
	}
	sendWG.Wait()
	close(concurrent)
	for e := range concurrent {
		if e != nil {
			t.Fatal(e)
		}
	}
	finalRead, e = r.Read(ctx, 1, saved.ID, 14)
	if e != nil || finalRead.State.UnreadCount != 10 || finalRead.State.LastSeq != 24 {
		t.Fatal("新消息被误标为已读", e)
	}
	// 固定上界补拉必须结束于旧上界，新提交的消息留给下一轮。
	after := int64(3)
	for after < page.Until {
		next, e := r.Sync(ctx, 1, saved.ID, after, page.Until, 3)
		if e != nil || len(next.Messages) == 0 {
			t.Fatal("分页补拉中断", e)
		}
		for _, m := range next.Messages {
			if m.Seq != after+1 {
				t.Fatal("补拉遗漏或重复")
			}
			after = m.Seq
		}
	}
	if after != 14 {
		t.Fatal("补拉越过固定上界")
	}
	latest, e := r.Sync(ctx, 1, saved.ID, after, 0, 100)
	if e != nil || len(latest.Messages) != 10 || latest.Until != 24 {
		t.Fatal("下一轮补拉未恢复新消息", e)
	}
	if _, e := r.Sync(ctx, 3, saved.ID, 0, 0, 20); e == nil {
		t.Fatal("非成员补拉")
	}
	if _, e := r.Sync(ctx, 1, saved.ID, 25, 0, 20); e == nil {
		t.Fatal("接受越界补拉")
	}
	if _, e := pool.ExecContext(ctx, "CREATE TRIGGER reject_read BEFORE UPDATE ON conversation_members FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'test read failure'"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Read(ctx, 1, saved.ID, 24); e == nil {
		t.Fatal("已读更新故障未传播")
	}
	var member model.Member
	if e := db.Where("conversation_id = ? AND user_id = ?", saved.ID, 1).Take(&member).Error; e != nil || member.LastReadSeq != 14 {
		t.Fatal("失败事务改变游标", e)
	}
	if _, e := pool.ExecContext(ctx, "DROP TRIGGER reject_read"); e != nil {
		t.Fatal(e)
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
