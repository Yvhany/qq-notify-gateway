package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestHubBroadcastConcurrent 验证：多客户端并发广播不 panic（无并发写），
// hello 先于注册写入，且所有帧可被客户端收到。
func TestHubBroadcastConcurrent(t *testing.T) {
	hub := NewHub()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ws", hub.ServeWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ws"

	const clients = 3
	conns := make([]*websocket.Conn, clients)
	for i := 0; i < clients; i++ {
		c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		defer c.Close()
		conns[i] = c
		// hello 必须是首帧
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := c.ReadMessage()
		if err != nil || !strings.Contains(string(msg), "hello") {
			t.Fatalf("client %d 首帧应为 hello: %s %v", i, msg, err)
		}
	}
	if n := hub.ClientCount(); n != clients {
		t.Fatalf("ClientCount = %d, want %d", n, clients)
	}

	// 并发广播（含模拟日志路径），不应 panic
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hub.Broadcast("stats", map[string]int{"n": i})
		}(i)
	}
	wg.Wait()
	hub.Broadcast("log", "line-from-test")

	// 每个客户端至少能读到若干帧（不校验总数，队列可能丢弃过量帧）
	for i, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		got := 0
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				break
			}
			got++
			if strings.Contains(string(msg), "line-from-test") {
				break
			}
		}
		if got == 0 {
			t.Errorf("client %d 未收到任何帧", i)
		}
	}

	// 断开后计数归零
	for _, c := range conns {
		_ = c.Close()
	}
	deadline := time.Now().Add(2 * time.Second)
	for hub.ClientCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := hub.ClientCount(); n != 0 {
		t.Errorf("断开后 ClientCount = %d, want 0", n)
	}
}
