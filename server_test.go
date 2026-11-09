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

	"golang.org/x/oauth2"
)

// staticTokenSource 测试用固定 token。
type staticTokenSource struct{}

func (staticTokenSource) Token() (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: "test-token", TokenType: "QQBot"}, nil
}

// testEnv 保留旧名字，内部复用 testStack（一条 default 的 generic 通道）。
type testEnv struct {
	*testStack
	qqMock *httptest.Server
}

func newTestEnv(t *testing.T, targetType string, qqHandler http.HandlerFunc) *testEnv {
	t.Helper()
	st := newTestStack(t, targetType, qqHandler)
	return &testEnv{testStack: st, qqMock: st.mock}
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
	// 设置 token 后：通道存储与 handler 共享同一份配置，无需重建路由
	env.setToken(t, "", "sekrit")

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

// TestTokenLifecycle 校验 token 的生成展示、重置与持久化闭环。
func TestTokenLifecycle(t *testing.T) {
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"m"}`))
	})
	env.setToken(t, "", "old-token-abc")

	// config 展示生效 token 的打码形式（多通道后不再明文回显密钥）
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "old-token-abc") {
		t.Errorf("/api/config 不应明文回显 token: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), maskSecret("old-token-abc")) {
		t.Errorf("/api/config 应展示打码后的 token: %s", rec.Body.String())
	}

	// 旧 token 可用
	if code, _ := doJSON(t, env.handler, notifyReq(`{"content":"x"}`, "old-token-abc")); code != 200 {
		t.Errorf("旧 token 应 200，得到 %d", code)
	}

	// 重置
	req2 := httptest.NewRequest(http.MethodPost, "/api/token/reset", nil)
	rec2 := httptest.NewRecorder()
	env.handler.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("reset = %d: %s", rec2.Code, rec2.Body.String())
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil || !resp.OK || resp.Token == "" || resp.Token == "old-token-abc" {
		t.Fatalf("reset 响应异常: %s", rec2.Body.String())
	}

	// 旧 token 失效、新 token 生效
	if code, _ := doJSON(t, env.handler, notifyReq(`{"content":"x"}`, "old-token-abc")); code != 401 {
		t.Errorf("重置后旧 token 应 401，得到 %d", code)
	}
	if code, _ := doJSON(t, env.handler, notifyReq(`{"content":"x"}`, resp.Token)); code != 200 {
		t.Errorf("新 token 应 200，得到 %d", code)
	}

	// 持久化：新 token 落盘到 channels.json，重新加载后仍生效
	reloaded, err := LoadChannels(env.ui.dataDir, env.cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	ch, ok := reloaded.Get(reloaded.Default())
	if !ok || ch.Token != resp.Token {
		t.Errorf("持久化 token 不一致: %q（期望 %q）", ch.Token, resp.Token)
	}
}

// TestRouteFilterPortSplit 双端口路由裁剪：API 口只放 /notify，UI 口屏蔽 /notify。
func TestRouteFilterPortSplit(t *testing.T) {
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"m"}`))
	})
	env.setToken(t, "", "split-test-token") // 非空才走校验
	apiH := routeFilter(env.handler, func(r *http.Request) bool { return isAPIPath(r.URL.Path) })
	uiH := routeFilter(env.handler, func(r *http.Request) bool { return !isAPIPath(r.URL.Path) })

	// API 口：/notify 放行（无 token → 401，证明进入了处理链），/api 拒绝
	rec := httptest.NewRecorder()
	apiH.ServeHTTP(rec, notifyReq(`{"content":"x"}`, ""))
	if rec.Code != 401 {
		t.Errorf("API 口 /notify 期望 401(无token)，得到 %d", rec.Code)
	}
	// API 口：/notify/<通道> 也要放行
	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/notify/default", strings.NewReader(`{"content":"x"}`))
	apiH.ServeHTTP(rec, r)
	if rec.Code != 401 {
		t.Errorf("API 口 /notify/default 期望 401(无token)，得到 %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	apiH.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rec.Code != 404 {
		t.Errorf("API 口 /api 期望 404，得到 %d", rec.Code)
	}

	// UI 口：/api 放行，/notify 与 /notify/<通道> 拒绝
	rec = httptest.NewRecorder()
	uiH.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	if rec.Code != 200 {
		t.Errorf("UI 口 /api 期望 200，得到 %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	uiH.ServeHTTP(rec, notifyReq(`{"content":"x"}`, ""))
	if rec.Code != 404 {
		t.Errorf("UI 口 /notify 期望 404，得到 %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	uiH.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/notify/default", strings.NewReader(`{"content":"x"}`)))
	if rec.Code != 404 {
		t.Errorf("UI 口 /notify/default 期望 404，得到 %d", rec.Code)
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

// fknockBody FnKnock「标准通知 Body」样本：正文嵌在 message 段，顶层只有元数据。
// facts 里刻意含空值和空标签，用于验证过滤。
const fknockBody = `{
  "context":{"event_id":"evt_webhook_test","mode":"provider_test"},
  "message":{
    "title":"测试通知",
    "body_text":"这是一条由敲门 knock 主动发出的测试通知。",
    "body_markdown":"**连通性检查已通过。**\n\n这是一条由敲门 knock 主动发出的测试通知。",
    "summary":"通知通道配置正常。",
    "severity":"info",
    "facts":[
      {"label":"凭证名称","value":"macOS"},
      {"label":"登录 IP","value":"192.0.2.10"},
      {"label":"认证方式","value":""},
      {"label":"","value":"孤立值"}
    ]
  },
  "provider_type":"webhook",
  "source":"fn_knock"
}`

// captureSentContent 发一次推送，返回最终送到 QQ 的 content 文本。
func captureSentContent(t *testing.T, body string) string {
	t.Helper()
	var got map[string]any
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	})
	code, resp := doJSON(t, env.handler, notifyReq(body, ""))
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d: %v", code, resp)
	}
	s, _ := got["content"].(string)
	return s
}

func TestNotifyFnKnockStandardBody(t *testing.T) {
	got := captureSentContent(t, fknockBody)
	want := "测试通知\n这是一条由敲门 knock 主动发出的测试通知。\n\n凭证名称：macOS\n登录 IP：192.0.2.10"
	if got != want {
		t.Errorf("正文不符\n期望:\n%q\n得到:\n%q", want, got)
	}
}

func TestNotifyFnKnockFallsBackToMarkdown(t *testing.T) {
	body := `{"message":{"title":"登录成功","body_markdown":"**有人登录了**"},"source":"fn_knock"}`
	got := captureSentContent(t, body)
	if !strings.Contains(got, "**有人登录了**") || !strings.HasPrefix(got, "登录成功\n") {
		t.Errorf("未回退到 body_markdown: %q", got)
	}
}

func TestNotifyFnKnockFactsOnly(t *testing.T) {
	// 没有正文只有 facts 时，facts 单独成文，不应被判为空消息
	body := `{"message":{"facts":[{"label":"事件类型","value":"登录成功"}]},"source":"fn_knock"}`
	got := captureSentContent(t, body)
	if got != "事件类型：登录成功" {
		t.Errorf("仅 facts 的正文不符: %q", got)
	}
}

func TestNotifyFlatBodyStillWorks(t *testing.T) {
	// 回归：老扁平契约完全不受 normalize 影响
	got := captureSentContent(t, `{"title":"标题","content":"正文","source":"WebHook"}`)
	if got != "标题\n正文" {
		t.Errorf("扁平契约回归失败: %q", got)
	}
}

func TestRecordDetailFeature(t *testing.T) {
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	})
	if code, _ := doJSON(t, env.handler, notifyReq(`{"title":"标题","content":"详情测试"}`, "")); code != http.StatusOK {
		t.Fatalf("推送准备失败: %d", code)
	}

	// 单条记录接口返回完整正文（不是摘要）
	code, m := doJSON(t, env.handler, httptest.NewRequest(http.MethodGet, "/api/records/1", nil))
	if code != http.StatusOK {
		t.Fatalf("/api/records/1 期望 200，得到 %d", code)
	}
	rec, _ := m["record"].(map[string]any)
	if rec == nil || rec["content"] != "标题\n详情测试" {
		t.Errorf("单条记录内容不符: %v", m)
	}

	if code, _ := doJSON(t, env.handler, httptest.NewRequest(http.MethodGet, "/api/records/9999", nil)); code != http.StatusNotFound {
		t.Errorf("不存在的记录期望 404，得到 %d", code)
	}

	// /<id> 落到前端页面（SPA 路由，由 JS 解析 pathname 展开记录）
	w := httptest.NewRecorder()
	env.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/1", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<html") {
		t.Errorf("/1 应返回前端页面，得到 %d", w.Code)
	}

	// 其余路径必须仍是 404，不能被 SPA fallback 吞掉
	for _, p := range []string{"/notify", "/foo", "/1/2", "/117abc"} {
		w := httptest.NewRecorder()
		env.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s 期望 404，得到 %d", p, w.Code)
		}
	}
}

func TestRecordDetailCaptured(t *testing.T) {
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	})
	env.setToken(t, "", "tok-abc-123456")

	req := notifyReq(`{"content":"带详情的消息"}`, "tok-abc-123456")
	req.Header.Set("X-Custom-Trace", "trace-1")
	if code, _ := doJSON(t, env.handler, req); code != http.StatusOK {
		t.Fatalf("推送失败: %d", code)
	}

	_, m := doJSON(t, env.handler, httptest.NewRequest(http.MethodGet, "/api/records/1", nil))
	rec, _ := m["record"].(map[string]any)
	detail, ok := rec["detail"].(map[string]any)
	if !ok {
		t.Fatalf("记录缺少 detail 快照: %v", rec)
	}
	if detail["method"] != "POST" || detail["path"] != "/notify" {
		t.Errorf("方法与路径不符: %v", detail)
	}
	if body, _ := detail["body"].(string); !strings.Contains(body, "带详情的消息") {
		t.Errorf("请求体快照不符: %q", body)
	}

	hdrs, _ := detail["headers"].(map[string]any)
	if got, _ := hdrs["X-Gateway-Token"].(string); got == "tok-abc-123456" {
		t.Error("敏感请求头未打码")
	}
	if got, _ := hdrs["X-Custom-Trace"].(string); got != "trace-1" {
		t.Errorf("普通请求头应原样保留，得到 %q", got)
	}
}

func TestHookInbox(t *testing.T) {
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		t.Error("测试收件箱不应调用 QQ")
	})

	// 任意方法 + 任意子路径都能收
	req := httptest.NewRequest(http.MethodPost, "/hook/a/b?x=1", strings.NewReader(`{"hello":"world"}`))
	req.Header.Set("X-Gateway-Token", "supersecretvalue123456")
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/hook 期望 200，得到 %d", rec.Code)
	}

	code, m := doJSON(t, env.handler, httptest.NewRequest(http.MethodGet, "/api/test/inbox", nil))
	if code != http.StatusOK {
		t.Fatalf("列表接口期望 200，得到 %d", code)
	}
	items, _ := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("期望 1 条，得到 %d", len(items))
	}
	e, _ := items[0].(map[string]any)
	if e["method"] != "POST" || e["path"] != "/hook/a/b" || e["query"] != "x=1" {
		t.Errorf("请求元信息不符: %v", e)
	}
	if body, _ := e["body"].(string); body != `{"hello":"world"}` {
		t.Errorf("请求体不符: %q", body)
	}
	hdrs, _ := e["headers"].(map[string]any)
	if got, _ := hdrs["X-Gateway-Token"].(string); got == "supersecretvalue123456" {
		t.Error("收件箱里的敏感头未打码")
	}

	code, m = doJSON(t, env.handler, httptest.NewRequest(http.MethodDelete, "/api/test/inbox", nil))
	if code != http.StatusOK {
		t.Fatalf("清空接口期望 200，得到 %d", code)
	}
	if removed, _ := m["removed"].(float64); removed != 1 {
		t.Errorf("期望清空 1 条，得到 %v", m["removed"])
	}
	if _, m := doJSON(t, env.handler, httptest.NewRequest(http.MethodGet, "/api/test/inbox", nil)); m["total"].(float64) != 0 {
		t.Errorf("清空后应为空，得到 %v", m["total"])
	}
}
