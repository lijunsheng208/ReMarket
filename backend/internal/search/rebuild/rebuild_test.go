package rebuild

import (
	"context"
	"errors"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/search/repository"
	"strings"
	"testing"
	"time"
)

type fakeSource struct{ bad bool }

// ExportSearchSnapshot 返回已售快照，用于验证重建保留不可见版本。
func (f fakeSource) ExportSearchSnapshot(context.Context, *productv1.ExportSearchSnapshotRequest, ...grpc.CallOption) (*productv1.ExportSearchSnapshotResponse, error) {
	p := &productv1.Product{Id: 1, SellerId: 2, CategoryId: 3, Title: "手机", PriceCent: 100, AggregateVersion: 4, Status: productv1.ProductStatus_PRODUCT_STATUS_SOLD, FirstPublishedAt: timestamppb.New(time.Now())}
	next := int64(0)
	if f.bad {
		next = 2
	}
	return &productv1.ExportSearchSnapshotResponse{Products: []*productv1.Product{p}, UpperId: 3, NextAfterId: next}, nil
}

type fakeTarget struct {
	created, put, verified, switched bool
	fail                             bool
}

// CreateIndex 标记新索引创建，不修改别名。
func (f *fakeTarget) CreateIndex(context.Context) error { f.created = true; return nil }

// Put 保存是否保留不可见快照。
func (f *fakeTarget) Put(_ context.Context, d repository.Document) (bool, error) {
	f.put = d.Status == "SOLD"
	return true, nil
}

// VerifyDocument 模拟内容核验，可在切换前制造失败。
func (f *fakeTarget) VerifyDocument(context.Context, repository.Document) error {
	if f.fail {
		return errors.New("mismatch")
	}
	f.verified = true
	return nil
}

// VerifyCount 校验总数步骤在别名切换前执行。
func (f *fakeTarget) VerifyCount(context.Context, int64) error {
	if !f.verified {
		return errors.New("not verified")
	}
	return nil
}

// SwitchReadAlias 标记显式切换动作。
func (f *fakeTarget) SwitchReadAlias(context.Context, string) error { f.switched = true; return nil }

// TestBuildAndSwitch 验证构建不切换、切换前重新核验及失败保留旧别名。
func TestBuildAndSwitch(t *testing.T) {
	token := strings.Repeat("k", 32)
	f := &fakeTarget{}
	if n, err := Run(context.Background(), fakeSource{}, f, token, ""); err != nil || n != 1 || !f.created || !f.put || f.switched {
		t.Fatal("构建边界错误")
	}
	f = &fakeTarget{fail: true}
	if _, err := Run(context.Background(), fakeSource{}, f, token, "products_v1"); err == nil || f.switched || f.created {
		t.Fatal("校验失败仍切换")
	}
	f = &fakeTarget{}
	if _, err := Run(context.Background(), fakeSource{}, f, token, "products_v1"); err != nil || !f.switched || f.created || f.put {
		t.Fatal("显式切换流程错误")
	}
	f = &fakeTarget{}
	if _, err := Run(context.Background(), fakeSource{bad: true}, f, token, ""); err == nil || f.switched {
		t.Fatal("错误分页未拒绝")
	}
}
