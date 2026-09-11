package app

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// 连接必须跨请求复用。
//
// 以前每个请求新建 Transport，实测 20 个请求开 20 条 TCP 连接：每次重做 TLS 握手，
// 且手工 Transport 的 IdleConnTimeout 零值 = 空闲连接永不回收，跑久了泄漏 socket。
func TestHTTPClientReusesConnections(t *testing.T) {
	var mu sync.Mutex
	conns := map[net.Conn]bool{}
	// 必须先 NewUnstartedServer 配好 ConnState 再 Start：
	// 服务已经在跑之后再改 Config 是数据竞态
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			conns[c] = true
			mu.Unlock()
		}
	}
	srv.Start()
	defer srv.Close()

	client := NewUpstreamClient(Config{
		UpstreamURL: srv.URL, RequestTimeoutSeconds: 30, ConnectTimeoutSeconds: 10,
		Models: []string{"m"}, AuthMode: AuthModeAPIKey,
	}, nil)
	acct := Account{ID: 1, APIKey: "ck_test"}
	const N = 20
	for i := 0; i < N; i++ {
		client.StreamChat(context.Background(), acct,
			map[string]any{"model": "m", "messages": []any{}},
			func([]byte, *StreamState) error { return nil })
	}
	mu.Lock()
	got := len(conns)
	mu.Unlock()
	t.Logf("发了 %d 个请求，服务端看到 %d 条新建 TCP 连接", N, got)
	if got > 2 {
		t.Errorf("连接没有复用：%d 个请求开了 %d 条连接，说明 Transport 又变成每请求新建了", N, got)
	}
}
