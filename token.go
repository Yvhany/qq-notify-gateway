package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

// tokenState 入站校验 token：运行时可重置，读写有锁。
type tokenState struct {
	mu  sync.RWMutex
	val string // 空表示不校验（仅测试路径；生产启动时必然非空）
}

func newTokenState(v string) *tokenState {
	return &tokenState{val: strings.TrimSpace(v)}
}

// Snapshot 返回当前 token。
func (s *tokenState) Snapshot() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.val
}

// Set 设置 token。
func (s *tokenState) Set(v string) {
	s.mu.Lock()
	s.val = strings.TrimSpace(v)
	s.mu.Unlock()
}

// Reset 生成并应用新 token，返回新值。
func (s *tokenState) Reset() (string, error) {
	v, err := randomToken()
	if err != nil {
		return "", err
	}
	s.Set(v)
	return v, nil
}

// randomToken 生成 32 字节熵的 hex token。
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机 token 失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
