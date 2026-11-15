package gateway

import (
	"context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"net/http/httptest"
	searchv1 "remarket/backend/gen/search/v1"
	"strings"
	"testing"
	"time"
)

type searchFake struct {
	calls int
	err   error
}

// SearchProducts 返回大整数或可控 RPC 错误以核对 HTTP 适配。
func (f *searchFake) SearchProducts(_ context.Context, _ *searchv1.SearchProductsRequest, _ ...grpc.CallOption) (*searchv1.SearchProductsResponse, error) {
	f.calls++
	return &searchv1.SearchProductsResponse{Products: []*searchv1.SearchProduct{{Id: 9007199254740993}}}, f.err
}

// TestSearchHTTP 验证严格参数解析、大整数 JSON 及现有错误映射。
func TestSearchHTTP(t *testing.T) {
	f := &searchFake{}
	router := NewRouter(nil, time.Second)
	AddSearchRoutes(router, f, time.Second)
	for _, query := range []string{"x=1", "page_size=1&page_size=2", "category_id=", "min_price_cent=-1", "page_size=2147483648", "category_id=9223372036854775808", "sort=", "query=%zz"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/search/products?"+query, nil))
		if w.Code != 400 {
			t.Errorf("%s: %d", query, w.Code)
		}
	}
	if f.calls != 0 {
		t.Fatal("非法参数触发 RPC")
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/search/products?query=phone", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"9007199254740993"`) {
		t.Fatal("JSON 精度丢失")
	}
	for code, httpCode := range map[codes.Code]int{codes.FailedPrecondition: 409, codes.Unavailable: 503, codes.DeadlineExceeded: 504, codes.Internal: 500} {
		f.err = status.Error(code, "secret")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/search/products", nil))
		if w.Code != httpCode {
			t.Errorf("%v: %d", code, w.Code)
		}
		if code == codes.Internal && strings.Contains(w.Body.String(), "secret") {
			t.Fatal("泄露内部错误")
		}
	}
}
