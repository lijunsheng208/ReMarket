package repository

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
)

// VerifyDocument 通过实时 GET 校验完整快照和外部版本，重建不只依赖写入成功。
func (r *Repository) VerifyDocument(ctx context.Context, d Document) error {
	code, data, err := r.request(ctx, http.MethodGet, "/"+r.index+"/_doc/"+strconv.FormatInt(d.ProductID, 10), nil)
	if err != nil {
		return err
	}
	var v struct {
		Found   bool     `json:"found"`
		Version int64    `json:"_version"`
		Source  Document `json:"_source"`
	}
	if code != 200 || json.Unmarshal(data, &v) != nil || !v.Found || v.Version != d.AggregateVersion {
		return ErrUnavailable
	}
	actual, _ := json.Marshal(v.Source)
	expected, _ := json.Marshal(d)
	if string(actual) != string(expected) {
		return ErrUnavailable
	}
	return nil
}

// VerifyCount 刷新后核对文档总数及分片成功，防止遗漏或多余历史记录。
func (r *Repository) VerifyCount(ctx context.Context, count int64) error {
	code, data, err := r.request(ctx, http.MethodPost, "/"+r.index+"/_refresh", nil)
	if err != nil {
		return err
	}
	var refresh struct {
		Shards struct{ Total, Successful, Failed int } `json:"_shards"`
	}
	if code != 200 || json.Unmarshal(data, &refresh) != nil || refresh.Shards.Total == 0 || refresh.Shards.Failed != 0 || refresh.Shards.Successful != refresh.Shards.Total {
		return ErrUnavailable
	}
	code, data, err = r.request(ctx, http.MethodGet, "/"+r.index+"/_count", nil)
	if err != nil {
		return err
	}
	var v struct {
		Count  int64                                   `json:"count"`
		Shards struct{ Total, Successful, Failed int } `json:"_shards"`
	}
	if code != 200 || json.Unmarshal(data, &v) != nil || v.Count != count || v.Shards.Total == 0 || v.Shards.Failed != 0 || v.Shards.Successful != v.Shards.Total {
		return ErrUnavailable
	}
	return nil
}

// SwitchReadAlias 校验旧目标唯一后原子移除/添加；需运维串行执行，不提供跨调用 CAS 保证。
// ACK 丢失时结果可能未知，调用方必须读取别名核查，不自动反向切换。
func (r *Repository) SwitchReadAlias(ctx context.Context, old string) error {
	if !indexName.MatchString(old) || old == r.index {
		return ErrInvalid
	}
	code, data, err := r.request(ctx, http.MethodGet, "/_alias/products_search", nil)
	if err != nil {
		return err
	}
	var aliases map[string]json.RawMessage
	if code != 200 || json.Unmarshal(data, &aliases) != nil || len(aliases) != 1 || aliases[old] == nil {
		return ErrInvalid
	}
	raw, _ := json.Marshal(map[string]any{"actions": []any{map[string]any{"remove": map[string]any{"index": old, "alias": "products_search", "must_exist": true}}, map[string]any{"add": map[string]any{"index": r.index, "alias": "products_search"}}}})
	code, data, err = r.request(ctx, http.MethodPost, "/_aliases", raw)
	if err != nil {
		return err
	}
	var ack struct {
		Acknowledged bool `json:"acknowledged"`
		Errors       bool `json:"errors"`
	}
	if code != 200 || json.Unmarshal(data, &ack) != nil || !ack.Acknowledged || ack.Errors {
		return ErrUnavailable
	}
	return nil
}
