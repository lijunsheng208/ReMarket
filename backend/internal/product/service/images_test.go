package service

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"remarket/backend/internal/product/model"
	"sync"
	"testing"
)

type imageMemory struct {
	mu   sync.Mutex
	data map[string][]byte
	puts int
}

// PresignPut 模拟对象写入地址，不生成真实云端请求。
func (m *imageMemory) PresignPut(context.Context, string, string) (string, error) {
	return "https://upload.invalid", nil
}

// Read 返回受测试控制的临时对象副本。
func (m *imageMemory) Read(_ context.Context, key string, _ int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte{}, m.data[key]...), nil
}

// Put 模拟正式对象独立键写入，测试中不涉及 OSS 账单。
func (m *imageMemory) Put(_ context.Context, key string, data []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = append([]byte{}, data...)
	m.puts++
	return nil
}

// PresignGet 模拟经过权限检查后的访问签名。
func (m *imageMemory) PresignGet(context.Context, string) (string, error) {
	return "https://image.invalid", nil
}

// testPNG 构造可验证的小 PNG 图片。
func testPNG(t *testing.T) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 2, 3))
	im.Set(0, 0, color.White)
	var out bytes.Buffer
	if err := png.Encode(&out, im); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// TestValidateImage 验证真实格式、声明大小、损坏文件以及重新编码去掉尾随数据。
func TestValidateImage(t *testing.T) {
	raw := testPNG(t)
	encoded, w, h, err := validateImage(raw, int64(len(raw)), "image/png")
	if err != nil || w != 2 || h != 3 || len(encoded) == 0 {
		t.Fatalf("有效图片失败 %v", err)
	}
	if _, _, _, err := validateImage(raw, int64(len(raw)+1), "image/png"); err == nil {
		t.Fatal("错误大小未拒绝")
	}
	if _, _, _, err := validateImage(raw, int64(len(raw)), "image/jpeg"); err == nil {
		t.Fatal("伪造类型未拒绝")
	}
	bad := []byte("<script>alert(1)</script>")
	if _, _, _, err := validateImage(bad, int64(len(bad)), "image/png"); err == nil {
		t.Fatal("非图片未拒绝")
	}
	poly := append(append([]byte{}, raw...), bad...)
	clean, _, _, err := validateImage(poly, int64(len(poly)), "image/png")
	if err != nil || bytes.Contains(clean, bad) {
		t.Fatal("尾随脚本未清理")
	}
}

// TestImageCompletionMySQL 使用临时库验证并发完成幂等、权限、事务内版本与 Outbox 一致性。
func TestImageCompletionMySQL(t *testing.T) {
	f := cacheSetup(t)
	p, err := New(f.repo).CreateDraft(f.seller, CreateDraftInput{CategoryID: 1, Title: "图片测试", Description: "test", PriceCent: 100})
	if err != nil {
		t.Fatal(err)
	}
	raw := testPNG(t)
	objects := &imageMemory{data: map[string][]byte{}}
	s := NewImages(f.repo, objects, nil, nil)
	u, _, err := s.Prepare(f.seller, p.ID, int64(len(raw)), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	objects.data["staging/"+u.ID] = raw
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Complete(f.seller, p.ID, u.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	images, err := f.repo.ListImages(f.ctx, p.ID)
	if err != nil || len(images) != 1 {
		t.Fatalf("重复完成产生多张图片 %v %d", err, len(images))
	}
	current, err := f.repo.GetByID(f.ctx, p.ID)
	if err != nil || current.AggregateVersion != 2 {
		t.Fatal("图片版本未且仅递增一次")
	}
	if _, err := s.Access(context.Background(), p.ID); err == nil {
		t.Fatal("匿名访问草稿图片")
	}
	if _, err := s.Access(f.seller, p.ID); err != nil {
		t.Fatal(err)
	}
	// 已确认会话不能因临时对象后来被覆盖而改变正式图片。
	objects.data["staging/"+u.ID] = []byte("replaced")
	if _, err := s.Complete(f.seller, p.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	again, _ := f.repo.ListImages(f.ctx, p.ID)
	if again[0].ObjectKey != images[0].ObjectKey {
		t.Fatal("已完成图片被覆盖")
	}
	published, err := NewPublisher(f.repo).Publish(f.seller, p.ID, current.AggregateVersion)
	if err != nil {
		t.Fatal(err)
	}
	u, _, err = s.Prepare(f.seller, p.ID, int64(len(raw)), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	objects.data["staging/"+u.ID] = raw
	if _, err := s.Complete(f.seller, p.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := f.db.Model(&model.OutboxEvent{}).Where("aggregate_id = ? AND aggregate_version = ? AND event_type = ?", p.ID, published.AggregateVersion+1, "product.updated.v1").Count(&count).Error; err != nil || count != 1 {
		t.Fatal("图片变更未与 Outbox 同事务提交")
	}
}
