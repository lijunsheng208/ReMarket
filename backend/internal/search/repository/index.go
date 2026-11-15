// Package repository 实现 Search 自有索引访问，不读取 Product 数据库。
package repository

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

//go:embed index.json
var indexDefinition []byte

var indexName = regexp.MustCompile(`^products_v[1-9][0-9]*$`)

var ErrUnavailable = errors.New("搜索存储不可用")
var ErrInvalid = errors.New("搜索索引参数无效")
var ErrExists = errors.New("搜索索引已存在")

// Config 指定独立物理写目标；HTTP 明文及零副本仅适用于本地开发。
type Config struct {
	Endpoint string
	Index    string
	Timeout  time.Duration
	Username string
	Password string
}

// Repository 持有专用连接池；调用方退出时负责 Close。
type Repository struct {
	endpoint           string
	index              string
	timeout            time.Duration
	username, password string
	client             *http.Client
}

// New 创建有界超时的 ES REST 客户端，不隐式建索引或探测，不接受 URL 中的凭据。
func New(c Config) (*Repository, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || !indexName.MatchString(c.Index) || c.Timeout <= 0 || (c.Username == "") != (c.Password == "") {
		return nil, ErrInvalid
	}
	if c.Username != "" && u.Scheme != "https" {
		return nil, ErrInvalid
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxConnsPerHost = 10
	return &Repository{endpoint: u.Scheme + "://" + u.Host, index: c.Index, timeout: c.Timeout, username: c.Username, password: c.Password, client: &http.Client{Transport: tr, CheckRedirect: rejectRedirect}}, nil
}

// rejectRedirect 禁止跟随重定向，防止凭据被转发或写请求改变目标。
func rejectRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

// Close 释放空闲连接；正在执行的请求仍由原 Context 控制。
func (r *Repository) Close() { r.client.CloseIdleConnections() }

// request 从上游派生超时，限制响应大小并关闭 Body；错误不携带 URL、正文或凭据。
func (r *Repository) request(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, r.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, ErrInvalid
	}
	req.Header.Set("Content-Type", "application/json")
	if r.username != "" {
		req.SetBasicAuth(r.username, r.password)
	}
	res, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, ErrUnavailable
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20+1))
	if err != nil || len(data) > 4<<20 {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, ErrUnavailable
	}
	return res.StatusCode, data, nil
}

// errorType 只提取机器错误类型，不传播可能包含敏感信息的 reason。
func errorType(data []byte) string {
	var v struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &v)
	return v.Error.Type
}

// CreateIndex 显式创建固定 Mapping，不覆盖已存在索引、不自动绑定或切换读别名。
// acknowledged 不完整时可能已创建，调用方应人工核查，不自动删除重建。
func (r *Repository) CreateIndex(ctx context.Context) error {
	code, data, err := r.request(ctx, http.MethodPut, "/"+r.index, indexDefinition)
	if err != nil {
		return err
	}
	if code == 400 && errorType(data) == "resource_already_exists_exception" {
		return ErrExists
	}
	if code != 200 {
		return ErrUnavailable
	}
	var result struct {
		Acknowledged bool `json:"acknowledged"`
		Shards       bool `json:"shards_acknowledged"`
	}
	if json.Unmarshal(data, &result) != nil || !result.Acknowledged || !result.Shards {
		return ErrUnavailable
	}
	return nil
}

// Document 是 Search 自有快照；金额、ID 与版本作为 JSON 整数精确编码。
type Document struct {
	ProductID        int64     `json:"product_id"`
	SellerID         int64     `json:"seller_id"`
	CategoryID       int64     `json:"category_id"`
	Title            string    `json:"title"`
	Description      string    `json:"description"`
	PriceCent        int64     `json:"price_cent"`
	AggregateVersion int64     `json:"aggregate_version"`
	Status           string    `json:"status"`
	FirstPublishedAt time.Time `json:"first_published_at"`
}

// Validate 限制搜索快照边界；不可售状态仍保留，草稿不允许进入索引。
func (d Document) Validate() error {
	if d.ProductID <= 0 || d.SellerID <= 0 || d.CategoryID <= 0 || d.PriceCent <= 0 || d.AggregateVersion < 2 || !utf8.ValidString(d.Title) || strings.TrimSpace(d.Title) == "" || utf8.RuneCountInString(d.Title) > 128 || !utf8.ValidString(d.Description) || len(d.Description) > 16<<10 || d.FirstPublishedAt.Before(time.Unix(0, 0)) || d.FirstPublishedAt.After(time.Date(2262, 4, 11, 0, 0, 0, 0, time.UTC)) {
		return ErrInvalid
	}
	switch d.Status {
	case "ON_SALE", "RESERVED", "SOLD", "OFF_SHELF":
		return nil
	}
	return ErrInvalid
}

// Put 使用严格 external 版本写完整快照；false 表示重复或旧版本，不能物理删除不可售快照。
// 不强制 refresh；成功只表示写入已确认，不表示搜索立即可见。
func (r *Repository) Put(ctx context.Context, d Document) (bool, error) {
	if err := d.Validate(); err != nil {
		return false, err
	}
	data, err := json.Marshal(d)
	if err != nil {
		return false, ErrInvalid
	}
	path := fmt.Sprintf("/%s/_doc/%s?version=%s&version_type=external", r.index, strconv.FormatInt(d.ProductID, 10), strconv.FormatInt(d.AggregateVersion, 10))
	code, response, err := r.request(ctx, http.MethodPut, path, data)
	if err != nil {
		return false, err
	}
	if code == 409 && errorType(response) == "version_conflict_engine_exception" {
		return false, nil
	}
	if code != 200 && code != 201 {
		return false, ErrUnavailable
	}
	var result struct {
		ID      string `json:"_id"`
		Version int64  `json:"_version"`
		Result  string `json:"result"`
	}
	if json.Unmarshal(response, &result) != nil || result.ID != strconv.FormatInt(d.ProductID, 10) || result.Version != d.AggregateVersion || (result.Result != "created" && result.Result != "updated") {
		return false, ErrUnavailable
	}
	return true, nil
}

// CheckIndex 检查显式物理索引存在，避免正常启动依赖 ES 自动建索引；不替代部署权限隔离。
func (r *Repository) CheckIndex(ctx context.Context) error {
	code, _, err := r.request(ctx, http.MethodHead, "/"+r.index, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return ErrUnavailable
	}
	return nil
}

// CheckServer 确认连接的是受支持的 Elasticsearch 8，防止误连本机其他搜索产品。
// 所有进程在任何建索引、写文档或提供查询前显式调用；错误不回显端点或响应正文。
func (r *Repository) CheckServer(ctx context.Context) error {
	code, data, err := r.request(ctx, http.MethodGet, "/", nil)
	if err != nil {
		return err
	}
	var info struct {
		Version struct {
			Number       string `json:"number"`
			Distribution string `json:"distribution"`
		} `json:"version"`
		Tagline string `json:"tagline"`
	}
	if code != 200 || json.Unmarshal(data, &info) != nil || !strings.HasPrefix(info.Version.Number, "8.") || info.Version.Distribution != "" || info.Tagline != "You Know, for Search" {
		return ErrUnavailable
	}
	return nil
}
