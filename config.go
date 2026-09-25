package main

import (
	"fmt"
	"os"
	"strings"
)

// Config 网关全部运行配置，来自环境变量（.env 注入）。
type Config struct {
	AppID        string
	Secret       string
	TargetType   string // c2c | group
	TargetOpenID string
	ListenAddr   string
	APIBase      string
	Sandbox      bool
	GatewayToken string
}

func getenvDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// LoadConfig 从环境变量加载并校验配置。
func LoadConfig() (Config, error) {
	c := Config{
		AppID:        strings.TrimSpace(os.Getenv("QQ_APP_ID")),
		Secret:       strings.TrimSpace(os.Getenv("QQ_SECRET")),
		TargetType:   strings.ToLower(strings.TrimSpace(os.Getenv("TARGET_TYPE"))),
		TargetOpenID: strings.TrimSpace(os.Getenv("TARGET_OPENID")),
		ListenAddr:   getenvDefault("LISTEN_ADDR", ":8080"),
		Sandbox:      strings.EqualFold(strings.TrimSpace(os.Getenv("QQ_SANDBOX")), "true"),
		GatewayToken: os.Getenv("GATEWAY_TOKEN"),
	}
	explicitBase := strings.TrimSpace(os.Getenv("QQ_API_BASE"))
	if explicitBase != "" {
		c.APIBase = strings.TrimRight(explicitBase, "/")
	} else if c.Sandbox {
		c.APIBase = "https://sandbox.api.sgroup.qq.com"
	} else {
		c.APIBase = "https://api.sgroup.qq.com"
	}

	var missing []string
	if c.AppID == "" {
		missing = append(missing, "QQ_APP_ID")
	}
	if c.Secret == "" {
		missing = append(missing, "QQ_SECRET")
	}
	if c.TargetOpenID == "" {
		missing = append(missing, "TARGET_OPENID")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("缺少必填环境变量: %s", strings.Join(missing, ", "))
	}
	if c.TargetType != "c2c" && c.TargetType != "group" {
		return Config{}, fmt.Errorf("TARGET_TYPE 必须是 c2c 或 group，当前值: %q", c.TargetType)
	}
	return c, nil
}
