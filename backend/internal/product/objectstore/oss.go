// Package objectstore 适配阿里云 OSS 私有图片存储，业务层不依赖云厂商 SDK。
package objectstore

import (
	"bytes"
	"context"
	"fmt"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"io"
	"net/http"
	"os"
	"time"
)

type Store struct {
	client *oss.Client
	bucket string
}

// Open 使用 OSS 环境凭据并核实已有 Bucket 为私有，不自动创建云资源或改写现有访问策略。
func Open(ctx context.Context) (*Store, error) {
	region, bucket := os.Getenv("OSS_REGION"), os.Getenv("OSS_BUCKET")
	if region == "" || bucket == "" {
		return nil, fmt.Errorf("OSS 地域和 Bucket 未配置")
	}
	cfg := oss.LoadDefaultConfig().WithRegion(region).WithCredentialsProvider(credentials.NewEnvironmentVariableCredentialsProvider()).WithRetryMaxAttempts(1).WithHttpClient(&http.Client{Timeout: 10 * time.Second})
	if endpoint := os.Getenv("OSS_ENDPOINT"); endpoint != "" {
		cfg.WithEndpoint(endpoint)
	}
	client := oss.NewClient(cfg)
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	acl, err := client.GetBucketAcl(probe, &oss.GetBucketAclRequest{Bucket: oss.Ptr(bucket)})
	if err != nil {
		return nil, fmt.Errorf("OSS Bucket 权限检查失败")
	}
	if oss.ToString(acl.ACL) != "private" {
		return nil, fmt.Errorf("OSS 图片 Bucket 必须设为私有")
	}
	return &Store{client, bucket}, nil
}

// PresignPut 签发五分钟临时对象 PUT 地址，正式入库前仍需服务端检查真实内容。
func (s *Store) PresignPut(ctx context.Context, key, contentType string) (string, error) {
	result, err := s.client.Presign(ctx, &oss.PutObjectRequest{Bucket: oss.Ptr(s.bucket), Key: oss.Ptr(key), ContentType: oss.Ptr(contentType)}, func(o *oss.PresignOptions) { o.Expires = 5 * time.Minute })
	if err != nil {
		return "", fmt.Errorf("OSS 上传签名失败")
	}
	return result.URL, nil
}

// Read 限制对象大小并及时关闭响应体，防止恶意文件占用过多内存。
func (s *Store) Read(ctx context.Context, key string, max int64) ([]byte, error) {
	result, err := s.client.GetObject(ctx, &oss.GetObjectRequest{Bucket: oss.Ptr(s.bucket), Key: oss.Ptr(key)})
	if err != nil {
		return nil, fmt.Errorf("上传对象不可读")
	}
	defer result.Body.Close()
	if result.ContentLength <= 0 || result.ContentLength > max {
		return nil, fmt.Errorf("上传对象大小超限")
	}
	data, err := io.ReadAll(io.LimitReader(result.Body, max+1))
	if err != nil || int64(len(data)) > max {
		return nil, fmt.Errorf("上传对象读取失败")
	}
	return data, nil
}

// Put 写入服务端重新编码的独立正式对象，禁止覆盖已有键。
func (s *Store) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, &oss.PutObjectRequest{Bucket: oss.Ptr(s.bucket), Key: oss.Ptr(key), Body: bytes.NewReader(data), ContentType: oss.Ptr(contentType), ContentLength: oss.Ptr(int64(len(data))), ForbidOverwrite: oss.Ptr("true")})
	if err != nil {
		return fmt.Errorf("OSS 图片保存失败")
	}
	return nil
}

// PresignGet 返回五分钟只读地址，调用前必须经过商品可见性校验。
func (s *Store) PresignGet(ctx context.Context, key string) (string, error) {
	result, err := s.client.Presign(ctx, &oss.GetObjectRequest{Bucket: oss.Ptr(s.bucket), Key: oss.Ptr(key)}, func(o *oss.PresignOptions) { o.Expires = 5 * time.Minute })
	if err != nil {
		return "", fmt.Errorf("OSS 图片签名失败")
	}
	return result.URL, nil
}
