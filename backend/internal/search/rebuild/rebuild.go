// Package rebuild 在受控维护窗口经 Product RPC 重建 Search 索引。
package rebuild

import (
	"context"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/search/repository"
)

// Source 仅通过服务 RPC 获取 Product 自有数据。
type Source interface {
	ExportSearchSnapshot(context.Context, *productv1.ExportSearchSnapshotRequest, ...grpc.CallOption) (*productv1.ExportSearchSnapshotResponse, error)
}

// Target 定义构建、逐条验证与受控别名切换。
type Target interface {
	CreateIndex(context.Context) error
	Put(context.Context, repository.Document) (bool, error)
	VerifyDocument(context.Context, repository.Document) error
	VerifyCount(context.Context, int64) error
	SwitchReadAlias(context.Context, string) error
}

// Run 构建阶段要求全新目标；切换阶段重新导出并逐条比较，最后才修改别名。
// 不自动暂停消费者或恢复商品写入；这些跨进程维护前提由操作规程保证。
func Run(ctx context.Context, source Source, target Target, token string, switchFrom string) (int64, error) {
	if len(token) < 32 {
		return 0, fmt.Errorf("需要独立导出凭据")
	}
	if switchFrom == "" {
		if err := target.CreateIndex(ctx); err != nil {
			return 0, err
		}
	}
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	var after, upper, total int64
	for {
		page, err := source.ExportSearchSnapshot(ctx, &productv1.ExportSearchSnapshotRequest{AfterId: after, UpperId: upper, PageSize: 50})
		if err != nil {
			return total, err
		}
		if page == nil || page.UpperId < 0 || upper != 0 && upper != page.UpperId || page.NextAfterId < 0 || len(page.Products) > 50 {
			return total, fmt.Errorf("导出分页响应无效")
		}
		upper = page.UpperId
		last := after
		for _, p := range page.Products {
			if p == nil || p.Id <= last || p.Id > upper || p.FirstPublishedAt == nil || p.FirstPublishedAt.CheckValid() != nil {
				return total, fmt.Errorf("导出商品无效")
			}
			last = p.Id
			state := map[productv1.ProductStatus]string{2: "ON_SALE", 3: "RESERVED", 4: "SOLD", 5: "OFF_SHELF"}[p.Status]
			d := repository.Document{ProductID: p.Id, SellerID: p.SellerId, CategoryID: p.CategoryId, Title: p.Title, Description: p.Description, PriceCent: p.PriceCent, AggregateVersion: p.AggregateVersion, Status: state, FirstPublishedAt: p.FirstPublishedAt.AsTime()}
			if err := d.Validate(); err != nil {
				return total, err
			}
			if switchFrom == "" {
				written, err := target.Put(ctx, d)
				if err != nil {
					return total, err
				}
				if !written {
					return total, fmt.Errorf("新索引出现版本冲突，停止重建")
				}
			}
			if err := target.VerifyDocument(ctx, d); err != nil {
				return total, err
			}
			total++
		}
		if page.NextAfterId == 0 {
			break
		}
		if page.NextAfterId != last || last <= after || last >= upper {
			return total, fmt.Errorf("导出游标未推进")
		}
		after = last
	}
	if err := target.VerifyCount(ctx, total); err != nil {
		return total, err
	}
	if switchFrom != "" {
		if err := target.SwitchReadAlias(ctx, switchFrom); err != nil {
			return total, err
		}
	}
	return total, nil
}
