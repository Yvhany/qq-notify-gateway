package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// hookSlugLen 地址随机段长度：36^8 ≈ 2.8e12 种组合。
	hookSlugLen = 8
	// hookTTL 地址有效期：从生成起 12 小时内可接收请求。
	hookTTL = 12 * time.Hour
	// hookRetention 数据保留：会话创建 15 天后连同收件一起删除。
	hookRetention = 15 * 24 * time.Hour
	// hookMaxSessions 最多保留几个地址，更早的自动淘汰。
	hookMaxSessions = 5
	// hookEntriesPer 每个地址最多保留多少条请求。
	hookEntriesPer = 50
	// hookSessionBytes 单个地址请求体总量上限（1GB），超出淘汰最旧的条目。
	hookSessionBytes = 1 << 30
	// hookPreviewBytes 落进 JSON 的正文预览长度。
	hookPreviewBytes = 32 << 10
	// hookJSONImageMax 从 JSON body 里抽取 base64 图片时允许的最大 body 尺寸。
	hookJSONImageMax = 32 << 20
)

var (
	errHookAlive = errors.New("当前地址仍在有效期内，请先作废再生成新地址")
	errHookNoCur = errors.New("当前没有可作废的地址")
)

// HookEntry 一条测试请求的快照。正文不在 JSON 里，只留文件引用与预览。
type HookEntry struct {
	ID        uint64            `json:"id"`
	TS        string            `json:"ts"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Query     string            `json:"query,omitempty"`
	Proto     string            `json:"proto,omitempty"`
	RemoteIP  string            `json:"remote_ip"`
	Headers   map[string]string `json:"headers"`
	BodyBytes int64             `json:"body_bytes"`
	Preview   string            `json:"preview,omitempty"`    // 正文前 32KB（文本）
	Truncated bool              `json:"truncated,omitempty"`  // 预览被截断
	IsImage   bool              `json:"is_image,omitempty"`   // Content-Type 是 image/*
	Binary    bool              `json:"binary,omitempty"`     // 非文本报文，不做预览
	ImageFile string            `json:"image_file,omitempty"` // JSON 内 base64 图片解出的文件名
	BodyFile  string            `json:"body_file,omitempty"`  // 原始报文文件名
}

// textualContentType 判断报文能否按文本预览。
func textualContentType(ct string) bool {
	if ct == "" {
		return true
	}
	for _, k := range []string{"text/", "json", "xml", "javascript", "x-www-form-urlencoded"} {
		if strings.Contains(ct, k) {
			return true
		}
	}
	return false
}

// HookSession 一次「测试地址」及其收件箱。
type HookSession struct {
	Slug      string      `json:"slug"`
	CreatedAt time.Time   `json:"created_at"`
	ExpiresAt time.Time   `json:"expires_at"`
	RevokedAt *time.Time  `json:"revoked_at,omitempty"`
	Entries   []HookEntry `json:"entries"`
}

func (s *HookSession) alive(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.ExpiresAt)
}

func (s *HookSession) totalBytes() int64 {
	var n int64
	for _, e := range s.Entries {
		n += e.BodyBytes
	}
	return n
}

// hookSummary 给前端的会话摘要（不含条目）。
type hookSummary struct {
	Slug      string `json:"slug"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	Revoked   bool   `json:"revoked"`
	Alive     bool   `json:"alive"`
	Count     int    `json:"count"`
	Bytes     int64  `json:"bytes"`
}

// hookFile 落盘结构（DATA_DIR/hook_sessions.json，0600）。
type hookFile struct {
	Version  int           `json:"version"`
	Current  string        `json:"current"`
	NextID   uint64        `json:"next_id"`
	Sessions []HookSession `json:"sessions"`
}

// hookStore 「Webhook 测试」的地址与收件箱。
// 一地址一会话：8 位随机路径、12 小时接收窗口、15 天保留、最多 5 个地址，
// 单地址正文总量上限 1GB（超出淘汰最旧条目）。正文存独立文件，不进 JSON。
type hookStore struct {
	mu    sync.Mutex
	dir   string // DATA_DIR
	path  string // DATA_DIR/hook_sessions.json
	files string // DATA_DIR/hook_files
	f     hookFile
	hub   *Hub
}

// LoadHookStore 读取并清理（过期会话、超额会话）后返回。
// 文件损坏时保留原文件为 .corrupt 并重置，不让收件箱拖垮整个网关启动。
func LoadHookStore(dataDir string, hub *Hub) *hookStore {
	s := &hookStore{
		dir:   dataDir,
		path:  filepath.Join(dataDir, "hook_sessions.json"),
		files: filepath.Join(dataDir, "hook_files"),
		hub:   hub,
		f:     hookFile{Version: 1, NextID: 1},
	}
	if data, err := os.ReadFile(s.path); err == nil {
		if err := json.Unmarshal(data, &s.f); err != nil {
			log.Printf("测试收件箱数据损坏（%v），已备份为 hook_sessions.json.corrupt 并重置", err)
			_ = os.Rename(s.path, s.path+".corrupt")
			s.f = hookFile{Version: 1, NextID: 1}
		}
	}
	if s.f.Version == 0 {
		s.f.Version = 1
	}
	if s.f.NextID == 0 {
		s.f.NextID = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.purgeLocked(time.Now()) {
		_ = s.saveLocked()
	}
	return s
}

// purgeLocked 淘汰过期会话与超额会话（调用方持锁）。返回是否有改动。
func (s *hookStore) purgeLocked(now time.Time) bool {
	before := len(s.f.Sessions)
	sorted := append([]HookSession(nil), s.f.Sessions...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CreatedAt.After(sorted[j].CreatedAt) })

	kept := make([]HookSession, 0, hookMaxSessions)
	dropped := make([]HookSession, 0)
	for _, sess := range sorted {
		if len(kept) >= hookMaxSessions || now.Sub(sess.CreatedAt) > hookRetention {
			dropped = append(dropped, sess)
			continue
		}
		kept = append(kept, sess)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].CreatedAt.Before(kept[j].CreatedAt) })

	changed := len(kept) != before
	s.f.Sessions = kept
	for _, d := range dropped {
		s.removeSessionFiles(d.Slug)
	}
	if s.f.Current != "" {
		if _, ok := s.findLocked(s.f.Current); !ok {
			s.f.Current = ""
			changed = true
		}
	}
	return changed
}

func (s *hookStore) findLocked(slug string) (*HookSession, bool) {
	for i := range s.f.Sessions {
		if s.f.Sessions[i].Slug == slug {
			return &s.f.Sessions[i], true
		}
	}
	return nil, false
}

func (s *hookStore) sessionDir(slug string) string { return filepath.Join(s.files, slug) }

// removeSessionFiles 删除某会话的文件目录（仅接受已存在于会话列表的 slug）。
func (s *hookStore) removeSessionFiles(slug string) {
	if slug == "" || strings.ContainsAny(slug, `/\`) {
		return
	}
	_ = os.RemoveAll(s.sessionDir(slug))
}

func (s *hookStore) saveLocked() error {
	data, err := json.MarshalIndent(s.f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

const hookAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func newSlug() (string, error) {
	raw := make([]byte, hookSlugLen*4)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, hookSlugLen)
	for i := range out {
		out[i] = hookAlphabet[int(raw[i])%len(hookAlphabet)]
	}
	return string(out), nil
}

// Create 生成新地址；当前地址仍有效时必须先作废。
func (s *hookStore) Create(now time.Time) (HookSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.findLocked(s.f.Current); ok && cur.alive(now) {
		return HookSession{}, errHookAlive
	}
	var slug string
	for i := 0; i < 16; i++ {
		cand, err := newSlug()
		if err != nil {
			return HookSession{}, err
		}
		if _, exists := s.findLocked(cand); !exists {
			slug = cand
			break
		}
	}
	if slug == "" {
		return HookSession{}, errors.New("生成地址失败，请重试")
	}
	sess := HookSession{
		Slug:      slug,
		CreatedAt: now,
		ExpiresAt: now.Add(hookTTL),
		Entries:   []HookEntry{},
	}
	s.f.Sessions = append(s.f.Sessions, sess)
	s.f.Current = slug
	s.purgeLocked(now)
	if err := s.saveLocked(); err != nil {
		return HookSession{}, err
	}
	return sess, nil
}

// Revoke 作废当前地址：立即停止接收，数据仍可查看至保留期结束。
func (s *hookStore) Revoke(now time.Time) (HookSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.findLocked(s.f.Current)
	if !ok {
		return HookSession{}, errHookNoCur
	}
	if cur.RevokedAt == nil {
		t := now
		cur.RevokedAt = &t
	}
	out := *cur
	if err := s.saveLocked(); err != nil {
		return HookSession{}, err
	}
	return out, nil
}

// State 返回全部会话摘要与指定 slug 的条目（新→旧）；slug 为空取当前会话。
func (s *hookStore) State(now time.Time, slug string) (string, []hookSummary, []HookEntry, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked(now)
	if slug == "" {
		slug = s.f.Current
	}
	sums := make([]hookSummary, 0, len(s.f.Sessions))
	for i := len(s.f.Sessions) - 1; i >= 0; i-- {
		sess := s.f.Sessions[i]
		sums = append(sums, hookSummary{
			Slug:      sess.Slug,
			CreatedAt: sess.CreatedAt.Format(time.RFC3339),
			ExpiresAt: sess.ExpiresAt.Format(time.RFC3339),
			Revoked:   sess.RevokedAt != nil,
			Alive:     sess.alive(now),
			Count:     len(sess.Entries),
			Bytes:     sess.totalBytes(),
		})
	}
	var entries []HookEntry
	var total int64
	if sess, ok := s.findLocked(slug); ok {
		total = sess.totalBytes()
		entries = make([]HookEntry, 0, len(sess.Entries))
		for i := len(sess.Entries) - 1; i >= 0; i-- {
			entries = append(entries, sess.Entries[i])
		}
	}
	return slug, sums, entries, total
}

// EntryByID 查找某条收件记录（含跨会话），供正文/图片下载。
func (s *hookStore) EntryByID(id uint64) (HookEntry, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.f.Sessions {
		for _, e := range sess.Entries {
			if e.ID == id {
				return e, sess.Slug, true
			}
		}
	}
	return HookEntry{}, "", false
}

// cappedWriter 只保留前 max 字节，其余丢弃并标记截断。
type cappedWriter struct {
	buf       []byte
	max       int
	truncated bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if len(c.buf) >= c.max {
		c.truncated = true
		return len(p), nil
	}
	room := c.max - len(c.buf)
	if len(p) > room {
		c.buf = append(c.buf, p[:room]...)
		c.truncated = true
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

// Append 接收一次请求：正文流式落盘、抽取图片、记元数据并落 JSON。
// 返回 false 表示地址不存在或已失效。
func (s *hookStore) Append(slug string, r *http.Request, now time.Time) (bool, error) {
	s.mu.Lock()
	sess, ok := s.findLocked(slug)
	alive := ok && sess.alive(now)
	var id uint64
	if alive {
		id = s.f.NextID
		s.f.NextID++
	}
	s.mu.Unlock()
	if !alive {
		return false, nil
	}

	dir := s.sessionDir(slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return true, err
	}
	// umask 可能放宽目录/文件权限，显式收紧（里面有他人的请求报文）
	_ = os.Chmod(dir, 0o700)
	bodyName := strconv.FormatUint(id, 10) + ".bin"
	bodyPath := filepath.Join(dir, bodyName)
	f, err := os.OpenFile(bodyPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return true, err
	}
	_ = os.Chmod(bodyPath, 0o600)
	cap := &cappedWriter{max: hookPreviewBytes}
	n, copyErr := io.Copy(io.MultiWriter(f, cap), r.Body)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(bodyPath)
		return true, copyErr
	}
	if closeErr != nil {
		return true, closeErr
	}

	e := HookEntry{
		ID:        id,
		TS:        now.Format(time.RFC3339),
		Method:    r.Method,
		Path:      r.URL.Path,
		Query:     r.URL.RawQuery,
		Proto:     r.Proto,
		RemoteIP:  clientIP(r),
		Headers:   make(map[string]string, len(r.Header)),
		BodyBytes: n,
		BodyFile:  bodyName,
		Preview:   string(cap.buf),
		Truncated: cap.truncated,
	}
	for k, vs := range r.Header {
		v := strings.Join(vs, ", ")
		if sensitiveHeader(k) {
			v = maskSecret(v)
		}
		e.Headers[k] = v
	}

	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	e.IsImage = strings.HasPrefix(ct, "image/")
	// 二进制报文不做文本预览，前端只给下载（图片另有 image 路由）
	if !e.IsImage && !textualContentType(ct) {
		e.Binary = true
		e.Preview = ""
	}

	// JSON body 里的 base64 图片单独解开存盘，便于前端直接显示
	if !e.IsImage && strings.Contains(ct, "json") && n > 0 && n <= hookJSONImageMax {
		if imgName, ok := extractJSONImage(bodyPath, dir, id); ok {
			e.ImageFile = imgName
		}
	}

	s.mu.Lock()
	sess, ok = s.findLocked(slug)
	if !ok || !sess.alive(now) {
		s.mu.Unlock()
		_ = os.Remove(bodyPath)
		return false, nil
	}
	sess.Entries = append(sess.Entries, e)
	s.evictLocked(sess)
	_ = s.saveLocked()
	s.mu.Unlock()

	if s.hub != nil {
		s.hub.Broadcast("hook", map[string]any{"slug": slug, "entry": e})
	}
	return true, nil
}

// evictLocked 淘汰最旧的条目：超过条数或超过单会话字节上限时。
func (s *hookStore) evictLocked(sess *HookSession) {
	dir := s.sessionDir(sess.Slug)
	total := sess.totalBytes()
	for len(sess.Entries) > 1 && (len(sess.Entries) > hookEntriesPer || total > hookSessionBytes) {
		old := sess.Entries[0]
		total -= old.BodyBytes
		sess.Entries = sess.Entries[1:]
		_ = os.Remove(filepath.Join(dir, old.BodyFile))
		if old.ImageFile != "" {
			_ = os.Remove(filepath.Join(dir, old.ImageFile))
		}
	}
}

// extractJSONImage 从 JSON 报文里解出 image 字段的 base64 图片并存盘。
func extractJSONImage(bodyPath, dir string, id uint64) (string, bool) {
	data, err := os.ReadFile(bodyPath)
	if err != nil {
		return "", false
	}
	var probe struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.Image == "" {
		return "", false
	}
	payload, err := decodeImagePayload(probe.Image)
	if err != nil || len(payload) == 0 {
		return "", false
	}
	ext := ".jpg"
	switch http.DetectContentType(payload) {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/gif":
		ext = ".gif"
	case "image/webp":
		ext = ".webp"
	default:
		return "", false
	}
	name := strconv.FormatUint(id, 10) + ext
	target := filepath.Join(dir, name)
	if err := os.WriteFile(target, payload, 0o600); err != nil {
		return "", false
	}
	_ = os.Chmod(target, 0o600)
	return name, true
}

// ServeHTTP 接收 /hook/<slug>[/...]。未知或失效地址一律 404，不区分原因。
func (s *hookStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		http.NotFound(w, r)
		return
	}
	accepted, err := s.Append(slug, r, time.Now())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败"})
		return
	}
	if !accepted {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// imageBytes 供 /api/hook/entry/<id>/image 使用。
func (s *hookStore) imageBytes(id uint64) ([]byte, string, bool) {
	e, slug, ok := s.EntryByID(id)
	if !ok {
		return nil, "", false
	}
	name := e.ImageFile
	if name == "" && e.IsImage {
		name = e.BodyFile
	}
	if name == "" {
		return nil, "", false
	}
	if strings.ContainsAny(name, `/\`) {
		return nil, "", false
	}
	data, err := os.ReadFile(filepath.Join(s.sessionDir(slug), name))
	if err != nil {
		return nil, "", false
	}
	ct := http.DetectContentType(data)
	if !strings.HasPrefix(ct, "image/") {
		ct = "application/octet-stream"
	}
	return data, ct, true
}

// bodyBytes 供 /api/hook/entry/<id>/body 使用（原始报文下载）。
func (s *hookStore) bodyBytes(id uint64) ([]byte, bool) {
	e, slug, ok := s.EntryByID(id)
	if !ok || e.BodyFile == "" || strings.ContainsAny(e.BodyFile, `/\`) {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(s.sessionDir(slug), e.BodyFile))
	if err != nil {
		return nil, false
	}
	return data, true
}
