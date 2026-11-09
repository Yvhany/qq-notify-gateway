package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxRecordsInMemory = 2000 // 启动载入与内存上限
	maxImagesRetained  = 200  // 图片文件保留张数
	snippetRunes       = 10   // 记录页内容截断字数
)

// Record 一条推送记录（持久化于 records.jsonl，不含图片字节）。
type Record struct {
	ID       uint64 `json:"id"`
	TS       string `json:"ts"`                // RFC3339 本地时区
	Channel  string `json:"channel,omitempty"` // 所属通道 ID；旧记录缺失视为 default
	Source   string `json:"source"`
	Status   string `json:"status"` // ok | error
	Error    string `json:"error,omitempty"`
	Content  string `json:"content"`
	Snippet  string `json:"snippet"`
	HasImage bool   `json:"has_image"`
	Detail   *RequestDetail `json:"detail,omitempty"` // 入站请求快照；旧记录无此字段
}

// RequestDetail 入站请求的原始快照，供详情页排查用。
// 敏感请求头已打码，请求体超 detailBodyLimit 时截断并置 BodyTruncated。
type RequestDetail struct {
	Method        string            `json:"method,omitempty"`
	Path          string            `json:"path,omitempty"`
	RemoteIP      string            `json:"remote_ip,omitempty"`
	ContentType   string            `json:"content_type,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	Body          string            `json:"body,omitempty"`
	BodyBytes     int               `json:"body_bytes,omitempty"`
	BodyTruncated bool              `json:"body_truncated,omitempty"`
}

// channelOf 记录所属通道；历史记录没有该字段时归入 default。
func channelOf(r Record) string {
	if strings.TrimSpace(r.Channel) == "" {
		return defaultChannelID
	}
	return r.Channel
}

// matchChannel 空 filter 表示不过滤。
func matchChannel(r Record, filter string) bool {
	filter = strings.TrimSpace(filter)
	return filter == "" || channelOf(r) == filter
}

// makeSnippet 按 rune 截取前 10 字，超出追加三个点。
func makeSnippet(content string) string {
	runes := []rune(strings.TrimSpace(content))
	if len(runes) <= snippetRunes {
		return string(runes)
	}
	return string(runes[:snippetRunes]) + "..."
}

// Stats 数据看板统计。
type Stats struct {
	TodayCount   int     `json:"today_count"`
	TodaySuccess int     `json:"today_success"`
	TodayRate    float64 `json:"today_rate"` // 0-100，今日无记录为 0
	TotalCount   int     `json:"total_count"`
	LastStatus   string  `json:"last_status"`
	DailyCounts  []int   `json:"daily_counts"` // 近 7 日 [旧→新]
	DailySuccess []int   `json:"daily_success"`
}

// RecordStore 推送记录存储：内存 + JSONL 追加 + 图片文件。
type RecordStore struct {
	mu       sync.Mutex
	dir      string
	records  []Record // 旧→新
	nextID   uint64
	onChange func(Record) // 新记录回调（用于 WS 广播），回调在锁外触发
	onPurge  func(int)    // 清理回调（删除条数），回调在锁外触发
}

// NewRecordStore 载入历史记录并准备图片目录。
func NewRecordStore(dir string) (*RecordStore, error) {
	s := &RecordStore{dir: dir, nextID: 1}
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		return nil, fmt.Errorf("创建记录目录失败: %w", err)
	}
	path := filepath.Join(dir, "records.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var all []Record
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		all = append(all, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取记录失败: %w", err)
	}
	if len(all) > maxRecordsInMemory {
		all = all[len(all)-maxRecordsInMemory:]
	}
	s.records = all
	// nextID 取历史最大值+1（JSONL 行序理论上与写入序一致，但以 max 兜底防撞号）
	var maxID uint64
	for _, r := range all {
		if r.ID > maxID {
			maxID = r.ID
		}
	}
	s.nextID = maxID + 1
	s.pruneImages()
	return s, nil
}

// SetOnChange 注册新记录回调（WS 广播用）。
func (s *RecordStore) SetOnChange(f func(Record)) { s.onChange = f }

// SetOnPurge 注册清理回调（删除条数，WS 广播用）。
func (s *RecordStore) SetOnPurge(f func(int)) { s.onPurge = f }

// PurgeBefore 删除 ts 早于 cutoff 的记录及其图片，返回删除条数。
// 记录文件原子重写；图片按“是否仍被保留记录引用”清扫（含历史孤立文件）。
func (s *RecordStore) PurgeBefore(cutoff time.Time) (int, error) {
	s.mu.Lock()
	kept := make([]Record, 0, len(s.records))
	keptIDs := make(map[uint64]struct{}, len(s.records))
	removed := 0
	for _, r := range s.records {
		ts, err := time.Parse(time.RFC3339, r.TS)
		if err == nil && ts.Before(cutoff) {
			removed++
			continue
		}
		kept = append(kept, r)
		keptIDs[r.ID] = struct{}{}
	}
	if removed == 0 {
		s.mu.Unlock()
		return 0, nil
	}
	s.records = kept
	// 持锁重写，避免与 Add 的追加写交叉导致丢行
	err := s.rewriteLocked()
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}

	// 清扫图片：不再被保留记录引用的文件一律删除
	files, _ := filepath.Glob(filepath.Join(s.dir, "images", "*"))
	for _, p := range files {
		base := filepath.Base(p)
		if i := strings.IndexByte(base, '.'); i > 0 {
			base = base[:i]
		}
		id, perr := strconv.ParseUint(base, 10, 64)
		if perr != nil {
			continue
		}
		if _, ok := keptIDs[id]; !ok {
			_ = os.Remove(p)
		}
	}

	if s.onPurge != nil {
		s.onPurge(removed)
	}
	return removed, nil
}

// rewriteLocked 原子重写 records.jsonl（调用方必须持有 s.mu）。
func (s *RecordStore) rewriteLocked() error {
	path := filepath.Join(s.dir, "records.jsonl")
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("重写记录文件失败: %w", err)
	}
	w := bufio.NewWriter(f)
	for _, r := range s.records {
		line, merr := json.Marshal(r)
		if merr != nil {
			continue
		}
		if _, werr := w.Write(append(line, '\n')); werr != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("写入记录文件失败: %w", werr)
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("刷新记录文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Add 写入一条记录并持久化（不指定通道，归入 default）。
func (s *RecordStore) Add(source, status, errMsg, content string, image []byte, imageExt string) Record {
	return s.AddChannel(defaultChannelID, source, status, errMsg, content, image, imageExt)
}

// AddChannel 写入一条带通道归属的记录并持久化；带图时先存图片文件。
func (s *RecordStore) AddChannel(channel, source, status, errMsg, content string, image []byte, imageExt string) Record {
	return s.AddChannelDetail(channel, source, status, errMsg, content, image, imageExt, nil)
}

// AddChannelDetail 同 AddChannel，额外落一条入站请求快照（detail 可为 nil）。
func (s *RecordStore) AddChannelDetail(channel, source, status, errMsg, content string, image []byte, imageExt string, detail *RequestDetail) Record {
	s.mu.Lock()
	rec := Record{
		ID:       s.nextID,
		TS:       time.Now().Format(time.RFC3339),
		Channel:  channel,
		Source:   source,
		Status:   status,
		Error:    errMsg,
		Content:  content,
		Snippet:  makeSnippet(content),
		HasImage: len(image) > 0,
		Detail:   detail,
	}
	s.nextID++
	s.records = append(s.records, rec)
	if len(s.records) > maxRecordsInMemory {
		s.records = s.records[len(s.records)-maxRecordsInMemory:]
	}
	s.mu.Unlock()

	// 持久化（追加一行）
	if f, err := os.OpenFile(filepath.Join(s.dir, "records.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		if line, err := json.Marshal(rec); err == nil {
			_, _ = f.Write(append(line, '\n'))
		}
		_ = f.Close()
	}

	// 图片落盘
	if rec.HasImage {
		if imageExt == "" {
			imageExt = ".jpg"
		}
		imgPath := filepath.Join(s.dir, "images", fmt.Sprintf("%d%s", rec.ID, imageExt))
		_ = os.WriteFile(imgPath, image, 0o644)
		s.pruneImages()
	}

	if s.onChange != nil {
		s.onChange(rec)
	}
	return rec
}

// ImagePath 返回记录对应图片路径（不存在返回空）。
func (s *RecordStore) ImagePath(id uint64) string {
	matches, _ := filepath.Glob(filepath.Join(s.dir, "images", fmt.Sprintf("%d.*", id)))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// pruneImages 仅保留最新 maxImagesRetained 张图片文件（按记录 id 数值排序）。
func (s *RecordStore) pruneImages() {
	files, _ := filepath.Glob(filepath.Join(s.dir, "images", "*"))
	if len(files) <= maxImagesRetained {
		return
	}
	sortByImageID(files)
	for _, p := range files[:len(files)-maxImagesRetained] {
		_ = os.Remove(p)
	}
}

// sortByImageID 按文件名前缀的数值 id 升序（忽略扩展名差异）。
func sortByImageID(paths []string) {
	idOf := func(p string) uint64 {
		base := filepath.Base(p)
		if i := strings.IndexByte(base, '.'); i > 0 {
			base = base[:i]
		}
		id, err := strconv.ParseUint(base, 10, 64)
		if err != nil {
			return 0
		}
		return id
	}
	sort.Slice(paths, func(i, j int) bool { return idOf(paths[i]) < idOf(paths[j]) })
}

// List 返回记录（新→旧），支持 offset/limit（不按通道过滤）。
func (s *RecordStore) List(offset, limit int) (total int, out []Record) {
	return s.ListByChannel("", offset, limit)
}

// ListByChannel 按通道过滤返回记录（新→旧）；channel 为空表示全部通道。
func (s *RecordStore) ListByChannel(channel string, offset, limit int) (total int, out []Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 新→旧遍历
	rev := make([]Record, 0, len(s.records))
	for i := len(s.records) - 1; i >= 0; i-- {
		if !matchChannel(s.records[i], channel) {
			continue
		}
		rev = append(rev, s.records[i])
	}
	total = len(rev)
	if offset < 0 {
		offset = 0
	}
	if offset >= len(rev) {
		return total, nil
	}
	end := offset + limit
	if limit <= 0 || end > len(rev) {
		end = len(rev)
	}
	return total, rev[offset:end]
}

// GetByID 按记录 id 取单条（供记录详情页，带完整正文）。
// records 按追加顺序保存、id 单调递增，故从尾部往前找更快命中新记录。
func (s *RecordStore) GetByID(id uint64) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.records) - 1; i >= 0; i-- {
		if s.records[i].ID == id {
			return s.records[i], true
		}
	}
	return Record{}, false
}

// Recent 返回最新的 n 条（旧→新，供事件流/初值）。
func (s *RecordStore) Recent(n int) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > len(s.records) {
		n = len(s.records)
	}
	out := make([]Record, n)
	copy(out, s.records[len(s.records)-n:])
	return out
}

// Stats 计算看板统计（全部通道）。
func (s *RecordStore) Stats() Stats {
	return s.StatsByChannel("")
}

// StatsByChannel 计算看板统计（按本机时区的“今日”与近 7 日）；channel 为空表示全部。
func (s *RecordStore) StatsByChannel(channel string) Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var st Stats
	days := make([]time.Time, 7)
	for i := 0; i < 7; i++ {
		days[i] = today.AddDate(0, 0, i-6)
	}
	st.DailyCounts = make([]int, 7)
	st.DailySuccess = make([]int, 7)

	for _, r := range s.records {
		if !matchChannel(r, channel) {
			continue
		}
		st.TotalCount++
		ts, err := time.Parse(time.RFC3339, r.TS)
		if err != nil {
			continue
		}
		if !ts.Before(today) {
			st.TodayCount++
			if r.Status == "ok" {
				st.TodaySuccess++
			}
		}
		day := time.Date(ts.Year(), ts.Month(), ts.Day(), 0, 0, 0, 0, ts.Location())
		for i, d := range days {
			if day.Equal(d) {
				st.DailyCounts[i]++
				if r.Status == "ok" {
					st.DailySuccess[i]++
				}
			}
		}
	}
	if st.TodayCount > 0 {
		st.TodayRate = float64(st.TodaySuccess) / float64(st.TodayCount) * 100
	}
	// 最近状态取过滤范围内的最后一条
	for i := len(s.records) - 1; i >= 0; i-- {
		if matchChannel(s.records[i], channel) {
			st.LastStatus = s.records[i].Status
			break
		}
	}
	return st
}
