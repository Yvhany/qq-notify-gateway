package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedStore 构造历史数据：id1/id2 为 40 天前，id3 为 1 天前，另加孤立图片 9.jpg。
func seedStore(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(id uint64, at time.Time, img bool) Record {
		return Record{
			ID: id, TS: at.Format(time.RFC3339), Source: "WebHook", Status: "ok",
			Content: "内容", Snippet: "内容", HasImage: img,
		}
	}
	var lines [][]byte
	for _, r := range []Record{
		mk(1, time.Now().AddDate(0, 0, -40), true),
		mk(2, time.Now().AddDate(0, 0, -40), true),
		mk(3, time.Now().AddDate(0, 0, -1), true),
	} {
		b, _ := json.Marshal(r)
		lines = append(lines, b)
	}
	f, err := os.OpenFile(filepath.Join(dir, "records.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		_, _ = f.Write(append(l, '\n'))
	}
	_ = f.Close()
	for _, name := range []string{"1.jpg", "2.jpg", "3.jpg", "9.jpg"} {
		_ = os.WriteFile(filepath.Join(dir, "images", name), []byte{0xFF, 0xD8, 0xFF, 0x00}, 0o644)
	}
	return dir
}

func TestPurgeBefore(t *testing.T) {
	dir := seedStore(t)
	store, err := NewRecordStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if total, _ := store.List(0, 10); total != 3 {
		t.Fatalf("前置数据错误: %d", total)
	}

	removed, err := store.PurgeBefore(time.Now().AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("应删除 2 条，实际 %d", removed)
	}
	total, items := store.List(0, 10)
	if total != 1 || items[0].ID != 3 {
		t.Errorf("应仅保留 id3，total=%d", total)
	}
	// 图片：保留 3.jpg；1/2 与孤立 9 被清
	for name, want := range map[string]bool{"1.jpg": false, "2.jpg": false, "3.jpg": true, "9.jpg": false} {
		exists := true
		if _, err := os.Stat(filepath.Join(dir, "images", name)); os.IsNotExist(err) {
			exists = false
		}
		if exists != want {
			t.Errorf("图片 %s 存在性=%v，期望 %v", name, exists, want)
		}
	}
	// nextID 需继续单调（不回收已删 id）
	if rec := store.Add("WebHook", "ok", "", "新记录", nil, ""); rec.ID != 4 {
		t.Errorf("清理后 nextID 应继续为 4，实际 %d", rec.ID)
	}
	// 重写后的文件可被重新加载
	store2, err := NewRecordStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if total, _ := store2.List(0, 10); total != 2 {
		t.Errorf("重载记录数应为 2，实际 %d", total)
	}
}

func TestRetentionAndPurgeAPI(t *testing.T) {
	dir := seedStore(t)
	store, err := NewRecordStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{AppID: "a", Secret: "s", TargetType: "c2c", TargetOpenID: "OID", APIBase: "http://example.invalid"}
	target := newTargetState(cfg.TargetType, cfg.TargetOpenID)
	qq := NewQQClient(cfg, staticTokenSource{}, time.Second, target)
	hub := NewHub()
	listener := newOpenIDListener(hub)
	ret := newRetentionState(0)
	tok := newTokenState("")
	ui := newWebUI(cfg, store, hub, dir, target, qq, listener, tok, ret)
	handler := newMux(tok, qq, ui)

	post := func(path, body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}

	// 应用 30 天保留 → 清掉 2 条旧记录
	code, resp := post("/api/retention", `{"days":30}`)
	if code != 200 || resp["ok"] != true {
		t.Fatalf("设置保留策略失败: %d %v", code, resp)
	}
	if resp["removed"] != float64(2) {
		t.Errorf("应清理 2 条，实际 %v", resp["removed"])
	}
	if ret.Snapshot() != 30 {
		t.Errorf("保留状态错误: %d", ret.Snapshot())
	}
	if st := loadStateFrom(dir); st.RetentionDays != 30 {
		t.Errorf("保留策略未持久化: %+v", st)
	}
	// /api/config 暴露
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rw := httptest.NewRecorder()
	handler.ServeHTTP(rw, req)
	if !strings.Contains(rw.Body.String(), `"retention_days":30`) {
		t.Errorf("config 未包含 retention_days: %s", rw.Body.String())
	}

	// 非法值拒绝
	if code, _ := post("/api/retention", `{"days":-1}`); code != 400 {
		t.Errorf("负天数应 400，得到 %d", code)
	}

	// 一键清空
	code, resp = post("/api/records/purge", `{"older_than_days":0}`)
	if code != 200 || resp["removed"] != float64(1) {
		t.Fatalf("清空失败: %d %v", code, resp)
	}
	if total, _ := store.List(0, 10); total != 0 {
		t.Errorf("清空后应为 0 条，实际 %d", total)
	}
	// 图片应全部清空
	files, _ := filepath.Glob(filepath.Join(dir, "images", "*"))
	if len(files) != 0 {
		t.Errorf("清空后图片残留: %v", files)
	}

	// 设为永久后再次清空应无副作用
	if code, resp := post("/api/retention", `{"days":0}`); code != 200 || resp["removed"] != float64(0) {
		t.Errorf("永久策略响应异常: %d %v", code, resp)
	}
	if _, ok := retentionCutoff(time.Now(), 0); ok {
		t.Error("永久策略不应产生清理截止时间")
	}
}
