package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed webui
var webuiFS embed.FS

var version = "dev"

// stateFile 持久化的 UI/目标状态（DATA_DIR/webui.json）。
type stateFile struct {
	WebhookURL   string `json:"webhook_url"`
	TargetType   string `json:"target_type,omitempty"`
	TargetOpenID string `json:"target_openid,omitempty"`
}

// webUI 聚合 Web 界面所需依赖与状态。
type webUI struct {
	cfg      Config
	store    *RecordStore
	hub      *Hub
	dataDir  string
	started  time.Time
	statePat string
	target   *targetState
	qq       *QQClient
	listener *openIDListener
}

func newWebUI(cfg Config, store *RecordStore, hub *Hub, dataDir string, target *targetState, qq *QQClient, listener *openIDListener) *webUI {
	return &webUI{
		cfg:      cfg,
		store:    store,
		hub:      hub,
		dataDir:  dataDir,
		started:  time.Now(),
		statePat: filepath.Join(dataDir, "webui.json"),
		target:   target,
		qq:       qq,
		listener: listener,
	}
}

// handleStats 看板统计。
func (u *webUI) handleStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stats": u.store.Stats()})
}

// handleRecords 分页列出记录（新→旧）。
func (u *webUI) handleRecords(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	total, items := u.store.List(offset, limit)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "total": total, "items": items})
}

// handleRecordImage 输出记录附带的图片。
func (u *webUI) handleRecordImage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	path := u.store.ImagePath(id)
	if path == "" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, path)
}

// handleConfig 只读全量配置（按用户指示不打码，访问控制由反代承担）。
func (u *webUI) handleConfig(w http.ResponseWriter, _ *http.Request) {
	st := u.loadState()
	typ, id := u.target.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"config": map[string]any{
			"target_type":    typ,
			"target_openid":  id,
			"api_base":       u.cfg.APIBase,
			"listen_addr":    u.cfg.ListenAddr,
			"app_id":         u.cfg.AppID,
			"app_secret":     u.cfg.Secret,
			"gateway_token":  u.cfg.GatewayToken,
			"data_dir":       u.dataDir,
			"webhook_url":    st.WebhookURL,
			"version":        version,
			"uptime_seconds": int(time.Since(u.started).Seconds()),
			"ws_clients":     u.hub.ClientCount(),
		},
	})
}

// handleLogs 读取日志文件尾部（初值；之后靠 WS 追加）。
func (u *webUI) handleLogs(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 2000 {
		n = 200
	}
	lines := tailFile(filepath.Join(u.dataDir, "logs", "gateway.log"), n)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "lines": lines})
}

// handleWebhookGet 读取已保存的公网域名。
func (u *webUI) handleWebhookGet(w http.ResponseWriter, _ *http.Request) {
	st := u.loadState()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": st.WebhookURL})
}

// handleWebhookPut 保存公网域名（仅 http/https，空值表示清除）。
func (u *webUI) handleWebhookPut(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体无效或过大"})
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if len(req.URL) > 512 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "URL 过长（>512）"})
		return
	}
	if req.URL != "" {
		parsed, err := url.Parse(req.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "仅允许 http/https 完整 URL"})
			return
		}
	}
	st := u.loadState()
	st.WebhookURL = req.URL
	if err := u.saveState(st); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	u.hub.Broadcast("webhook", map[string]any{"url": st.WebhookURL})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": st.WebhookURL})
}

// loadState 读取持久化状态文件。
func (u *webUI) loadState() stateFile {
	return loadStateFrom(u.dataDir)
}

// saveState 写入持久化状态文件。
func (u *webUI) saveState(st stateFile) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(u.statePat, data, 0o644)
}

// loadStateFrom 包级读取（main 启动时应用目标覆盖）。
func loadStateFrom(dataDir string) stateFile {
	var st stateFile
	data, err := os.ReadFile(filepath.Join(dataDir, "webui.json"))
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, &st)
	return st
}

// handleIndex 提供内嵌的单文件前端。
func (u *webUI) handleIndex(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(webuiFS, "webui")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	http.FileServer(http.FS(sub)).ServeHTTP(w, r)
}

// tailFile 取文件末尾 maxLines 行（读取末尾至多 512KB）。
func tailFile(path string, maxLines int) []string {
	f, err := os.Open(path)
	if err != nil {
		return []string{}
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return []string{}
	}
	size := fi.Size()
	const window = 512 * 1024
	start := int64(0)
	if size > window {
		start = size - window
	}
	buf := make([]byte, size-start)
	nr, err := f.ReadAt(buf, start)
	if nr == 0 {
		return []string{}
	}
	buf = buf[:nr] // 短读截断，避免半缓冲混入 NUL
	lines := strings.Split(string(buf), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	// start 落在行中间时首行才是残行（前一字符非换行），需丢弃
	if start > 0 && len(lines) > 0 {
		prev := make([]byte, 1)
		if _, err := f.ReadAt(prev, start-1); err == nil && prev[0] != '\n' {
			lines = lines[1:]
		}
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines
}
