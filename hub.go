package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Hub 管理 /api/ws 连接并做单向广播（服务端 → 页面）。
type Hub struct {
	mu      sync.Mutex
	clients map[*websocket.Conn]struct{}
}

func NewHub() *Hub {
	return &Hub{clients: make(map[*websocket.Conn]struct{})}
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

// Broadcast 向所有连接推送事件；写失败的连接即时剔除。
func (h *Hub) Broadcast(typ string, data any) {
	frame, err := json.Marshal(wsEvent{Type: typ, Data: data})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := c.WriteMessage(websocket.TextMessage, frame); err != nil {
			_ = c.Close()
			delete(h.clients, c)
		}
	}
}

// ServeWS 升级并注册连接；读循环用于感知客户端关闭（页面隐藏/卸载时前端主动 close）。
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.clients[conn] = struct{}{}
	h.mu.Unlock()

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = conn.WriteJSON(wsEvent{Type: "hello", Data: map[string]any{"ok": true}})

	defer func() {
		h.mu.Lock()
		delete(h.clients, conn)
		h.mu.Unlock()
		_ = conn.Close()
	}()

	// 只读循环：客户端只发关闭/ping，不做业务消息
	conn.SetReadLimit(1024)
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
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

// setupLog 将标准库 log 同时写入文件与 stderr，并经 hub 广播到页面。
// botgo SDK 内部 debug 走自身 logger，不进 UI 日志。
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
	log.SetOutput(&teeLogWriter{file: f, hub: h})
	return nil
}

type teeLogWriter struct {
	file *os.File
	hub  *Hub
}

func (t *teeLogWriter) Write(p []byte) (int, error) {
	line := string(p)
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	t.hub.broadcastLog(line)
	return t.file.Write(p)
}
