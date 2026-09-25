package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// staticTokenSource 测试用固定 token。
type staticTokenSource struct{}

func (staticTokenSource) Token() (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: "test-token", TokenType: "QQBot"}, nil
}

// testEnv 组装：QQ mock 服务器 + 配置 + 客户端 + 网关 handler。
type testEnv struct {
	qqMock  *httptest.Server
	cfg     Config
	qq      *QQClient
	store   *RecordStore
	ui      *webUI
	handler http.Handler
}

func newTestEnv(t *testing.T, targetType string, qqHandler http.HandlerFunc) *testEnv {
	t.Helper()
	mock := httptest.NewServer(qqHandler)
	t.Cleanup(mock.Close)

	cfg := Config{
		AppID:        "test-app-id",
		Secret:       "test-secret",
		TargetType:   targetType,
		TargetOpenID: "OPENID_TEST",
		ListenAddr:   ":0",
		APIBase:      mock.URL,
	}
	qq := NewQQClient(cfg, staticTokenSource{}, 5*time.Second, newTargetState(cfg.TargetType, cfg.TargetOpenID))
	store, err := NewRecordStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	listener := newOpenIDListener(hub)
	ui := newWebUI(cfg, store, hub, t.TempDir(), qq.target, qq, listener)
	return &testEnv{qqMock: mock, cfg: cfg, qq: qq, store: store, ui: ui, handler: newMux(cfg, qq, ui)}
}

func notifyReq(body, gatewayToken string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if gatewayToken != "" {
		r.Header.Set("X-Gateway-Token", gatewayToken)
	}
	return r
}

func doJSON(t *testing.T, h http.Handler, r *http.Request) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var m map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
	}
	return rec.Code, m
}

// pngMagic 有效的 PNG 文件头（DetectContentType 依据前 8 字节识别）。
func pngMagic(n int) []byte {
	sig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if n <= len(sig) {
		return sig
	}
	return append(sig, bytes.Repeat([]byte{0}, n-len(sig))...)
}

func TestNotifyTextOnly(t *testing.T) {
	var got map[string]any
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/users/OPENID_TEST/messages" {
			t.Errorf("路径错误: %s", r.URL.Path)
		}
		if ah := r.Header.Get("Authorization"); ah != "QQBot test-token" {
			t.Errorf("Authorization 错误: %q", ah)
		}
		if x := r.Header.Get("X-Union-Appid"); x != "test-app-id" {
			t.Errorf("X-Union-Appid 错误: %q", x)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	})

	code, resp := doJSON(t, env.handler, notifyReq(`{"title":"三月七小助手","content":"每日实训已完成"}`, ""))
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d: %v", code, resp)
	}
	if resp["ok"] != true {
		t.Fatalf("期望 ok=true，得到 %v", resp)
	}
	if got["msg_type"] != float64(0) {
		t.Errorf("msg_type 错误: %v", got["msg_type"])
	}
	if got["content"] != "三月七小助手\n每日实训已完成" {
		t.Errorf("content 错误: %v", got["content"])
	}
}

func TestNotifyGroupPath(t *testing.T) {
	var path string
	env := newTestEnv(t, "group", func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	})
	code, _ := doJSON(t, env.handler, notifyReq(`{"title":"t","content":"c"}`, ""))
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", code)
	}
	if path != "/v2/groups/OPENID_TEST/messages" {
		t.Errorf("群路径错误: %s", path)
	}
}

func TestNotifyAuthRequired(t *testing.T) {
	var calls atomic.Int32
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"m"}`))
	})
	env.cfg.GatewayToken = "sekrit"
	env.handler = newMux(env.cfg, env.qq, env.ui)

	if code, _ := doJSON(t, env.handler, notifyReq(`{"content":"x"}`, "")); code != http.StatusUnauthorized {
		t.Errorf("无 token 期望 401，得到 %d", code)
	}
	if code, _ := doJSON(t, env.handler, notifyReq(`{"content":"x"}`, "wrong")); code != http.StatusUnauthorized {
		t.Errorf("错误 token 期望 401，得到 %d", code)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("鉴权失败时不应调用 QQ，实际 %d 次", n)
	}
	if code, _ := doJSON(t, env.handler, notifyReq(`{"content":"x"}`, "sekrit")); code != http.StatusOK {
		t.Errorf("正确 token 期望 200，得到 %d", code)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("期望调用 QQ 1 次，实际 %d 次", n)
	}
}

func TestNotifyBadRequests(t *testing.T) {
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		t.Error("参数错误时不应调用 QQ")
	})

	cases := []struct {
		name string
		body string
		want int
	}{
		{"空消息", `{}`, http.StatusBadRequest},
		{"坏JSON", `not-json`, http.StatusBadRequest},
		{"坏base64", `{"content":"x","image":"!!!not-base64!!!"}`, http.StatusBadRequest},
		{"非图片类型", `{"content":"x","image":"aGVsbG8="}`, http.StatusUnsupportedMediaType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, resp := doJSON(t, env.handler, notifyReq(tc.body, ""))
			if code != tc.want {
				t.Errorf("期望 %d，得到 %d: %v", tc.want, code, resp)
			}
		})
	}
}

func TestNotifyImageTooLarge(t *testing.T) {
	old := maxImageBytes
	maxImageBytes = 64
	t.Cleanup(func() { maxImageBytes = old })

	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		t.Error("超限时不应调用 QQ")
	})
	img := base64.StdEncoding.EncodeToString(pngMagic(128))
	code, _ := doJSON(t, env.handler, notifyReq(`{"content":"x","image":"`+img+`"}`, ""))
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("期望 413，得到 %d", code)
	}
}

func TestDecodeImagePayload(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString([]byte("hello"))
	data, err := decodeImagePayload(raw)
	if err != nil || string(data) != "hello" {
		t.Errorf("普通 base64 失败: %q %v", data, err)
	}
	data, err = decodeImagePayload("data:image/png;base64," + raw)
	if err != nil || string(data) != "hello" {
		t.Errorf("data URI 失败: %q %v", data, err)
	}
	if data, err := decodeImagePayload("  "); err != nil || data != nil {
		t.Errorf("空白应返回 nil: %q %v", data, err)
	}
	if _, err := decodeImagePayload("!!"); err == nil {
		t.Error("非法 base64 应报错")
	}
}
