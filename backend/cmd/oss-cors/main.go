// oss-cors 显式补充本地开发直传规则，保留 Bucket 已有 CORS 配置。
package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"os"
	"time"
)

// main 合并限定本机来源的直传规则；错误只输出分类，避免记录云端凭据。
func main() {
	if err := run(); err != nil {
		fmt.Println("OSS CORS 配置失败:", err)
		os.Exit(1)
	}
}

// run 只增加指定本地来源，不覆盖其他来源，不修改 Bucket ACL。
func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bucket := os.Getenv("OSS_BUCKET")
	if bucket == "" || os.Getenv("OSS_REGION") == "" {
		return fmt.Errorf("缺少 OSS 配置")
	}
	cfg := oss.LoadDefaultConfig().WithRegion(os.Getenv("OSS_REGION")).WithCredentialsProvider(credentials.NewEnvironmentVariableCredentialsProvider()).WithRetryMaxAttempts(1)
	if e := os.Getenv("OSS_ENDPOINT"); e != "" {
		cfg.WithEndpoint(e)
	}
	client := oss.NewClient(cfg)
	current, err := client.GetBucketCors(ctx, &oss.GetBucketCorsRequest{Bucket: oss.Ptr(bucket)})
	rules := &oss.CORSConfiguration{}
	if err != nil {
		var se *oss.ServiceError
		if !errors.As(err, &se) || se.Code != "NoSuchCORSConfiguration" {
			return fmt.Errorf("读取失败，请检查 GetBucketCORS 权限")
		}
	} else if current.CORSConfiguration != nil {
		rules = current.CORSConfiguration
	}
	origins := []string{"http://localhost:5173", "http://127.0.0.1:5173"}
	for _, origin := range origins {
		found := false
		for _, r := range rules.CORSRules {
			for _, v := range r.AllowedOrigins {
				if v == origin {
					for _, m := range r.AllowedMethods {
						if m == "PUT" {
							found = true
						}
					}
				}
			}
		}
		if !found {
			rules.CORSRules = append(rules.CORSRules, oss.CORSRule{AllowedOrigins: []string{origin}, AllowedMethods: []string{"PUT", "GET", "HEAD"}, AllowedHeaders: []string{"Content-Type"}, ExposeHeaders: []string{"ETag"}, MaxAgeSeconds: oss.Ptr(int64(600))})
		}
	}
	if len(rules.CORSRules) > 10 {
		return fmt.Errorf("CORS 规则数超过限制，未修改")
	}
	if _, err := client.PutBucketCors(ctx, &oss.PutBucketCorsRequest{Bucket: oss.Ptr(bucket), CORSConfiguration: rules}); err != nil {
		return fmt.Errorf("写入失败，请检查 PutBucketCORS 权限")
	}
	fmt.Println("OSS 本地开发 CORS 已合并")
	return nil
}
