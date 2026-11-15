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

// TestSearchProtocol 验证在售过滤、多取一条、精确游标、最新 PIT 及部分结果拒绝。
func TestSearchProtocol(t *testing.T) {
	for _, tc := range []struct {
		body string
		code int
		want error
	}{
		{`{"pit_id":"next","timed_out":false,"_shards":{"total":1,"successful":1,"failed":0},"hits":{"hits":[]}}`, 200, nil},
		{`{"pit_id":"next","timed_out":true,"_shards":{"total":1,"successful":1,"failed":0},"hits":{"hits":[]}}`, 200, ErrUnavailable},
		{`{"pit_id":"next","_shards":{"total":2,"successful":1,"failed":1},"hits":{"hits":[]}}`, 200, ErrUnavailable},
		{`{"error":{"type":"search_context_missing_exception"}}`, 404, ErrPITExpired},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.URL.Path != "/_search" || r.URL.Query().Get("allow_partial_search_results") != "false" || string(body["size"]) != "21" || !strings.Contains(string(body["query"]), "ON_SALE") || !strings.Contains(string(body["search_after"]), "9007199254740993") {
				t.Error("检索约束丢失")
			}
			w.WriteHeader(tc.code)
			_, _ = w.Write([]byte(tc.body))
		}))
		repo, _ := New(Config{Endpoint: server.URL, Index: "products_v1", Timeout: time.Second})
		_, err := repo.Search(context.Background(), Query{Size: 20, Sort: "newest"}, "pit", []json.RawMessage{json.RawMessage(`9007199254740993`)})
		if !errors.Is(err, tc.want) {
			t.Errorf("got %v want %v", err, tc.want)
		}
		repo.Close()
		server.Close()
	}
}
