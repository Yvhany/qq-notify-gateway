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

// webhookCfg 持久化的公网域名（仅 UI 展示与复制用）。
type webhookCfg struct {
	WebhookURL string `json:"webhook_url"`
}

// webUI 聚合 Web 界面所需依赖与状态。
type webUI struct {
	cfg         Config
	store       *RecordStore
	hub         *Hub
	dataDir     string
	startedAt   time.Time
	webhookFile string
}

func newWebUI(cfg Config, store *RecordStore, hub *Hub, dataDir string) *webUI {
	return &webUI{
		cfg:         cfg,
		store:       store,
		hub:         hub,
		dataDir:     dataDir,
		startedAt:   time.Now(),
		webhookFile: filepath.Join(dataDir, "webui.json"),
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
	wh := u.loadWebhook()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"config": map[string]any{
			"target_type":    u.cfg.TargetType,
			"target_openid":  u.cfg.TargetOpenID,
			"api_base":       u.cfg.APIBase,
			"listen_addr":    u.cfg.ListenAddr,
			"app_id":         u.cfg.AppID,
			"app_secret":     u.cfg.Secret,
			"gateway_token":  u.cfg.GatewayToken,
			"data_dir":       u.dataDir,
			"webhook_url":    wh.WebhookURL,
			"version":        version,
			"uptime_seconds": int(time.Since(u.startedAt).Seconds()),
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
	wh := u.loadWebhook()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": wh.WebhookURL})
}

// handleWebhookPut 保存公网域名（仅 http/https，空值表示清除）。
func (u *webUI) handleWebhookPut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败"})
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL != "" {
		parsed, err := url.Parse(req.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "仅允许 http/https 完整 URL"})
			return
		}
	}
	wh := webhookCfg{WebhookURL: req.URL}
	data, _ := json.MarshalIndent(wh, "", "  ")
	if err := os.WriteFile(u.webhookFile, data, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	u.hub.Broadcast("webhook", wh)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": wh.WebhookURL})
}

func (u *webUI) loadWebhook() webhookCfg {
	var wh webhookCfg
	data, err := os.ReadFile(u.webhookFile)
	if err != nil {
		return wh
	}
	_ = json.Unmarshal(data, &wh)
	return wh
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

// tailFile 取文件末尾 n 行（读取末尾至多 512KB）。
func tailFile(path string, n int) []string {
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
	if _, err := f.ReadAt(buf, start); err != nil && len(buf) == 0 {
		return []string{}
	}
	lines := strings.Split(string(buf), "\n")
	// 去掉末尾空行
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // 首行可能是被截断的半行
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
