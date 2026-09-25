package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// QQAPIError QQ OpenAPI 返回的业务或 HTTP 错误。
type QQAPIError struct {
	Status  int
	Code    int
	Message string
	TraceID string
}

func (e *QQAPIError) Error() string {
	msg := fmt.Sprintf("QQ API 错误 (http=%d code=%d)", e.Status, e.Code)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.TraceID != "" {
		msg += " trace=" + e.TraceID
	}
	return msg
}

// QQClient 出站调用 QQ 开放平台 API v2 的客户端。
// AccessToken 来自 botgo 的 token source（自动缓存与刷新）。
type QQClient struct {
	cfg    Config
	tokens oauth2.TokenSource
	http   *http.Client
	target *targetState // 运行时可切换的推送目标
}

// NewQQClient 创建客户端。timeout 为单次 HTTP 调用上限。
func NewQQClient(cfg Config, tokens oauth2.TokenSource, timeout time.Duration, target *targetState) *QQClient {
	if target == nil {
		target = newTargetState(cfg.TargetType, cfg.TargetOpenID)
	}
	return &QQClient{
		cfg:    cfg,
		tokens: tokens,
		http:   &http.Client{Timeout: timeout},
		target: target,
	}
}

// targetPrefix 按当前目标快照返回 /v2/users/{openid} 或 /v2/groups/{openid}。
func (c *QQClient) targetPrefix() string {
	typ, id := c.target.Snapshot()
	if typ == "group" {
		return "/v2/groups/" + id
	}
	return "/v2/users/" + id
}

func (c *QQClient) authorize(req *http.Request) error {
	tok, err := c.tokens.Token()
	if err != nil {
		return fmt.Errorf("获取 AccessToken 失败: %w", err)
	}
	req.Header.Set("Authorization", "QQBot "+tok.AccessToken)
	req.Header.Set("X-Union-Appid", c.cfg.AppID)
	return nil
}

// rawToken 返回当前 AccessToken（WS Identify 用）。
func (c *QQClient) rawToken() (string, error) {
	tok, err := c.tokens.Token()
	if err != nil {
		return "", fmt.Errorf("获取 AccessToken 失败: %w", err)
	}
	return tok.AccessToken, nil
}

// gatewayURL 获取 WS 长连接接入点（运行时 OpenID 采集用）。
func (c *QQClient) gatewayURL(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.APIBase+"/gateway/bot", nil)
	if err != nil {
		return "", err
	}
	if err := c.authorize(req); err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("获取 WS 接入点失败: %w", err)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := parseQQResponse(resp, &out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", fmt.Errorf("WS 接入点为空")
	}
	return out.URL, nil
}

// postJSON 向 APIBase+path 发送带鉴权的 JSON POST。
// out 非 nil 时把 2xx 响应体解析进去。
func (c *QQClient) postJSON(ctx context.Context, path string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("构造请求体失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIBase+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.authorize(req); err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("请求 %s 失败: %w", path, err)
	}
	return parseQQResponse(resp, out)
}

// parseQQResponse 统一处理 QQ 响应：非 2xx 或业务 code!=0 都是错误。
func parseQQResponse(resp *http.Response, out any) error {
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}
	traceID := resp.Header.Get("X-Tps-trace-ID")

	var envelope struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	decoded := json.Unmarshal(data, &envelope) == nil

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := &QQAPIError{Status: resp.StatusCode, TraceID: traceID}
		if decoded {
			e.Code = envelope.Code
			e.Message = envelope.Message
		}
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		return e
	}
	if decoded && envelope.Code != 0 {
		return &QQAPIError{Status: resp.StatusCode, Code: envelope.Code, Message: envelope.Message, TraceID: traceID}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("解析响应失败: %w (body=%s)", err, truncate(string(data), 200))
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// SendText 发送文本主动消息（msg_type 0）。
func (c *QQClient) SendText(ctx context.Context, content string) error {
	body := map[string]any{
		"content": content,
		"msg_type": 0,
	}
	return c.postJSON(ctx, c.targetPrefix()+"/messages", body, nil)
}

// SendImage 用已上传的 file_info 发送富媒体图片消息（msg_type 7）。
func (c *QQClient) SendImage(ctx context.Context, fileInfo string) error {
	body := map[string]any{
		"msg_type": 7,
		"media":    map[string]any{"file_info": fileInfo},
	}
	return c.postJSON(ctx, c.targetPrefix()+"/messages", body, nil)
}
