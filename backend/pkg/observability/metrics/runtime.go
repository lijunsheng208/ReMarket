package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FromEnv 读取显式开关与独立采集地址；关闭时不创建 Registry 或监听新端口。
func FromEnv(service string) (*Metrics, error) {
	ports := map[string]string{"user": "9101", "product": "9102", "order": "9103", "search": "9104", "feed": "9105", "chat": "9106", "gateway": ""}
	port, ok := ports[service]
	if !ok {
		return nil, errors.New("指标服务名无效")
	}
	enabled := false
	if raw := os.Getenv("METRICS_ENABLED"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, errors.New("指标开关无效")
		}
		enabled = value
	}
	address := ""
	if service != "gateway" {
		address = os.Getenv(strings.ToUpper(service) + "_METRICS_ADDR")
		if address == "" {
			address = net.JoinHostPort("127.0.0.1", port)
		}
		host, p, err := net.SplitHostPort(address)
		n, e := strconv.Atoi(p)
		if err != nil || e != nil || host == "" || n < 1 || n > 65535 {
			return nil, errors.New("指标监听地址无效")
		}
	}
	if !enabled {
		return nil, nil
	}
	m, err := New(service)
	if err != nil {
		return nil, err
	}
	m.address = address
	return m, nil
}

// Start 在业务组装完成后启动独立采集 HTTP；绑定失败返回错误，不能假装可采集。
func Start(ctx context.Context, m *Metrics) (func(), error) {
	if m == nil || m.address == "" {
		return func() {}, nil
	}
	listener, err := net.Listen("tcp", m.address)
	if err != nil {
		return nil, errors.New("指标采集端口监听失败")
	}
	return m.serve(ctx, listener), nil
}

// serve 管理独立监听器生命周期，退出时关闭端口并等待后台协程结束。
func (m *Metrics) serve(ctx context.Context, listener net.Listener) func() {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("指标采集服务意外退出")
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if server.Shutdown(shutdown) != nil {
				_ = server.Close()
			}
			<-done
		})
	}
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-done:
		}
	}()
	return stop
}
