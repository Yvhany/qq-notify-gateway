package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsClient 单连接的发送队列。写操作只发生在其专属 writer goroutine，
// 彻底规避 gorilla “concurrent write to websocket connection” panic。
type wsClient struct {
	conn *websocket.Conn
	send chan []byte
}

// Hub 管理 /api/ws 连接并做单向广播（服务端 → 页面）。
type Hub struct {
	mu      sync.Mutex
	clients map[*wsClient]struct{}
}

const wsQueueSize = 128

func NewHub() *Hub {
	return &Hub{clients: make(map[*wsClient]struct{})}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	// 部署在反代登录与防火墙之后，不做 Origin 校验（与 /notify 同一信任模型）
	CheckOrigin: func(*http.Request) bool { return true },
}

// wsEvent 统一推送帧。
type wsEvent struct {
	Type string `json:"type"` // hello | record | log | stats | webhook
	Data any    `json:"data"`
}

// Broadcast 将帧入队到每个连接；队列满视为慢客户端，立即剔除。
// 锁内只做入队，不做网络写。
func (h *Hub) Broadcast(typ string, data any) {
	frame, err := json.Marshal(wsEvent{Type: typ, Data: data})
	if err != nil {
		return
	}
	var evicted []*wsClient
	h.mu.Lock()
	for c := range h.clients {
		select {
		case c.send <- frame:
		default:
			// 队列满：踢掉慢客户端，稍后在锁外关连接
			delete(h.clients, c)
			evicted = append(evicted, c)
		}
	}
	h.mu.Unlock()
	for _, c := range evicted {
		close(c.send) // 唤醒并结束其 writer；map 中已摘除，remove 不会二次 close
		_ = c.conn.Close()
	}
}

// remove 幂等地摘除连接（返回是否由本次调用摘除，避免重复 close(send)）。
func (h *Hub) remove(c *wsClient) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; !ok {
		return false
	}
	delete(h.clients, c)
	close(c.send)
	return true
}

// ServeWS 升级连接：先写 hello（此时连接尚未注册，无并发写方），
// 再注册并启动专属 writer goroutine。
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	// hello：连接私有阶段直接写，无竞态
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteJSON(wsEvent{Type: "hello", Data: map[string]any{"ok": true}}); err != nil {
		_ = conn.Close()
		return
	}

	c := &wsClient{conn: conn, send: make(chan []byte, wsQueueSize)}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	// 专属写协程：唯一的 WriteMessage 调用方
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for frame := range c.send {
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
				h.remove(c)
				_ = conn.Close()
				return
			}
		}
		// send 被 close：连接已被摘除，做最后一次关闭
		_ = conn.Close()
	}()

	// 读循环：感知客户端关闭（页面隐藏/卸载时前端主动 close）
	conn.SetReadLimit(1024)
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			h.remove(c)
			_ = conn.Close()
			<-writerDone
			return
		}
	}
}

// ClientCount 当前连接数（诊断用）。
func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// broadcastLog 供 log tee 调用：没人看时直接跳过。
func (h *Hub) broadcastLog(line string) {
	if h.ClientCount() == 0 {
		return
	}
	h.Broadcast("log", line)
}

// setupLog 将标准库 log 同时写入文件与 stderr（spec 契约：docker logs 可见），
// 并经 hub 广播到页面。botgo SDK 内部 debug 走自身 logger，不进 UI 日志。
func setupLog(dir string, h *Hub) error {
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(logDir, "gateway.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > 32<<20 {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	log.SetOutput(&teeLogWriter{
		mw:  io.MultiWriter(f, os.Stderr),
		hub: h,
	})
	return nil
}

type teeLogWriter struct {
	mw  io.Writer
	hub *Hub
}

func (t *teeLogWriter) Write(p []byte) (int, error) {
	line := string(p)
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	t.hub.broadcastLog(line)
	return t.mw.Write(p)
}
