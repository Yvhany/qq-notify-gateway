package main

import (
	"bytes"
	"log"
	"strings"
	"testing"

	botgolog "github.com/tencent-connect/botgo/log"
)

// TestQuietBotgoLoggerDropsSecrets SDK 的 Debug/Info 会打印 AppSecret 与 AccessToken，
// 必须被丢弃；Warn/Error 保留并带 botgo 前缀。
func TestQuietBotgoLoggerDropsSecrets(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	l := quietBotgoLogger{}
	l.Debug("clientSecret=LEAKED")
	l.Debugf("access_token=%s", "LEAKED")
	l.Info("clientSecret=LEAKED")
	l.Infof("access_token=%s", "LEAKED")
	if buf.Len() != 0 {
		t.Fatalf("Debug/Info 不应产生任何输出，实际: %q", buf.String())
	}
	if err := l.Sync(); err != nil {
		t.Errorf("Sync 应返回 nil: %v", err)
	}

	l.Warnf("something %d", 1)
	l.Error("boom")
	out := buf.String()
	if !strings.Contains(out, "botgo: something 1") {
		t.Errorf("Warn 应转发并带前缀: %q", out)
	}
	if !strings.Contains(out, "botgo: boom") {
		t.Errorf("Error 应转发并带前缀: %q", out)
	}
	if strings.Contains(out, "LEAKED") {
		t.Errorf("输出中不应出现敏感值: %q", out)
	}
}

// TestInstallQuietBotgoLogger 安装后全局 logger 被替换。
func TestInstallQuietBotgoLogger(t *testing.T) {
	installQuietBotgoLogger()
	if _, ok := botgolog.DefaultLogger.(quietBotgoLogger); !ok {
		t.Fatalf("默认 logger 应被替换为 quietBotgoLogger，实际 %T", botgolog.DefaultLogger)
	}
}
