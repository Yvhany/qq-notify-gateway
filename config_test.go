package main

import (
	"strings"
	"testing"
)

func TestLoadConfigValid(t *testing.T) {
	t.Setenv("QQ_APP_ID", "aid")
	t.Setenv("QQ_SECRET", "sec")
	t.Setenv("TARGET_TYPE", "C2C") // 大小写不敏感
	t.Setenv("TARGET_OPENID", "oid")
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("QQ_API_BASE", "")
	t.Setenv("QQ_SANDBOX", "")
	t.Setenv("GATEWAY_TOKEN", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("期望成功，得到 %v", err)
	}
	if cfg.TargetType != "c2c" {
		t.Errorf("TargetType 应小写化: %q", cfg.TargetType)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("默认端口错误: %q", cfg.ListenAddr)
	}
	if cfg.APIBase != "https://api.sgroup.qq.com" {
		t.Errorf("默认 API 域名错误: %q", cfg.APIBase)
	}
}

func TestLoadConfigSandbox(t *testing.T) {
	t.Setenv("QQ_APP_ID", "aid")
	t.Setenv("QQ_SECRET", "sec")
	t.Setenv("TARGET_TYPE", "group")
	t.Setenv("TARGET_OPENID", "oid")
	t.Setenv("QQ_API_BASE", "")
	t.Setenv("QQ_SANDBOX", "true")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIBase != "https://sandbox.api.sgroup.qq.com" {
		t.Errorf("sandbox 域名错误: %q", cfg.APIBase)
	}
}

func TestLoadConfigMissing(t *testing.T) {
	t.Setenv("QQ_APP_ID", "")
	t.Setenv("QQ_SECRET", "")
	t.Setenv("TARGET_TYPE", "c2c")
	t.Setenv("TARGET_OPENID", "")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("缺少必填项应报错")
	}
	for _, key := range []string{"QQ_APP_ID", "QQ_SECRET", "TARGET_OPENID"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("错误信息应包含 %s: %v", key, err)
		}
	}
}

func TestLoadConfigBadTargetType(t *testing.T) {
	t.Setenv("QQ_APP_ID", "aid")
	t.Setenv("QQ_SECRET", "sec")
	t.Setenv("TARGET_TYPE", "频道")
	t.Setenv("TARGET_OPENID", "oid")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("非法 TARGET_TYPE 应报错")
	}
}

func TestMergeText(t *testing.T) {
	if got := mergeText("标题", "正文"); got != "标题\n正文" {
		t.Errorf("合并错误: %q", got)
	}
	if got := mergeText("", "正文"); got != "正文" {
		t.Errorf("仅正文: %q", got)
	}
	if got := mergeText("标题", " "); got != "标题" {
		t.Errorf("空白正文应忽略: %q", got)
	}
	if got := mergeText(" ", " "); got != "" {
		t.Errorf("全空白应为空: %q", got)
	}
}
