package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// randomToken 生成 32 字节熵的 hex token（通道入站校验用）。
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机 token 失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
