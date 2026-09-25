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
	ID      uint64 `json:"id"`
	TS      string `json:"ts"` // RFC3339 本地时区
	Source  string `json:"source"`
	Status  string `json:"status"` // ok | error
	Error   string `json:"error,omitempty"`
	Content string `json:"content"`
	Snippet string `json:"snippet"`
	HasImage bool  `json:"has_image"`
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
	TodayCount      int      `json:"today_count"`
	TodaySuccess    int      `json:"today_success"`
	TodayRate       float64  `json:"today_rate"` // 0-100，今日无记录为 0
	TotalCount      int      `json:"total_count"`
	LastStatus      string   `json:"last_status"`
	DailyCounts     []int    `json:"daily_counts"` // 近 7 日 [旧→新]
	DailySuccess    []int    `json:"daily_success"`
}

// RecordStore 推送记录存储：内存 + JSONL 追加 + 图片文件。
type RecordStore struct {
	mu       sync.Mutex
	dir      string
	records  []Record // 旧→新
	nextID   uint64
	onChange func(Record) // 新记录回调（用于 WS 广播），回调在锁外触发
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

// Add 写入一条记录并持久化；带图时先存图片文件。
func (s *RecordStore) Add(source, status, errMsg, content string, image []byte, imageExt string) Record {
	s.mu.Lock()
	rec := Record{
		ID:       s.nextID,
		TS:       time.Now().Format(time.RFC3339),
		Source:   source,
		Status:   status,
		Error:    errMsg,
		Content:  content,
		Snippet:  makeSnippet(content),
		HasImage: len(image) > 0,
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

// List 返回记录（新→旧），支持 offset/limit。
func (s *RecordStore) List(offset, limit int) (total int, out []Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	total = len(s.records)
	if offset < 0 {
		offset = 0
	}
	// 新→旧遍历
	rev := make([]Record, 0, len(s.records))
	for i := len(s.records) - 1; i >= 0; i-- {
		rev = append(rev, s.records[i])
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

// Stats 计算看板统计（按本机时区的“今日”与近 7 日）。
func (s *RecordStore) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var st Stats
	st.TotalCount = len(s.records)
	days := make([]time.Time, 7)
	for i := 0; i < 7; i++ {
		days[i] = today.AddDate(0, 0, i-6)
	}
	st.DailyCounts = make([]int, 7)
	st.DailySuccess = make([]int, 7)

	for _, r := range s.records {
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
	if n := len(s.records); n > 0 {
		st.LastStatus = s.records[n-1].Status
	}
	return st
}
