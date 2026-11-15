package repository

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

var ErrPITExpired = errors.New("搜索快照已失效")

// Query 保存已规范化的搜索条件，Sort 使用 relevance/newest/price_asc/price_desc。
type Query struct {
	Text                           string
	CategoryID, MinPrice, MaxPrice *int64
	Sort                           string
	Size                           int
}

// Hit 保留原始 sort JSON，避免经 float64 损失大整数精度。
type Hit struct {
	Document Document          `json:"_source"`
	Sort     []json.RawMessage `json:"sort"`
}

// Page 包含当前 PIT ID 和多取一条的结果。
type Page struct {
	PIT  string
	Hits []Hit
}

// OpenPIT 从固定读别名创建短期快照，不隐式修改别名。
func (r *Repository) OpenPIT(ctx context.Context) (string, error) {
	code, data, err := r.request(ctx, http.MethodPost, "/products_search/_pit?keep_alive=60s&allow_partial_search_results=false", nil)
	if err != nil {
		return "", err
	}
	var v struct {
		ID string `json:"id"`
	}
	if code != 200 || json.Unmarshal(data, &v) != nil || v.ID == "" {
		return "", ErrUnavailable
	}
	return v.ID, nil
}

// ClosePIT 尽力释放快照；取消沿用上游，未释放部分依赖 ES 超时回收。
func (r *Repository) ClosePIT(ctx context.Context, id string) {
	raw, _ := json.Marshal(map[string]string{"id": id})
	_, _, _ = r.request(ctx, http.MethodDelete, "/_pit", raw)
}

// Search 强制在售过滤及稳定排序，拒绝超时或部分分片结果。
func (r *Repository) Search(ctx context.Context, q Query, pit string, after []json.RawMessage) (Page, error) {
	filters := []any{map[string]any{"term": map[string]any{"status": "ON_SALE"}}}
	if q.CategoryID != nil {
		filters = append(filters, map[string]any{"term": map[string]any{"category_id": *q.CategoryID}})
	}
	bounds := map[string]int64{}
	if q.MinPrice != nil {
		bounds["gte"] = *q.MinPrice
	}
	if q.MaxPrice != nil {
		bounds["lte"] = *q.MaxPrice
	}
	if len(bounds) > 0 {
		filters = append(filters, map[string]any{"range": map[string]any{"price_cent": bounds}})
	}
	boolean := map[string]any{"filter": filters}
	if q.Text != "" {
		boolean["must"] = map[string]any{"multi_match": map[string]any{"query": q.Text, "fields": []string{"title^3", "description"}, "type": "best_fields", "operator": "and"}}
	}
	var primary any
	switch q.Sort {
	case "relevance":
		primary = map[string]string{"_score": "desc"}
	case "newest":
		primary = map[string]any{"first_published_at": map[string]string{"order": "desc", "format": "strict_date_optional_time_nanos"}}
	case "price_asc":
		primary = map[string]string{"price_cent": "asc"}
	case "price_desc":
		primary = map[string]string{"price_cent": "desc"}
	default:
		return Page{}, ErrInvalid
	}
	body := map[string]any{"size": q.Size + 1, "track_total_hits": false, "query": map[string]any{"bool": boolean}, "sort": []any{primary, map[string]string{"product_id": "desc"}, map[string]string{"_shard_doc": "asc"}}, "pit": map[string]string{"id": pit, "keep_alive": "60s"}}
	if len(after) > 0 {
		body["search_after"] = after
	}
	raw, _ := json.Marshal(body)
	code, data, err := r.request(ctx, http.MethodPost, "/_search?allow_partial_search_results=false", raw)
	if err != nil {
		return Page{}, err
	}
	if code == 404 && (errorType(data) == "search_context_missing_exception" || errorType(data) == "resource_not_found_exception") {
		return Page{}, ErrPITExpired
	}
	var result struct {
		PIT      string `json:"pit_id"`
		TimedOut bool   `json:"timed_out"`
		Shards   struct {
			Failed     int `json:"failed"`
			Total      int `json:"total"`
			Successful int `json:"successful"`
		} `json:"_shards"`
		Hits struct {
			Hits []Hit `json:"hits"`
		} `json:"hits"`
	}
	if code != 200 || json.Unmarshal(data, &result) != nil || result.TimedOut || result.Shards.Failed != 0 || result.Shards.Total == 0 || result.Shards.Successful != result.Shards.Total || result.PIT == "" {
		return Page{}, ErrUnavailable
	}
	for _, h := range result.Hits.Hits {
		if h.Document.Validate() != nil || h.Document.Status != "ON_SALE" || len(h.Sort) != 3 {
			return Page{}, ErrUnavailable
		}
	}
	return Page{PIT: result.PIT, Hits: result.Hits.Hits}, nil
}
