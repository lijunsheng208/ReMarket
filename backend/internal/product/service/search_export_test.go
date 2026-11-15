package service

import (
	"context"
	"remarket/backend/internal/product/model"
	"testing"
)

type exportFake struct{}

// SearchExportUpper 模拟稳定维护窗口的商品上界。
func (exportFake) SearchExportUpper(context.Context) (int64, error) { return 10, nil }

// ExportSearch 返回包括下架在内的曾发布记录。
func (exportFake) ExportSearch(context.Context, int64, int64, int) ([]model.Product, error) {
	return []model.Product{{ID: 2, Status: model.StatusOffShelf}, {ID: 5, Status: model.StatusSold}}, nil
}

// TestSearchExport 验证维护约束、分页上界和多取一条产生的游标。
func TestSearchExport(t *testing.T) {
	if _, _, _, err := NewSearchExporter(exportFake{}, false).Export(context.Background(), 0, 0, 1); err == nil {
		t.Fatal("维护外允许导出")
	}
	rows, upper, next, err := NewSearchExporter(exportFake{}, true).Export(context.Background(), 0, 0, 1)
	if err != nil || len(rows) != 1 || upper != 10 || next != 2 || rows[0].Status != model.StatusOffShelf {
		t.Fatal("快照分页不符")
	}
	if _, _, _, err := NewSearchExporter(exportFake{}, true).Export(context.Background(), 5, 0, 1); err == nil {
		t.Fatal("无上界后续页未拒绝")
	}
}
