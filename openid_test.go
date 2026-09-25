package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestTargetStateUpdateAndPersist(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{AppID: "a", Secret: "s", TargetType: "c2c", TargetOpenID: "OLD_ID_C2C", APIBase: "http://example.invalid"}
	target := newTargetState(cfg.TargetType, cfg.TargetOpenID)
	qq := NewQQClient(cfg, staticTokenSource{}, time.Second, target)

	if got := qq.targetPrefix(); got != "/v2/users/OLD_ID_C2C" {
		t.Errorf("初始前缀错误: %s", got)
	}
	if err := target.Update("GROUP", "NEW_GROUP_OPENID"); err != nil {
		t.Fatal(err)
	}
	if got := qq.targetPrefix(); got != "/v2/groups/NEW_GROUP_OPENID" {
		t.Errorf("切换后前缀错误: %s", got)
	}
	if err := target.Update("频道", "X"); err == nil {
		t.Error("非法类型应报错")
	}
	if err := target.Update("c2c", "bad id with spaces"); err == nil {
		t.Error("非法 openid 应报错")
	}

	// 经 API 切换并持久化
	store, _ := NewRecordStore(t.TempDir())
	hub := NewHub()
	listener := newOpenIDListener(hub)
	ui := newWebUI(cfg, store, hub, dir, target, qq, listener)
	handler := newMux(cfg, qq, ui)

	body := `{"target_type":"group","target_openid":"PERSIST_GROUP_1"}`
	req := httptest.NewRequest(http.MethodPut, "/api/target", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("PUT /api/target = %d: %s", rec.Code, rec.Body.String())
	}

	st := loadStateFrom(dir)
	if st.TargetType != "group" || st.TargetOpenID != "PERSIST_GROUP_1" {
		t.Errorf("持久化目标错误: %+v", st)
	}
	// 模拟重启：新 target 从持久化状态恢复
	target2 := newTargetState("c2c", "OLD")
	if st2 := loadStateFrom(dir); st2.TargetOpenID != "" {
		if err := target2.Update(st2.TargetType, st2.TargetOpenID); err != nil {
			t.Fatal(err)
		}
	}
	if typ, id := target2.Snapshot(); typ != "group" || id != "PERSIST_GROUP_1" {
		t.Errorf("重启恢复错误: %s/%s", typ, id)
	}
}

// TestOpenIDListenerWindow 用 mock WS 服务器跑完整采集窗口：
// hello → Identify → 派发 C2C/群@ 事件 → 状态与广播可见 → Stop 后停止。
func TestOpenIDListenerWindow(t *testing.T) {
	var identified atomic.Bool
	wsUpgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// HELLO
		_ = conn.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 1000}})
		// 读 Identify
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var id struct {
			OP int `json:"op"`
			D  struct {
				Intents int `json:"intents"`
			} `json:"d"`
		}
		_ = json.Unmarshal(msg, &id)
		if id.OP == 2 && id.D.Intents == groupMessagesIntent {
			identified.Store(true)
		}
		// 派发两个事件
		_ = conn.WriteJSON(map[string]any{
			"op": 0, "s": 1, "t": "C2C_MESSAGE_CREATE",
			"d": map[string]any{"author": map[string]any{"user_openid": "CAPTURED_C2C_1"}, "content": "hi"},
		})
		_ = conn.WriteJSON(map[string]any{
			"op": 0, "s": 2, "t": "GROUP_AT_MESSAGE_CREATE",
			"d": map[string]any{"group_openid": "CAPTURED_GROUP_1", "content": "@bot"},
		})
		_ = conn.WriteJSON(map[string]any{
			"op": 0, "s": 3, "t": "GROUP_ADD_ROBOT",
			"d": map[string]any{"group_openid": "CAPTURED_GROUP_ADD"},
		})
		// 保持连接直到客户端断开
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer wsServer.Close()

	httpMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/gateway/bot") {
			wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
			_ = json.NewEncoder(w).Encode(map[string]any{"url": wsURL, "shards": 1})
			return
		}
		w.WriteHeader(404)
	}))
	defer httpMock.Close()

	cfg := Config{AppID: "a", Secret: "s", TargetType: "c2c", TargetOpenID: "OLD", APIBase: httpMock.URL}
	qq := NewQQClient(cfg, staticTokenSource{}, 5*time.Second, newTargetState(cfg.TargetType, cfg.TargetOpenID))
	hub := NewHub()
	listener := newOpenIDListener(hub)

	if err := listener.Start(qq); err != nil {
		t.Fatal(err)
	}
	if err := listener.Start(qq); err == nil {
		t.Error("重复 Start 应报冲突")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := listener.Snapshot()
		if s["c2c"] == "CAPTURED_C2C_1" && s["group"] == "CAPTURED_GROUP_ADD" && identified.Load() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	s := listener.Snapshot()
	if s["c2c"] != "CAPTURED_C2C_1" {
		t.Errorf("c2c 采集失败: %v", s["c2c"])
	}
	if s["group"] != "CAPTURED_GROUP_ADD" {
		t.Errorf("group 采集失败: %v", s["group"])
	}
	if !identified.Load() {
		t.Error("未收到正确 intents 的 Identify")
	}
	if !s["running"].(bool) {
		t.Error("窗口应仍在运行")
	}

	listener.Stop()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !listener.Snapshot()["running"].(bool) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if listener.Snapshot()["running"].(bool) {
		t.Error("Stop 后窗口应停止")
	}
}
