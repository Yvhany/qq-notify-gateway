package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func base64PNG(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestMakeSnippet(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"短", "短"},
		{"正好十个汉字啊啊啊啊", "正好十个汉字啊啊啊啊"}, // 恰 10 runes，不截断
		{"1234567890", "1234567890"},
		{"12345678901", "1234567890..."},
		{"  带空白的内容测试  ", "带空白的内容测试"},
	}
	for _, c := range cases {
		if got := makeSnippet(c.in); got != c.want {
			t.Errorf("makeSnippet(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// 10 个中文字 + 1 字 → 截断
	long := strings.Repeat("字", 11)
	if got := makeSnippet(long); got != strings.Repeat("字", 10)+"..." {
		t.Errorf("11 字截断错误: %q", got)
	}
}

func TestRecordStorePersistence(t *testing.T) {
	dir := t.TempDir()
	store, err := NewRecordStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	r1 := store.Add("WebHook", "ok", "", "第一条消息", nil, "")
	if r1.ID != 1 || r1.Snippet != "第一条消息" || r1.HasImage {
		t.Errorf("r1 错误: %+v", r1)
	}
	r2 := store.Add("WebHook", "error", "QQ API 错误", "失败的消息内容啊啊啊啊啊", []byte{0xFF, 0xD8, 0xFF}, ".jpg")
	if r2.ID != 2 || !r2.HasImage {
		t.Errorf("r2 错误: %+v", r2)
	}
	if store.ImagePath(2) == "" {
		t.Error("带图记录应有图片文件")
	}

	total, items := store.List(0, 50)
	if total != 2 || len(items) != 2 {
		t.Fatalf("List total=%d len=%d", total, len(items))
	}
	if items[0].ID != 2 || items[1].ID != 1 {
		t.Errorf("应新→旧排序，得到 %d,%d", items[0].ID, items[1].ID)
	}

	st := store.Stats()
	if st.TotalCount != 2 || st.TodayCount != 2 || st.TodaySuccess != 1 || st.LastStatus != "error" {
		t.Errorf("Stats 错误: %+v", st)
	}
	if st.TodayRate != 50 {
		t.Errorf("成功率错误: %v", st.TodayRate)
	}
	if len(st.DailyCounts) != 7 || st.DailyCounts[6] != 2 {
		t.Errorf("近7日计数错误: %v", st.DailyCounts)
	}

	// 重新载入：持久化往返
	store2, err := NewRecordStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	total2, items2 := store2.List(0, 50)
	if total2 != 2 || items2[0].ID != 2 {
		t.Errorf("重载后 total=%d", total2)
	}
	r3 := store2.Add("WebHook", "ok", "", "第三条", nil, "")
	if r3.ID != 3 {
		t.Errorf("重载后 nextID 错误: %d", r3.ID)
	}
}

func TestNotifyWritesRecordAndImage(t *testing.T) {
	var mock *httptest.Server
	mock = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages") && r.Method == http.MethodPost:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"id":"m"}`))
		case strings.HasSuffix(r.URL.Path, "/upload_prepare"):
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"upload_id":"u1","block_size":"64","parts":[{"index":0,"presigned_url":"` + mock.URL + `/put0","block_size":"64"}],"upload_config":{"concurrency":1}}`))
		case r.URL.Path == "/put0":
			w.WriteHeader(200)
		case strings.HasSuffix(r.URL.Path, "/upload_part_finish"):
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/files"):
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"file_info":"FI"}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"code":404,"message":"nf"}`))
		}
	}))
	defer mock.Close()

	cfg := Config{AppID: "a", Secret: "s", TargetType: "c2c", TargetOpenID: "OPENID_TEST", APIBase: mock.URL}
	qq := NewQQClient(cfg, staticTokenSource{}, 5*time.Second)
	store, _ := NewRecordStore(t.TempDir())
	hub := NewHub()
	ui := newWebUI(cfg, store, hub, t.TempDir())
	handler := newMux(cfg, qq, ui)

	img := pngMagic(64)
	body := `{"title":"标题","content":"内容正文","source":"三月七小助手","image":"` + base64PNG(img) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("期望 200，得到 %d: %s", rec.Code, rec.Body.String())
	}

	total, items := store.List(0, 10)
	if total != 1 {
		t.Fatalf("应有 1 条记录，得到 %d", total)
	}
	got := items[0]
	if got.Status != "ok" || got.Source != "三月七小助手" || !got.HasImage {
		t.Errorf("记录错误: %+v", got)
	}
	if got.Content != "标题\n内容正文" {
		t.Errorf("content 错误: %q", got.Content)
	}
	if store.ImagePath(got.ID) == "" {
		t.Error("图片文件应已落盘")
	}

	// API 层能取回
	req2 := httptest.NewRequest(http.MethodGet, "/api/records?offset=0&limit=10", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != 200 || !strings.Contains(rec2.Body.String(), `"total":1`) {
		t.Errorf("/api/records 异常: %d %s", rec2.Code, rec2.Body.String())
	}
}
