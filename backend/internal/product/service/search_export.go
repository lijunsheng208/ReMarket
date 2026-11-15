package service

import (
	"context"
	"remarket/backend/internal/product/model"
	"remarket/backend/pkg/apperror"
)

// SearchExportRepository 定义 Product 自有快照扫描，不暴露数据库给 Search。
type SearchExportRepository interface {
	SearchExportUpper(context.Context) (int64, error)
	ExportSearch(context.Context, int64, int64, int) ([]model.Product, error)
}

// SearchExporter 仅在进程写维护开关启用时提供导出。
type SearchExporter struct {
	repo   SearchExportRepository
	paused bool
}

// NewSearchExporter 注入本服务存储和启动时维护状态。
func NewSearchExporter(repo SearchExportRepository, paused bool) *SearchExporter {
	return &SearchExporter{repo, paused}
}

// Export 按固定 ID 上界扫描，多取一条决定下一页；维护窗口外拒绝快照承诺。
func (s *SearchExporter) Export(ctx context.Context, after, upper int64, size int) ([]model.Product, int64, int64, error) {
	if !s.paused {
		return nil, 0, 0, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "导出要求商品写维护窗口"}
	}
	if after < 0 || upper < 0 || size < 0 || size > 100 || upper > 0 && after > upper || upper == 0 && after != 0 {
		return nil, 0, 0, &apperror.Error{Kind: apperror.Invalid, Message: "导出分页参数无效"}
	}
	if size == 0 {
		size = 50
	}
	if upper == 0 {
		var err error
		upper, err = s.repo.SearchExportUpper(ctx)
		if err != nil {
			return nil, 0, 0, err
		}
	}
	rows, err := s.repo.ExportSearch(ctx, after, upper, size+1)
	if err != nil {
		return nil, 0, 0, err
	}
	var next int64
	if len(rows) > size {
		rows = rows[:size]
		next = rows[len(rows)-1].ID
	}
	return rows, upper, next, nil
}
