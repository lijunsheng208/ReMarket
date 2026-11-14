package service

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	pc "remarket/backend/internal/product/cache"
	"remarket/backend/internal/product/model"
	"remarket/backend/internal/product/repository"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/database"
	rx "remarket/backend/pkg/redis"
)

type cacheFixture struct {
	db       *gorm.DB
	repo     *repository.ProductRepository
	redis    *redis.Client
	store    *pc.Store
	ctx      context.Context
	id       int64
	dsn      string
	seller   context.Context
	sqlReads atomic.Int64
}

// cacheSetup 创建独立数据库及高位商品 ID，所有 Redis Key 均在结束时精确清理。
func cacheSetup(t *testing.T) *cacheFixture {
	t.Helper()
	dsn := os.Getenv("REMARKET_TEST_MYSQL_DSN")
	if dsn == "" || os.Getenv("REDIS_INTEGRATION") != "1" {
		t.Skip("需要 REMARKET_TEST_MYSQL_DSN 和 REDIS_INTEGRATION=1")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("测试数据库配置无效")
	}
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("remarket_cache_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE " + name); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	db, pool, err := database.OpenMySQL(ctx, database.MySQLConfig{Address: cfg.Addr, Username: cfg.User, Password: cfg.Passwd, Database: name})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	for _, version := range []string{"000001_core", "000002_seed_categories", "000003_reservation_snapshot", "000004_cache_invalidations"} {
		raw, err := os.ReadFile("../../../../migrations/product/" + version + ".up.sql")
		if err != nil {
			t.Fatal(err)
		}
		lines := []string{}
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				lines = append(lines, line)
			}
		}
		for _, stmt := range strings.Split(strings.Join(lines, "\n"), ";") {
			if strings.TrimSpace(stmt) != "" {
				if err := db.Exec(stmt).Error; err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	rc, err := rx.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	client, err := rx.Open(ctx, rc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	store, err := pc.New(client, pc.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000_000_000_000_000))
	if err != nil {
		t.Fatal(err)
	}
	id := int64(7_000_000_000_000_000_000) + n.Int64()
	if err := db.Exec("ALTER TABLE products AUTO_INCREMENT = " + strconv.FormatInt(id, 10)).Error; err != nil {
		t.Fatal(err)
	}
	cfg.DBName = name
	f := &cacheFixture{db: db, repo: repository.NewProductRepository(db), redis: client, store: store, ctx: ctx, id: id, dsn: cfg.FormatDSN()}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		for _, pid := range []int64{id, id + 1} {
			if err := client.Del(clean, fmt.Sprintf("product:detail:v1:{%d}", pid), fmt.Sprintf("product:epoch:v1:{%d}", pid)).Err(); err != nil {
				t.Error(err)
			}
		}
	})
	manager, err := auth.NewJWTManager(auth.JWTConfig{Secret: strings.Repeat("s", 32), Issuer: "test", Audience: "test", AccessTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := manager.IssueAccessToken(2, "USER")
	if err != nil {
		t.Fatal(err)
	}
	f.seller, err = auth.Authenticate(ctx, raw, manager)
	if err != nil {
		t.Fatal(err)
	}
	// GORM Query 回调直接统计真实读取 SQL，避免把模拟 Repository 次数称为 SQL 数。
	if err := db.Callback().Query().Before("gorm:query").Register("cache_acceptance_count", func(*gorm.DB) { f.sqlReads.Add(1) }); err != nil {
		t.Fatal(err)
	}
	return f
}

// must 在集成测试主协程中立即报告失败。
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// query 创建独立服务实例，模拟多个进程各有 Singleflight 的边界。
func (f *cacheFixture) query(t *testing.T, repo QueryRepository, store DetailCache) *QueryService {
	t.Helper()
	q, err := NewCachedQuery(repo, nil, store, f.ctx, DetailLoadConfig{MaxConcurrent: 4, MaxWaiting: 50, LoadTimeout: 3 * time.Second, WaitTimeout: 100 * time.Millisecond, CacheTimeout: 200 * time.Millisecond})
	must(t, err)
	return q
}

// create 在临时库创建并发布商品，通过真实提交后 hook 失效。
func (f *cacheFixture) create(t *testing.T) {
	t.Helper()
	hook := NewInvalidationRunner(f.repo, f.store, nil)
	p, err := New(f.repo).WithInvalidation(hook).CreateDraft(f.seller, CreateDraftInput{CategoryID: 1, Title: "缓存验收", PriceCent: 100})
	must(t, err)
	if p.ID != f.id {
		t.Fatal("隔离 ID 不匹配")
	}
	_, err = NewPublisher(f.repo).WithInvalidation(hook).Publish(f.seller, p.ID, p.AggregateVersion)
	must(t, err)
}

// TestCacheAcceptance 验证真实数据库与 Redis 的命中、自动失效、空值及断连恢复。
func TestCacheAcceptance(t *testing.T) {
	f := cacheSetup(t)
	f.create(t)
	q := f.query(t, f.repo, f.store)
	_, err := q.GetDetail(f.ctx, f.id)
	must(t, err)
	before := f.sqlReads.Load()
	_, err = q.GetDetail(f.ctx, f.id)
	must(t, err)
	if f.sqlReads.Load() != before {
		t.Fatal("缓存命中仍执行读取 SQL")
	}
	q2 := f.query(t, f.repo, f.store)
	_, err = q2.GetDetail(f.ctx, f.id)
	must(t, err)
	if f.sqlReads.Load() != before {
		t.Fatal("另一实例未共享 Redis")
	}
	hook := NewInvalidationRunner(f.repo, f.store, nil)
	title := "更新后"
	p, err := NewMutation(f.repo, nil).WithInvalidation(hook).Update(f.seller, UpdateInput{ID: f.id, ExpectedVersion: 2, Title: &title})
	must(t, err)
	d, err := q2.GetDetail(f.ctx, f.id)
	must(t, err)
	if d.Product.Title != title {
		t.Fatal("更新后旧值未失效")
	}
	_, err = NewMutation(f.repo, nil).WithInvalidation(hook).OffShelf(f.seller, f.id, p.AggregateVersion)
	must(t, err)
	if _, err := q.GetDetail(f.ctx, f.id); !errors.Is(err, repository.ErrProductNotFound) {
		t.Fatal("下架后匿名仍见旧缓存")
	}
	_, err = q.GetDetail(f.seller, f.id)
	must(t, err)
	_, err = q.GetDetail(f.ctx, f.id+1)
	if !errors.Is(err, repository.ErrProductNotFound) {
		t.Fatal(err)
	}
	before = f.sqlReads.Load()
	_, err = q.GetDetail(f.ctx, f.id+1)
	if !errors.Is(err, repository.ErrProductNotFound) || f.sqlReads.Load() != before {
		t.Fatal("负缓存仍回源")
	}
	// 新建草稿 ID 正好为此前负缓存 ID，创建提交后应清除空值。
	p, err = New(f.repo).WithInvalidation(hook).CreateDraft(f.seller, CreateDraftInput{CategoryID: 1, Title: "新商品", PriceCent: 101})
	must(t, err)
	_, err = q.GetDetail(f.seller, p.ID)
	must(t, err)
	// 使用真正关闭的 Redis 连接池模拟客户端断连，不停止本机其他服务共用的 Redis。
	rc, err := rx.LoadConfig()
	must(t, err)
	disconnected, err := rx.Open(f.ctx, rc)
	must(t, err)
	broken, err := pc.New(disconnected, pc.DefaultConfig())
	must(t, err)
	must(t, disconnected.Close())
	title = "断连期间更新"
	degraded := f.query(t, f.repo, broken)
	_, err = degraded.GetDetail(f.seller, f.id)
	must(t, err)
	_, err = NewMutation(f.repo, nil).WithInvalidation(NewInvalidationRunner(f.repo, broken, nil)).Update(f.seller, UpdateInput{ID: f.id, ExpectedVersion: d.Product.AggregateVersion + 1, Title: &title})
	must(t, err)
	var pending int64
	must(t, f.db.Model(&repository.Invalidation{}).Where("status = 'PENDING'").Count(&pending).Error)
	if pending == 0 {
		t.Fatal("断连后任务丢失")
	}
	// 将重试时间推进至当前，避免基于 sleep 碰运气。
	must(t, f.db.Exec("UPDATE product_cache_invalidations SET next_attempt_at=UTC_TIMESTAMP(6) WHERE status='PENDING'").Error)
	if !hook.Process(f.ctx, f.id) {
		t.Fatal("恢复后未处理任务")
	}
	_, err = q.GetDetail(f.seller, f.id)
	must(t, err)
	t.Log("真实 SQL 计数证明正负命中零读取；跨实例缓存、更新/下架/创建失效及断连恢复通过")
}

type blockedSnapshot struct {
	*repository.ProductRepository
	once    sync.Once
	loaded  chan struct{}
	release chan struct{}
}

// GetDetailSnapshot 在真实旧快照读取完成后阻塞，以精确构造旧读晚回填。
func (r *blockedSnapshot) GetDetailSnapshot(ctx context.Context, id int64) (*model.Product, []model.ProductImage, error) {
	p, imgs, err := r.ProductRepository.GetDetailSnapshot(ctx, id)
	r.once.Do(func() { close(r.loaded) })
	select {
	case <-r.release:
		return p, imgs, err
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// TestOldFillAcceptance 验证另一实例提交失效后，旧数据库快照不能覆盖缓存。
func TestOldFillAcceptance(t *testing.T) {
	f := cacheSetup(t)
	f.create(t)
	block := &blockedSnapshot{ProductRepository: f.repo, loaded: make(chan struct{}), release: make(chan struct{})}
	q := f.query(t, block, f.store)
	done := make(chan error, 1)
	go func() { _, err := q.GetDetail(f.ctx, f.id); done <- err }()
	select {
	case <-block.loaded:
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	title := "并发新值"
	_, err := NewMutation(f.repo, nil).WithInvalidation(NewInvalidationRunner(f.repo, f.store, nil)).Update(f.seller, UpdateInput{ID: f.id, ExpectedVersion: 2, Title: &title})
	must(t, err)
	q2 := f.query(t, f.repo, f.store)
	d, err := q2.GetDetail(f.ctx, f.id)
	must(t, err)
	if d.Product.Title != title {
		t.Fatal("新值未可见")
	}
	close(block.release)
	must(t, <-done)
	cached, hit, err := f.store.Get(f.ctx, f.id)
	must(t, err)
	if !hit || cached.Title != title {
		t.Fatal("旧读覆盖新值")
	}
}

// TestCacheCrashHelper 在子进程中执行真实任务，在领取或 Redis ACK 后等待父进程强杀。
func TestCacheCrashHelper(t *testing.T) {
	mode := os.Getenv("REMARKET_CACHE_CHILD")
	if mode == "" {
		t.Skip("仅由故障验收父测试启动")
	}
	cfg, err := driver.ParseDSN(os.Getenv("REMARKET_TEST_MYSQL_DSN"))
	must(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, pool, err := database.OpenMySQL(ctx, database.MySQLConfig{Address: cfg.Addr, Username: cfg.User, Password: cfg.Passwd, Database: cfg.DBName})
	must(t, err)
	defer pool.Close()
	rc, err := rx.LoadConfig()
	must(t, err)
	client, err := rx.Open(ctx, rc)
	must(t, err)
	defer client.Close()
	store, err := pc.New(client, pc.DefaultConfig())
	must(t, err)
	repo := repository.NewProductRepository(db)
	id, err := strconv.ParseInt(os.Getenv("REMARKET_CACHE_ID"), 10, 64)
	must(t, err)
	if mode == "recover" {
		if !NewInvalidationRunner(repo, store, nil).Process(ctx, id) {
			t.Fatal("子进程未恢复任务")
		}
		return
	}
	task, err := repo.ClaimInvalidation(ctx, id, 5*time.Second)
	must(t, err)
	if task == nil {
		t.Fatal("无任务")
	}
	if mode == "ack" {
		must(t, store.Invalidate(ctx, id))
	}
	fmt.Println("CACHE_CRASH_READY")
	<-ctx.Done()
}

// TestProcessCrashAcceptance 验证独立 Worker 领取后或 ACK 后崩溃，再由新进程恢复。
func TestProcessCrashAcceptance(t *testing.T) {
	for _, mode := range []string{"claim", "ack"} {
		t.Run(mode, func(t *testing.T) {
			f := cacheSetup(t)
			f.create(t)
			q := f.query(t, f.repo, f.store)
			_, err := q.GetDetail(f.ctx, f.id)
			must(t, err)
			title := "进程恢复"
			_, err = NewMutation(f.repo, nil).Update(f.seller, UpdateInput{ID: f.id, ExpectedVersion: 2, Title: &title})
			must(t, err)
			executable, err := os.Executable()
			must(t, err)
			env := []string{}
			for _, v := range os.Environ() {
				if !strings.HasPrefix(v, "REMARKET_TEST_MYSQL_DSN=") && !strings.HasPrefix(v, "REMARKET_CACHE_CHILD=") && !strings.HasPrefix(v, "REMARKET_CACHE_ID=") {
					env = append(env, v)
				}
			}
			env = append(env, "REMARKET_TEST_MYSQL_DSN="+f.dsn, "REMARKET_CACHE_ID="+strconv.FormatInt(f.id, 10))
			cmd := exec.CommandContext(f.ctx, executable, "-test.run=^TestCacheCrashHelper$", "-test.v")
			cmd.Env = append(env, "REMARKET_CACHE_CHILD="+mode)
			pipe, err := cmd.StdoutPipe()
			must(t, err)
			must(t, cmd.Start())
			ready := make(chan bool, 1)
			go func() {
				scan := bufio.NewScanner(pipe)
				for scan.Scan() {
					if scan.Text() == "CACHE_CRASH_READY" {
						ready <- true
						return
					}
				}
				ready <- false
			}()
			select {
			case ok := <-ready:
				if !ok {
					_ = cmd.Wait()
					t.Fatal("故障子进程未就绪")
				}
			case <-f.ctx.Done():
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatal("子进程超时")
			}
			must(t, cmd.Process.Kill())
			_ = cmd.Wait()
			must(t, f.db.Exec("UPDATE product_cache_invalidations SET locked_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE status='PROCESSING'").Error)
			recover := exec.CommandContext(f.ctx, executable, "-test.run=^TestCacheCrashHelper$", "-test.v")
			recover.Env = append(env, "REMARKET_CACHE_CHILD=recover")
			if output, err := recover.CombinedOutput(); err != nil {
				t.Fatalf("恢复失败: %v %s", err, output)
			}
			d, err := q.GetDetail(f.ctx, f.id)
			must(t, err)
			if d.Product.Title != title {
				t.Fatal("进程恢复后旧值残留")
			}
			var count int64
			must(t, f.db.Model(&repository.Invalidation{}).Where("status <> 'DONE'").Count(&count).Error)
			if count != 0 {
				t.Fatal("恢复后任务未完成")
			}
		})
	}
}

// TestMutationMatrixAcceptance 验证所有实际写入口、幂等无变更及任务写失败回滚业务/事件。
func TestMutationMatrixAcceptance(t *testing.T) {
	f := cacheSetup(t)
	p, err := New(f.repo).CreateDraft(f.seller, CreateDraftInput{CategoryID: 1, Title: "草稿", PriceCent: 100})
	must(t, err)
	title := "草稿编辑"
	p, err = NewMutation(f.repo, nil).Update(f.seller, UpdateInput{ID: p.ID, ExpectedVersion: p.AggregateVersion, Title: &title})
	must(t, err)
	p, err = NewPublisher(f.repo).Publish(f.seller, p.ID, p.AggregateVersion)
	must(t, err)
	p, err = NewMutation(f.repo, nil).OffShelf(f.seller, p.ID, p.AggregateVersion)
	must(t, err)
	title = "下架编辑"
	p, err = NewMutation(f.repo, nil).Update(f.seller, UpdateInput{ID: p.ID, ExpectedVersion: p.AggregateVersion, Title: &title})
	must(t, err)
	p, err = NewPublisher(f.repo).Publish(f.seller, p.ID, p.AggregateVersion)
	must(t, err)
	// 数据库触发器仅作用于临时库，任务插入失败必须回滚商品及已写业务事件。
	must(t, f.db.Exec("CREATE TRIGGER fail_cache_task BEFORE INSERT ON product_cache_invalidations FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected'").Error)
	var before int64
	must(t, f.db.Model(&model.OutboxEvent{}).Count(&before).Error)
	title = "不得保存"
	_, err = NewMutation(f.repo, nil).Update(f.seller, UpdateInput{ID: p.ID, ExpectedVersion: p.AggregateVersion, Title: &title})
	if err == nil {
		t.Fatal("任务失败未回滚在售编辑")
	}
	must(t, f.db.Exec("DROP TRIGGER fail_cache_task").Error)
	current, err := f.repo.GetByID(f.ctx, p.ID)
	must(t, err)
	var after int64
	must(t, f.db.Model(&model.OutboxEvent{}).Count(&after).Error)
	if current.AggregateVersion != p.AggregateVersion || current.Title == title || before != after {
		t.Fatal("任务失败留下业务或事件")
	}
	reservations := NewReservations(f.repo)
	input := ReserveInput{OrderID: 100, ProductID: p.ID, BuyerID: 3, ExpectedSellerID: 2, ExpectedPriceCent: 100, ExpectedProductVersion: p.AggregateVersion, ExpiresAt: time.Now().Add(time.Minute).UTC().Truncate(time.Microsecond)}
	_, err = reservations.Reserve(f.ctx, input)
	must(t, err)
	_, err = reservations.Reserve(f.ctx, input)
	must(t, err)
	_, err = reservations.Release(f.ctx, 100, p.ID)
	must(t, err)
	_, err = reservations.Release(f.ctx, 100, p.ID)
	must(t, err)
	current, err = f.repo.GetByID(f.ctx, p.ID)
	must(t, err)
	input.OrderID = 101
	input.ExpectedProductVersion = current.AggregateVersion
	_, err = reservations.Reserve(f.ctx, input)
	must(t, err)
	_, err = reservations.Confirm(f.ctx, 101, p.ID)
	must(t, err)
	_, err = reservations.Confirm(f.ctx, 101, p.ID)
	must(t, err)
	current, err = f.repo.GetByID(f.ctx, p.ID)
	must(t, err)
	var tasks int64
	must(t, f.db.Model(&repository.Invalidation{}).Where("product_id=?", p.ID).Count(&tasks).Error)
	if tasks != current.AggregateVersion || current.Status != model.StatusSold {
		t.Fatalf("变更任务未逐版本覆盖: %d/%d", tasks, current.AggregateVersion)
	}
	runner := NewInvalidationRunner(f.repo, f.store, nil)
	for runner.Process(f.ctx, p.ID) {
	}
	var pending int64
	must(t, f.db.Model(&repository.Invalidation{}).Where("status<>'DONE'").Count(&pending).Error)
	if pending != 0 {
		t.Fatal("重复/顺序失效未完成")
	}
}

// TestSnapshotAcceptance 用查询回调屏障验证图片和商品确实来自同一 MySQL 快照。
func TestSnapshotAcceptance(t *testing.T) {
	f := cacheSetup(t)
	f.create(t)
	must(t, f.db.Create(&model.ProductImage{ProductID: f.id, ObjectKey: "old-image", SortOrder: 0}).Error)
	loaded := make(chan struct{})
	release := make(chan struct{})
	var blocked atomic.Bool
	must(t, f.db.Callback().Query().After("gorm:query").Register("cache_snapshot_barrier", func(tx *gorm.DB) {
		// 只阻塞首次商品读取，写事务及后续读取不受影响。
		if tx.Statement.Table == "products" && blocked.CompareAndSwap(false, true) {
			close(loaded)
			select {
			case <-release:
			case <-f.ctx.Done():
			}
		}
	}))
	type result struct {
		p      *model.Product
		images []model.ProductImage
		err    error
	}
	done := make(chan result, 1)
	go func() { p, imgs, err := f.repo.GetDetailSnapshot(f.ctx, f.id); done <- result{p, imgs, err} }()
	select {
	case <-loaded:
	case <-f.ctx.Done():
		t.Fatal("快照屏障未到达")
	}
	must(t, f.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.Product{}).Where("id=?", f.id).Updates(map[string]any{"title": "new-title", "aggregate_version": 3}).Error; err != nil {
			return err
		}
		return tx.Model(&model.ProductImage{}).Where("product_id=?", f.id).Update("object_key", "new-image").Error
	}))
	close(release)
	r := <-done
	must(t, r.err)
	if r.p.AggregateVersion != 2 || len(r.images) != 1 || r.images[0].ObjectKey != "old-image" {
		t.Fatal("商品和图片混用了快照")
	}
}

// TestNegativeFillRaceAcceptance 验证创建后，较早的不存在查询不能重新覆盖正值。
func TestNegativeFillRaceAcceptance(t *testing.T) {
	f := cacheSetup(t)
	block := &blockedSnapshot{ProductRepository: f.repo, loaded: make(chan struct{}), release: make(chan struct{})}
	q := f.query(t, block, f.store)
	done := make(chan error, 1)
	go func() { _, err := q.GetDetail(f.ctx, f.id); done <- err }()
	select {
	case <-block.loaded:
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	f.create(t)
	q2 := f.query(t, f.repo, f.store)
	_, err := q2.GetDetail(f.ctx, f.id)
	must(t, err)
	close(block.release)
	if err := <-done; !errors.Is(err, repository.ErrProductNotFound) {
		t.Fatal("旧不存在查询返回语义错误")
	}
	d, hit, err := f.store.Get(f.ctx, f.id)
	must(t, err)
	if !hit || d == nil {
		t.Fatal("晚到空值覆盖了新商品")
	}
}
