package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"golang.org/x/sync/singleflight"
	pc "remarket/backend/internal/product/cache"
	"remarket/backend/internal/product/model"
	"remarket/backend/internal/product/repository"
	"remarket/backend/pkg/apperror"
)

// DetailCache 定义原始详情缓存，不包含授权结果。
type DetailCache interface {
	Get(context.Context, int64) (*pc.Detail, bool, error)
	Begin(context.Context, int64) (string, error)
	Deadline(time.Time, bool) (time.Time, error)
	Put(context.Context, int64, string, *pc.Detail, time.Time) (bool, error)
}

// SnapshotRepository 提供商品与图片的同一数据库快照。
type SnapshotRepository interface {
	GetDetailSnapshot(context.Context, int64) (*model.Product, []model.ProductImage, error)
}

// DetailLoadConfig 限制回源及等待容量；Observe 只能接收固定事件名且必须并发安全、不可阻塞。
type DetailLoadConfig struct {
	MaxConcurrent, MaxWaiting              int
	LoadTimeout, WaitTimeout, CacheTimeout time.Duration
	Observe                                func(string)
}

type detailLoader struct {
	cache           DetailCache
	repo            SnapshotRepository
	lifecycle       context.Context
	cfg             DetailLoadConfig
	group           singleflight.Group
	active, waiting chan struct{}
}

// NewCachedQuery 显式构造可测试的缓存查询服务；生产入口在可靠失效完成前不调用此构造器。
func NewCachedQuery(products QueryRepository, admins AdminVerifier, cache DetailCache, lifecycle context.Context, cfg DetailLoadConfig) (*QueryService, error) {
	repo, ok := products.(SnapshotRepository)
	if !ok || cache == nil || lifecycle == nil || cfg.MaxConcurrent <= 0 || cfg.MaxWaiting <= 0 || cfg.LoadTimeout <= 0 || cfg.WaitTimeout <= 0 || cfg.CacheTimeout <= 0 {
		return nil, fmt.Errorf("详情缓存依赖或容量配置无效")
	}
	s := NewQuery(products, admins)
	s.cached = &detailLoader{cache: cache, repo: repo, lifecycle: lifecycle, cfg: cfg, active: make(chan struct{}, cfg.MaxConcurrent), waiting: make(chan struct{}, cfg.MaxWaiting)}
	return s, nil
}

// observe 记录固定类别，不暴露详情、凭据或底层错误内容。
func (l *detailLoader) observe(event string) {
	if l.cfg.Observe != nil {
		l.cfg.Observe(event)
	}
}

// read 对缓存访问施加短超时，Redis 故障与真正未命中分别记录。
func (l *detailLoader) read(ctx context.Context, id int64) (*pc.Detail, bool) {
	short, cancel := context.WithTimeout(ctx, l.cfg.CacheTimeout)
	defer cancel()
	d, hit, err := l.cache.Get(short, id)
	if err != nil {
		l.observe("cache_read_error")
		return nil, false
	}
	return d, hit
}

// get 限制等待总量并合并本进程回源；每个调用者可独立取消，最多重试一次共享取消。
func (l *detailLoader) get(ctx context.Context, id int64) (*ProductDetail, error) {
	if err := l.lifecycle.Err(); err != nil {
		return nil, err
	}
	if d, hit := l.read(ctx, id); hit {
		return fromCache(d)
	}
	select {
	case l.waiting <- struct{}{}:
		defer func() { <-l.waiting }()
	default:
		return nil, busy()
	}
	for attempt := 0; attempt < 2; attempt++ {
		// 首个调用者的 Context 约束共享加载，服务退出同样取消它，不脱离上游继续读取。
		ch := l.group.DoChan(strconv.FormatInt(id, 10), func() (any, error) {
			load, cancel := context.WithTimeout(ctx, l.cfg.LoadTimeout)
			defer cancel()
			stop := context.AfterFunc(l.lifecycle, cancel)
			defer stop()
			return l.load(load, id)
		})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-l.lifecycle.Done():
			return nil, l.lifecycle.Err()
		case result := <-ch:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if result.Err != nil {
				if errors.Is(result.Err, context.Canceled) || errors.Is(result.Err, context.DeadlineExceeded) {
					continue
				}
				return nil, result.Err
			}
			return fromCache(result.Val.(*pc.Detail))
		}
	}
	return nil, &apperror.Error{Kind: apperror.Unavailable, Message: "商品查询暂不可用，请重试"}
}

// busy 返回不含内部连接信息的容量错误。
func busy() error {
	return &apperror.Error{Kind: apperror.Unavailable, Message: "商品查询繁忙，请稍后重试"}
}

// load 在组内二次检查后受限回源；只有明确物理不存在才能写负缓存。
func (l *detailLoader) load(ctx context.Context, id int64) (*pc.Detail, error) {
	if d, hit := l.read(ctx, id); hit {
		return d, nil
	}
	wait, cancel := context.WithTimeout(ctx, l.cfg.WaitTimeout)
	select {
	case l.active <- struct{}{}:
		cancel()
		defer func() { <-l.active }()
	case <-wait.Done():
		cancel()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, busy()
	}
	short, stop := context.WithTimeout(ctx, l.cfg.CacheTimeout)
	epoch, err := l.cache.Begin(short, id)
	stop()
	canFill := err == nil
	if !canFill {
		l.observe("cache_begin_error")
	}
	start := time.Now()
	product, images, err := l.repo.GetDetailSnapshot(ctx, id)
	if err != nil && !errors.Is(err, repository.ErrProductNotFound) {
		return nil, err
	}
	var d *pc.Detail
	if err == nil {
		d = toCache(product, images)
	}
	if canFill && ctx.Err() == nil {
		deadline, err := l.cache.Deadline(start, d == nil)
		if err != nil {
			l.observe("cache_deadline_error")
		} else {
			short, stop := context.WithTimeout(ctx, l.cfg.CacheTimeout)
			ok, err := l.cache.Put(short, id, epoch, d, deadline)
			stop()
			if err != nil {
				l.observe("cache_write_error")
			} else if !ok {
				l.observe("cache_fill_rejected")
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d, nil
}

// toCache 将数据库模型转换为不含权限的缓存快照。
func toCache(p *model.Product, images []model.ProductImage) *pc.Detail {
	d := &pc.Detail{ProductID: p.ID, Version: p.AggregateVersion, SellerID: p.SellerID, CategoryID: p.CategoryID, PriceCent: p.PriceCent, Title: p.Title, Description: p.Description, Status: string(p.Status), FirstPublishedAt: p.FirstPublishedAt, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, Images: make([]pc.Image, 0, len(images))}
	for _, img := range images {
		d.Images = append(d.Images, pc.Image{ID: img.ID, ObjectKey: img.ObjectKey, SortOrder: img.SortOrder, CreatedAt: img.CreatedAt})
	}
	return d
}

// fromCache 为每个调用者复制商品、可空时间与图片，避免共享结果被修改。
func fromCache(d *pc.Detail) (*ProductDetail, error) {
	if d == nil {
		return nil, repository.ErrProductNotFound
	}
	p := &model.Product{ID: d.ProductID, AggregateVersion: d.Version, SellerID: d.SellerID, CategoryID: d.CategoryID, PriceCent: d.PriceCent, Title: d.Title, Description: d.Description, Status: model.ProductStatus(d.Status), CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
	if d.FirstPublishedAt != nil {
		t := *d.FirstPublishedAt
		p.FirstPublishedAt = &t
	}
	images := make([]model.ProductImage, 0, len(d.Images))
	for _, img := range d.Images {
		images = append(images, model.ProductImage{ID: img.ID, ProductID: d.ProductID, ObjectKey: img.ObjectKey, SortOrder: img.SortOrder, CreatedAt: img.CreatedAt})
	}
	return &ProductDetail{Product: p, Images: images}, nil
}
