package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	openIDWindow = 60 * time.Second
	// IntentGroupMessages：覆盖 C2C_MESSAGE_CREATE / GROUP_AT_MESSAGE_CREATE 等
	groupMessagesIntent = 1 << 25 // 33554432，与 botgo IntentGroupMessages 一致
)

// openidCapture 单次采集结果（WS 广播与 API 共用结构）。
type openidCapture struct {
	Kind string `json:"kind"` // c2c | group
	ID   string `json:"id"`
	At   string `json:"at"`
}

// openIDListener 运行时 60 秒采集窗口：手写 WS 客户端，生命周期完全受控
// （botgo session manager 无停止 API，窗口外无法保证零接收，故不复用）。
type openIDListener struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	cancel    context.CancelFunc
	c2c       string
	group     string
	hub       *Hub
}

func newOpenIDListener(hub *Hub) *openIDListener {
	return &openIDListener{hub: hub}
}

// Snapshot 供 GET /api/openid。
func (l *openIDListener) Snapshot() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	remaining := 0
	if l.running {
		left := openIDWindow - time.Since(l.startedAt)
		if left > 0 {
			remaining = int(left.Seconds()) + 1
		}
	}
	return map[string]any{
		"running":           l.running,
		"remaining_seconds": remaining,
		"c2c":               l.c2c,
		"group":             l.group,
	}
}

// record 更新状态并广播采集事件。
func (l *openIDListener) record(kind, id string) {
	if id == "" {
		return
	}
	ev := openidCapture{Kind: kind, ID: id, At: time.Now().Format(time.RFC3339)}
	l.mu.Lock()
	if kind == "c2c" {
		l.c2c = id
	} else {
		l.group = id
	}
	l.mu.Unlock()
	log.Printf("OpenID 采集: type=%s id=%s", kind, id)
	l.hub.Broadcast("openid", ev)
}

// Start 启动一个采集窗口；已在运行时返回错误。
func (l *openIDListener) Start(qq *QQClient) error {
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return fmt.Errorf("采集窗口已在运行")
	}
	ctx, cancel := context.WithTimeout(context.Background(), openIDWindow)
	l.cancel = cancel
	l.running = true
	l.startedAt = time.Now()
	l.mu.Unlock()

	l.hub.Broadcast("openid_state", l.Snapshot())
	go func() {
		defer func() {
			l.mu.Lock()
			l.running = false
			l.cancel = nil
			l.mu.Unlock()
			cancel()
			l.hub.Broadcast("openid_state", l.Snapshot())
			log.Printf("OpenID 采集窗口结束")
		}()
		if err := l.run(ctx, qq); err != nil && ctx.Err() == nil {
			log.Printf("OpenID 采集异常: %v", err)
		}
	}()
	return nil
}

// Stop 手动停止窗口（幂等）。
func (l *openIDListener) Stop() {
	l.mu.Lock()
	cancel := l.cancel
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// run 建立 WS 连接并消费事件直到窗口结束。
func (l *openIDListener) run(ctx context.Context, qq *QQClient) error {
	wsURL, err := qq.gatewayURL(ctx)
	if err != nil {
		return err
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("连接 WS 失败: %w", err)
	}
	defer conn.Close()

	// ctx 结束时关闭连接，解除读循环阻塞
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	var (
		seq       atomic.Int64 // 主循环写、心跳协程读，原子化避免数据竞争
		hbStarted bool
	)
	// 心跳：hello 之后由主循环启动，确保 Identify 先发（单写者顺序：Identify → 心跳）
	stopHB := make(chan struct{})
	defer close(stopHB)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return nil // 窗口关闭或对端断开均视为正常结束
		}
		var frame struct {
			OP int             `json:"op"`
			T  string          `json:"t"`
			D  json.RawMessage `json:"d"`
			S  int             `json:"s"`
		}
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		switch frame.OP {
		case 10: // HELLO → Identify（此写发生在心跳协程启动前，无并发写）
			token, err := qq.rawToken()
			if err != nil {
				return err
			}
			identify := map[string]any{
				"op": 2,
				"d": map[string]any{
					"token":      "QQBot " + token,
					"intents":    groupMessagesIntent,
					"shard":      []int{0, 1},
					"properties": map[string]string{},
				},
			}
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteJSON(identify); err != nil {
				return fmt.Errorf("Identify 失败: %w", err)
			}
			if !hbStarted {
				hbStarted = true
				interval := 41 * time.Second
				var hello struct {
					HeartbeatInterval int64 `json:"heartbeat_interval"`
				}
				_ = json.Unmarshal(frame.D, &hello)
				if hello.HeartbeatInterval > 0 {
					interval = time.Duration(hello.HeartbeatInterval) * time.Millisecond
				}
				go heartbeatWriter(conn, ctx, interval, &seq, stopHB)
			}
		case 0: // DISPATCH
			seq.Store(int64(frame.S))
			l.dispatch(frame.T, data)
		case 11: // HEARTBEAT ACK — 忽略
		}
	}
}

// dispatch 按事件类型提取 openid。
func (l *openIDListener) dispatch(eventType string, raw []byte) {
	switch eventType {
	case "C2C_MESSAGE_CREATE":
		l.record("c2c", extractOpenID("c2c", raw))
	case "GROUP_AT_MESSAGE_CREATE":
		l.record("group", extractOpenID("group", raw))
	case "GROUP_ADD_ROBOT":
		id := extractOpenID("group", raw)
		if id == "" {
			id = extractGroupNested(raw)
		}
		l.record("group", id)
	default:
		// 其余同 intent 事件（FRIEND_ADD 等）忽略；Plain 语义由本 switch 承担
	}
}

// extractGroupNested 兜底：group_openid 嵌在 group 对象里的载荷形态。
func extractGroupNested(raw []byte) string {
	var probe struct {
		D struct {
			Group struct {
				GroupOpenID string `json:"group_openid"`
			} `json:"group"`
		} `json:"d"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.D.Group.GroupOpenID
}

// heartbeatWriter 定时心跳；写失败或窗口结束即退出。
func heartbeatWriter(conn *websocket.Conn, ctx context.Context, interval time.Duration, seq *atomic.Int64, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteJSON(map[string]any{"op": 1, "d": seq.Load()}); err != nil {
				return
			}
		}
	}
}

// handleOpenIDGet /api/openid
func (u *webUI) handleOpenIDGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": u.listener.Snapshot()})
}

// handleOpenIDListen POST /api/openid/listen — 启动 60s 窗口
func (u *webUI) handleOpenIDListen(w http.ResponseWriter, _ *http.Request) {
	if err := u.listener.Start(u.qq); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": u.listener.Snapshot()})
}

// handleOpenIDStop POST /api/openid/stop
func (u *webUI) handleOpenIDStop(w http.ResponseWriter, _ *http.Request) {
	u.listener.Stop()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleTargetPut PUT /api/target — 切换单聊/群组并持久化
func (u *webUI) handleTargetPut(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		TargetType   string `json:"target_type"`
		TargetOpenID string `json:"target_openid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体无效"})
		return
	}
	// 先做与 target.Update 相同的校验（不动内存）
	typ := strings.ToLower(strings.TrimSpace(req.TargetType))
	id := strings.TrimSpace(req.TargetOpenID)
	if typ != "c2c" && typ != "group" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "目标类型必须是 c2c 或 group"})
		return
	}
	if !validOpenID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "OpenID 格式无效"})
		return
	}
	// 落盘成功后才更新内存，保证两者一致
	u.stateMu.Lock()
	st := u.loadState()
	st.TargetType, st.TargetOpenID = typ, id
	saveErr := u.saveState(st)
	u.stateMu.Unlock()
	if saveErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": saveErr.Error()})
		return
	}
	if err := u.target.Update(typ, id); err != nil {
		// 校验已通过，理论上不可达；防御性回滚
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	u.hub.Broadcast("target", map[string]any{"target_type": typ, "target_openid": id})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "target_type": typ, "target_openid": id})
}
