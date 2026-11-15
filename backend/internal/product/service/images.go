package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"net/http"
	"remarket/backend/internal/product/events"
	"remarket/backend/internal/product/model"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/auth"
	"time"
)

const MaxImageBytes int64 = 5 << 20

// ImageObjects 定义私有对象存储能力，不向业务暴露云厂商 SDK。
type ImageObjects interface {
	PresignPut(context.Context, string, string) (string, error)
	Read(context.Context, string, int64) ([]byte, error)
	Put(context.Context, string, []byte, string) error
	PresignGet(context.Context, string) (string, error)
}
type ImageRepository interface {
	GetByID(context.Context, int64) (*model.Product, error)
	ListImages(context.Context, int64) ([]model.ProductImage, error)
	CreateImageUpload(context.Context, *model.ImageUpload, func(*model.Product) error) error
	GetImageUpload(context.Context, string) (*model.ImageUpload, error)
	CommitImage(context.Context, *model.ImageUpload, *model.ProductImage, func(*model.Product, *model.ImageUpload, int64) (*model.OutboxEvent, error)) (*model.Product, string, error)
}
type Images struct {
	repo         ImageRepository
	objects      ImageObjects
	invalidation PostCommit
	admins       AdminVerifier
}

// NewImages 注入商品自有数据和私有对象存储，所有权校验仍在业务层完成。
func NewImages(repo ImageRepository, objects ImageObjects, admins AdminVerifier, hook PostCommit) *Images {
	return &Images{repo, objects, hook, admins}
}

// imageError 将可公开的参数错误统一映射，不泄露云端请求凭据。
func imageError(message string) error {
	return &apperror.Error{Kind: apperror.Invalid, Message: message}
}

// imageOwner 只允许卖家修改可编辑状态，预占及成交后禁止图片变更。
func imageOwner(ctx context.Context, p *model.Product) error {
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok {
		return &apperror.Error{Kind: apperror.Unauthenticated, Message: "请先登录"}
	}
	if p.SellerID != identity.UserID {
		return &apperror.Error{Kind: apperror.Forbidden, Message: "只能修改自己的商品图片"}
	}
	if p.Status != model.StatusDraft && p.Status != model.StatusOnSale && p.Status != model.StatusOffShelf {
		return &apperror.Error{Kind: apperror.FailedPrecondition, Message: "当前商品状态不允许修改图片"}
	}
	return nil
}

// randomImageID 生成不可预测的对象与会话标识，不使用用户文件名构造路径。
func randomImageID() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// Prepare 保存五分钟受限上传会话，客户端只能向该会话的临时对象写入。
func (s *Images) Prepare(ctx context.Context, id, size int64, contentType string) (*model.ImageUpload, string, error) {
	if id <= 0 || size <= 0 || size > MaxImageBytes || (contentType != "image/jpeg" && contentType != "image/png") {
		return nil, "", imageError("图片仅支持 JPEG/PNG，大小不得超过 5MB")
	}
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok {
		return nil, "", &apperror.Error{Kind: apperror.Unauthenticated, Message: "请先登录"}
	}
	nonce, err := randomImageID()
	if err != nil {
		return nil, "", err
	}
	u := &model.ImageUpload{ID: nonce, ProductID: id, OwnerID: identity.UserID, SizeBytes: size, ContentType: contentType, ExpiresAt: time.Now().UTC().Add(5 * time.Minute)}
	if err := s.repo.CreateImageUpload(ctx, u, func(p *model.Product) error { return imageOwner(ctx, p) }); err != nil {
		return nil, "", err
	}
	url, err := s.objects.PresignPut(ctx, "staging/"+nonce, contentType)
	return u, url, err
}

// validateImage 校验完整解码与像素上限，再重新编码以去掉尾随脚本和多余元数据。
func validateImage(data []byte, size int64, mime string) ([]byte, int, int, error) {
	if int64(len(data)) != size || http.DetectContentType(data) != mime {
		return nil, 0, 0, imageError("图片大小或真实格式与上传声明不一致")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > 8192 || config.Height > 8192 || int64(config.Width)*int64(config.Height) > 16000000 {
		return nil, 0, 0, imageError("图片无效或像素超过限制")
	}
	if (mime == "image/jpeg" && format != "jpeg") || (mime == "image/png" && format != "png") {
		return nil, 0, 0, imageError("图片真实格式不匹配")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, imageError("图片内容损坏")
	}
	var out bytes.Buffer
	if mime == "image/jpeg" {
		err = jpeg.Encode(&out, decoded, &jpeg.Options{Quality: 90})
	} else {
		err = png.Encode(&out, decoded)
	}
	if err != nil || int64(out.Len()) > MaxImageBytes {
		return nil, 0, 0, imageError("图片处理后超过大小限制")
	}
	return out.Bytes(), config.Width, config.Height, nil
}

// detailFresh 直接读取最新图片元数据，写入响应不依赖可能尚未失效的详情缓存。
func (s *Images) detailFresh(ctx context.Context, p *model.Product) (*ProductDetail, error) {
	images, err := s.repo.ListImages(ctx, p.ID)
	return &ProductDetail{Product: p, Images: images}, err
}

// Complete 验证临时对象后写独立正式键，锁内提交图片和版本；重复完成不覆盖已确认对象。
func (s *Images) Complete(ctx context.Context, id int64, uploadID string) (*ProductDetail, error) {
	if id <= 0 || len(uploadID) != 48 {
		return nil, imageError("上传会话无效")
	}
	u, err := s.repo.GetImageUpload(ctx, uploadID)
	if err != nil {
		return nil, err
	}
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok || identity.UserID != u.OwnerID || id != u.ProductID {
		return nil, &apperror.Error{Kind: apperror.Forbidden, Message: "上传会话不属于当前用户或商品"}
	}
	p, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := imageOwner(ctx, p); err != nil {
		return nil, err
	}
	if u.CompletedKey != "" {
		return s.detailFresh(ctx, p)
	}
	if time.Now().After(u.ExpiresAt) {
		return nil, imageError("上传会话已过期，请重新选择图片")
	}
	data, err := s.objects.Read(ctx, "staging/"+u.ID, MaxImageBytes)
	if err != nil {
		return nil, imageError("请先完成图片上传，或检查文件大小")
	}
	encoded, width, height, err := validateImage(data, u.SizeBytes, u.ContentType)
	if err != nil {
		return nil, err
	}
	nonce, err := randomImageID()
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("images/%d/%s", id, nonce)
	if err := s.objects.Put(ctx, key, encoded, u.ContentType); err != nil {
		return nil, err
	}
	meta := &model.ProductImage{ObjectKey: key, ContentType: u.ContentType, SizeBytes: int64(len(encoded)), Width: width, Height: height}
	p, _, err = s.repo.CommitImage(ctx, u, meta, func(p *model.Product, current *model.ImageUpload, count int64) (*model.OutboxEvent, error) {
		if err := imageOwner(ctx, p); err != nil {
			return nil, err
		}
		if time.Now().After(current.ExpiresAt) || current.OwnerID != identity.UserID {
			return nil, imageError("上传会话已失效")
		}
		if count >= 9 {
			return nil, imageError("每件商品最多 9 张图片")
		}
		if p.AggregateVersion == math.MaxInt64 {
			return nil, imageError("商品版本已达上限")
		}
		p.AggregateVersion++
		p.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
		if p.Status == model.StatusOnSale {
			return events.NewUpdatedOutbox(*p, p.UpdatedAt)
		}
		return nil, nil
	})
	notifyCommit(ctx, s.invalidation, id, err)
	if err != nil {
		return nil, err
	}
	return s.detailFresh(ctx, p)
}

type ImageURL struct {
	ID        int64
	SortOrder int32
	URL       string
}

// Access 从 MySQL 重新判断商品可见性后签发五分钟只读 URL，不接受任意对象键。
func (s *Images) Access(ctx context.Context, id int64) ([]ImageURL, error) {
	if id <= 0 {
		return nil, imageError("商品 ID 无效")
	}
	p, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	query := &QueryService{admins: s.admins}
	if err := query.visible(ctx, p); err != nil {
		return nil, err
	}
	images, err := s.repo.ListImages(ctx, id)
	if err != nil {
		return nil, err
	}
	result := make([]ImageURL, 0, len(images))
	for _, image := range images {
		url, err := s.objects.PresignGet(ctx, image.ObjectKey)
		if err != nil {
			return nil, err
		}
		result = append(result, ImageURL{ID: image.ID, SortOrder: image.SortOrder, URL: url})
	}
	return result, nil
}
