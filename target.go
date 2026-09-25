package main

import (
	"fmt"
	"strings"
	"sync"
)

// targetState 可在运行时切换的推送目标（单聊/群组），读写有锁。
// QQClient 每次发送取快照，避免数据竞争。
type targetState struct {
	mu sync.RWMutex
	typ string // c2c | group
	id  string
}

func newTargetState(typ, id string) *targetState {
	return &targetState{typ: typ, id: id}
}

// Snapshot 返回当前目标快照。
func (s *targetState) Snapshot() (typ, id string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.typ, s.id
}

// Update 校验并更新目标。
func (s *targetState) Update(typ, id string) error {
	typ = strings.ToLower(strings.TrimSpace(typ))
	id = strings.TrimSpace(id)
	if typ != "c2c" && typ != "group" {
		return fmt.Errorf("目标类型必须是 c2c 或 group")
	}
	if !validOpenID(id) {
		return fmt.Errorf("OpenID 格式无效")
	}
	s.mu.Lock()
	s.typ, s.id = typ, id
	s.mu.Unlock()
	return nil
}

// validOpenID 允许官方加密 openid 的字符集与长度。
func validOpenID(id string) bool {
	if len(id) < 4 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '!':
			continue
		default:
			return false
		}
	}
	return true
}
