package objectstore

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestOSSPrivateBucket 验证真实凭据可读取私有 Bucket ACL，不输出凭据或签名 URL。
func TestOSSPrivateBucket(t *testing.T) {
	if os.Getenv("OSS_INTEGRATION") != "1" {
		t.Skip("需要 OSS_INTEGRATION=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := Open(ctx); err != nil {
		t.Fatal(err)
	}
}
