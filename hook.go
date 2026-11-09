package main

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// hookInboxCap 收件箱保留的请求条数（内存环形缓冲，重启清空）。
	hookInboxCap = 100
	// hookReadMax 单次最多读取的请求体字节数，超出部分丢弃并标记截断。
	hookReadMax = 1 << 20
)

// HookEntry 一条测试请求的快照。
type HookEntry struct {
	ID        uint64            `json:"id"`
	TS        string            `json:"ts"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Query     string            `json:"query,omitempty"`
	Proto     string            `json:"proto,omitempty"`
	RemoteIP  string            `json:"remote_ip"`
	Headers   map[string]string `json:"headers"`
	Body      string            `json:"body"`
	BodyBytes int               `json:"body_bytes"`
	Truncated bool              `json:"truncated"`
}

// hookInbox 「Webhook 测试」页的收件箱：任何打到 /hook 的请求都会记录并保留最近 N 条，
// 不推送 QQ、不校验 token、不落 records.jsonl。
type hookInbox struct {
	mu      sync.Mutex
	entries []HookEntry
	nextID  uint64
	hub     *Hub
}

func newHookInbox(hub *Hub) *hookInbox {
	return &hookInbox{hub: hub, nextID: 1}
}

// ServeHTTP 接收任意方法、任意子路径，记录后回执。
func (h *hookInbox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, hookReadMax))

	e := HookEntry{
		Method:    r.Method,
		Path:      r.URL.Path,
		Query:     r.URL.RawQuery,
		Proto:     r.Proto,
		RemoteIP:  clientIP(r),
		Headers:   make(map[string]string, len(r.Header)),
		BodyBytes: len(raw),
	}
	for k, vs := range r.Header {
		v := strings.Join(vs, ", ")
		if sensitiveHeader(k) {
			v = maskSecret(v)
		}
		e.Headers[k] = v
	}
	if len(raw) > detailBodyLimit {
		cut := detailBodyLimit
		for cut > 0 && !utf8Start(raw[cut]) {
			cut--
		}
		e.Body = string(raw[:cut])
		e.Truncated = true
	} else {
		e.Body = string(raw)
	}

	h.mu.Lock()
	e.ID = h.nextID
	h.nextID++
	e.TS = time.Now().Format(time.RFC3339)
	h.entries = append(h.entries, e)
	if len(h.entries) > hookInboxCap {
		h.entries = h.entries[len(h.entries)-hookInboxCap:]
	}
	h.mu.Unlock()

	if h.hub != nil {
		h.hub.Broadcast("hook", e)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"id":             e.ID,
		"received_bytes": len(raw),
	})
}

// List 返回最近的 limit 条（新→旧）与总数。
func (h *hookInbox) List(limit int) (total int, out []HookEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	total = len(h.entries)
	if limit <= 0 || limit > len(h.entries) {
		limit = len(h.entries)
	}
	out = make([]HookEntry, 0, limit)
	for i := len(h.entries) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, h.entries[i])
	}
	return total, out
}

// Clear 清空收件箱，返回清除条数。
func (h *hookInbox) Clear() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(h.entries)
	h.entries = nil
	return n
}
