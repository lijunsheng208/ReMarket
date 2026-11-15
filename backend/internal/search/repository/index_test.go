package repository

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPutVersions 验证大整数、严格外部版本及非版本冲突不得被吞掉。
func TestPutVersions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		body    string
		written bool
		want    error
	}{
		{"created", 201, `{"_id":"9007199254740993","_version":3,"result":"created"}`, true, nil},
		{"stale", 409, `{"error":{"type":"version_conflict_engine_exception"}}`, false, nil},
		{"other conflict", 409, `{"error":{"type":"other","reason":"secret"}}`, false, ErrUnavailable},
		{"overload", 429, `secret`, false, ErrUnavailable},
		{"malformed success", 201, `{}`, false, ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "PUT" || r.URL.Path != "/products_v1/_doc/9007199254740993" || r.URL.Query().Get("version_type") != "external" || r.URL.Query().Get("version") != "3" {
					t.Errorf("错误写入目标: %s", r.URL)
				}
				var d Document
				if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.ProductID != 9007199254740993 || d.Status != "OFF_SHELF" {
					t.Error("快照精度或状态丢失")
				}
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			repo, err := New(Config{Endpoint: s.URL, Index: "products_v1", Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			ok, err := repo.Put(context.Background(), Document{ProductID: 9007199254740993, SellerID: 1, CategoryID: 1, Title: "手机", PriceCent: 100, AggregateVersion: 3, Status: "OFF_SHELF", FirstPublishedAt: time.Now()})
			if ok != tc.written || !errors.Is(err, tc.want) {
				t.Fatalf("got %v %v", ok, err)
			}
		})
	}
}

// TestCreateIndex 验证固定 Mapping、重复建索引及未确认响应的处理。
func TestCreateIndex(t *testing.T) {
	for _, tc := range []struct {
		code int
		body string
		want error
	}{
		{200, `{"acknowledged":true,"shards_acknowledged":true}`, nil},
		{200, `{"acknowledged":true,"shards_acknowledged":false}`, ErrUnavailable},
		{400, `{"error":{"type":"resource_already_exists_exception"}}`, ErrExists},
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Mappings struct {
					Dynamic    string                     `json:"dynamic"`
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"mappings"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Mappings.Dynamic != "strict" || len(body.Mappings.Properties) != 9 {
				t.Error("索引定义不符")
			}
			w.WriteHeader(tc.code)
			_, _ = w.Write([]byte(tc.body))
		}))
		repo, err := New(Config{Endpoint: s.URL, Index: "products_v1", Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.CreateIndex(context.Background()); !errors.Is(err, tc.want) {
			t.Errorf("got %v", err)
		}
		repo.Close()
		s.Close()
	}
}

// TestContextAndConfig 验证上游取消和地址注入限制，避免错误泄露敏感 URL。
func TestContextAndConfig(t *testing.T) {
	for _, endpoint := range []string{"http://user:secret@localhost", "http://localhost/path", "http://localhost?x=secret"} {
		_, err := New(Config{Endpoint: endpoint, Index: "products_v1", Timeout: time.Second})
		if !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "secret") {
			t.Fatal("地址未被安全拒绝")
		}
	}
	repo, err := New(Config{Endpoint: "http://127.0.0.1:1", Index: "products_v1", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := repo.CreateIndex(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消语义丢失: %v", err)
	}
}

// TestServerIdentity 验证误连 OpenSearch 时拒绝启动，且不会向错误实例写入。
func TestServerIdentity(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"version":{"number":"8.19.25"},"tagline":"You Know, for Search"}`, true},
		{`{"version":{"number":"2.19.1","distribution":"opensearch"},"tagline":"The OpenSearch Project"}`, false},
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/" {
				t.Error("身份探测产生写入")
			}
			_, _ = w.Write([]byte(tc.body))
		}))
		repo, _ := New(Config{Endpoint: s.URL, Index: "products_v1", Timeout: time.Second})
		err := repo.CheckServer(context.Background())
		if (err == nil) != tc.valid {
			t.Errorf("错误实例识别失败: %v", err)
		}
		repo.Close()
		s.Close()
	}
}
