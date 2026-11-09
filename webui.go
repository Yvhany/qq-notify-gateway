package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed webui
var webuiFS embed.FS

var version = "dev"

// stateFile 持久化的 UI 级状态（DATA_DIR/webui.json）。
// target_type / target_openid / gateway_token 为单通道时代遗留字段：
// 仅在首次迁移生成 channels.json 时读取，之后以 channels.json 为准。
type stateFile struct {
	WebhookURL    string `json:"webhook_url"`
	TargetType    string `json:"target_type,omitempty"`
	TargetOpenID  string `json:"target_openid,omitempty"`
	GatewayToken  string `json:"gateway_token,omitempty"`
	RetentionDays int    `json:"retention_days,omitempty"`
}

// webUI 聚合 Web 界面所需依赖与状态。
type webUI struct {
	cfg       Config
	store     *RecordStore
	hub       *Hub
	dataDir   string
	started   time.Time
	statePat  string
	stateMu   sync.Mutex // webui.json 的 load→save 互斥
	channels  *channelStore
	clients   *channelHub
	listener  *openIDListener
	retention *retentionState
	hook      *hookStore
}

func newWebUI(cfg Config, store *RecordStore, hub *Hub, dataDir string, channels *channelStore, clients *channelHub, listener *openIDListener, retention *retentionState) *webUI {
	return &webUI{
		cfg:       cfg,
		store:     store,
		hub:       hub,
		dataDir:   dataDir,
		started:   time.Now(),
		statePat:  filepath.Join(dataDir, "webui.json"),
		channels:  channels,
		clients:   clients,
		listener:  listener,
		retention: retention,
		hook:      LoadHookStore(dataDir, hub),
	}
}

// handleStats 看板统计（?channel= 过滤单通道）。
func (u *webUI) handleStats(w http.ResponseWriter, r *http.Request) {
	ch := strings.TrimSpace(r.URL.Query().Get("channel"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stats": u.store.StatsByChannel(ch)})
}

// handleRecords 分页列出记录（新→旧，?channel= 过滤单通道）。
func (u *webUI) handleRecords(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	ch := strings.TrimSpace(r.URL.Query().Get("channel"))
	total, items := u.store.ListByChannel(ch, offset, limit)
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

// handleRecordGet 输出单条记录（记录详情页用，带完整正文，非摘要）。
func (u *webUI) handleRecordGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "无效的记录 ID"})
		return
	}
	rec, ok := u.store.GetByID(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "记录不存在"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "record": rec})
}

// handleHookState 返回地址列表与选中地址的收件（slug 为空取当前地址）。
func (u *webUI) handleHookState(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	cur, sessions, entries, _ := u.hook.State(time.Now(), slug)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"current":  cur,
		"sessions": sessions,
		"entries":  entries,
	})
}

// handleHookNew 生成新测试地址；当前地址仍有效时必须先作废。
func (u *webUI) handleHookNew(w http.ResponseWriter, r *http.Request) {
	sess, err := u.hook.Create(time.Now())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	u.hub.Broadcast("hook_session", map[string]any{"action": "create", "slug": sess.Slug})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "slug": sess.Slug})
}

// handleHookRevoke 作废当前地址（立即停止接收，数据仍可查看）。
func (u *webUI) handleHookRevoke(w http.ResponseWriter, r *http.Request) {
	sess, err := u.hook.Revoke(time.Now())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	u.hub.Broadcast("hook_session", map[string]any{"action": "revoke", "slug": sess.Slug})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleHookEntryImage 输出收件里的图片（Content-Type 为 image/* 的报文，
// 或 JSON 内 base64 图片解出来的那张）。
func (u *webUI) handleHookEntryImage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, ct, ok := u.hook.imageBytes(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// handleHookEntryBody 下载原始报文（不限体积）。
func (u *webUI) handleHookEntryBody(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, ok := u.hook.bodyBytes(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="hook-%d.bin"`, id))
	_, _ = w.Write(data)
}

// handleRetentionPost 应用保留策略：days>0 时立即清理更早的数据。
func (u *webUI) handleRetentionPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Days int `json:"days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体无效"})
		return
	}
	if req.Days < 0 || req.Days > 3650 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "保留天数需在 0-3650 之间（0=永久）"})
		return
	}
	// 先落盘，成功才生效
	u.stateMu.Lock()
	st := u.loadState()
	st.RetentionDays = req.Days
	saveErr := u.saveState(st)
	u.stateMu.Unlock()
	if saveErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": saveErr.Error()})
		return
	}
	u.retention.Set(req.Days)
	removed := 0
	if cutoff, ok := retentionCutoff(time.Now(), req.Days); ok {
		n, err := u.store.PurgeBefore(cutoff)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "策略已保存，但清理失败: " + err.Error()})
			return
		}
		removed = n
	}
	u.hub.Broadcast("retention", map[string]any{"days": req.Days})
	log.Printf("保留策略已应用: %d 天（立即清理 %d 条）", req.Days, removed)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "days": req.Days, "removed": removed})
}

// handleRecordsPurge 手动清理：older_than_days=0 表示清空全部。
func (u *webUI) handleRecordsPurge(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		OlderThanDays int `json:"older_than_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体无效"})
		return
	}
	if req.OlderThanDays < 0 || req.OlderThanDays > 3650 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "天数需在 0-3650 之间"})
		return
	}
	cutoff := time.Now()
	if req.OlderThanDays > 0 {
		cutoff = time.Now().AddDate(0, 0, -req.OlderThanDays)
	}
	removed, err := u.store.PurgeBefore(cutoff)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	log.Printf("手动清理完成: 删除 %d 条（阈值 %d 天）", removed, req.OlderThanDays)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
}

// handleConfig 只读全量配置。密钥类字段一律打码（多通道后同进程内持有多个机器人
// AppSecret，明文展示风险过高），需要真实值请走 POST /api/channels/{id}/secret。
func (u *webUI) handleConfig(w http.ResponseWriter, _ *http.Request) {
	st := u.loadState()
	defID := u.channels.Default()
	def, _ := u.channels.Get(defID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"config": map[string]any{
			"default_channel":  defID,
			"channel_count":    len(u.channels.List()),
			"target_type":      def.QQ.TargetType,
			"target_openid":    def.QQ.TargetOpenID,
			"api_base":         def.qqConfig().APIBase,
			"listen_addr":      u.cfg.ListenAddr,
			"ui_listen_addr":   u.cfg.UIListenAddr,
			"app_id":           def.QQ.AppID,
			"app_secret":       maskSecret(def.QQ.AppSecret),
			"data_dir":         u.dataDir,
			"webhook_url":      st.WebhookURL,
			"gateway_token":    maskSecret(def.Token),
			"retention_days":   u.retention.Snapshot(),
			"version":          version,
			"uptime_seconds":   int(time.Since(u.started).Seconds()),
			"ws_clients":       u.hub.ClientCount(),
			"channels_file":    u.channels.path,
			"client_pool_size": u.clients.Size(),
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
	u.stateMu.Lock()
	st := u.loadState()
	st.WebhookURL = req.URL
	saveErr := u.saveState(st)
	u.stateMu.Unlock()
	if saveErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": saveErr.Error()})
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
	return saveStateTo(u.dataDir, st)
}

// saveStateTo 包级写入（main 启动时生成 token 用）。
func saveStateTo(dataDir string, st stateFile) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, "webui.json"), data, 0o644)
}

// handleTokenReset 重置默认通道的入站 token：先持久化，成功后才生效。
func (u *webUI) handleTokenReset(w http.ResponseWriter, _ *http.Request) {
	candidate, err := randomToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	id := u.channels.Default()
	if _, err := u.channels.SetToken(id, candidate); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	log.Printf("通道 %s 的入站 token 已重置", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channel": id, "token": candidate})
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

// recordDetailRoute 匹配记录详情的前端路由 /<id>（如 /117）。
// 只放行纯数字路径，其余非根路径维持 404，避免误吞入站/API 路由。
var recordDetailRoute = regexp.MustCompile(`^/[0-9]{1,20}$`)

// handleIndex 提供内嵌的单文件前端。
// /<id> 是记录详情的前端路由：同一个单文件前端接管，由 JS 解析 pathname 展开对应记录。
func (u *webUI) handleIndex(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(webuiFS, "webui")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p := r.URL.Path
	isDetail := recordDetailRoute.MatchString(p)
	if p != "/" && p != "/index.html" && !isDetail {
		http.NotFound(w, r)
		return
	}
	if isDetail {
		// 交回首页内容，路径本身由前端读取 location.pathname 还原
		r = r.Clone(r.Context())
		r.URL.Path = "/"
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
