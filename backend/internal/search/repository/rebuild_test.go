package repository

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestAliasSwitch 验证唯一旧目标检查、原子动作与 must_exist，失败不发送切换请求。
func TestAliasSwitch(t *testing.T) {
	for _, valid := range []bool{true, false} {
		switched := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				if valid {
					_, _ = w.Write([]byte(`{"products_v1":{"aliases":{"products_search":{}}}}`))
				} else {
					_, _ = w.Write([]byte(`{"products_v3":{}}`))
				}
				return
			}
			var body struct {
				Actions []map[string]map[string]any `json:"actions"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Actions) != 2 || body.Actions[0]["remove"]["must_exist"] != true || body.Actions[1]["add"]["index"] != "products_v2" {
				t.Error("切换动作无效")
			}
			switched = true
			_, _ = w.Write([]byte(`{"acknowledged":true,"errors":false}`))
		}))
		repo, _ := New(Config{Endpoint: server.URL, Index: "products_v2", Timeout: time.Second})
		err := repo.SwitchReadAlias(context.Background(), "products_v1")
		if (err == nil) != valid || switched != valid {
			t.Fatal("旧目标不匹配仍切换")
		}
		repo.Close()
		server.Close()
	}
}
