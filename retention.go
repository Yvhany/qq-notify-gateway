package main

import (
	"sync"
	"time"
)

// retentionState 记录/图片保留策略（天）。0 = 永久保留。
type retentionState struct {
	mu   sync.RWMutex
	days int
}

func newRetentionState(days int) *retentionState {
	return &retentionState{days: days}
}

// Snapshot 返回保留天数（0=永久）。
func (s *retentionState) Snapshot() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.days
}

// Set 更新保留天数。
func (s *retentionState) Set(days int) {
	s.mu.Lock()
	s.days = days
	s.mu.Unlock()
}

// cutoff 按保留天数计算清理截止时间；0 返回零值（表示不清理）。
func retentionCutoff(now time.Time, days int) (time.Time, bool) {
	if days <= 0 {
		return time.Time{}, false
	}
	return now.AddDate(0, 0, -days), true
}
