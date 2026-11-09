package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// lanAPIBase 内网基地址（用于在 UI 展示各通道的入站地址）。
// 由环境变量 LAN_API_BASE 提供，例如 http://192.168.1.10:18080；为空时前端用当前主机名推断。
func lanAPIBase() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("LAN_API_BASE")), "/")
}

// normalizePublicBase 把「公网基地址」规整成不带尾部 /notify 的形式。
// 历史数据里这一栏存的是完整端点（如 https://push.example.com/notify），
// 多通道后改为基地址语义，这里做兼容，避免拼出 /notify/notify/<id>。
func normalizePublicBase(u string) string {
	u = strings.TrimRight(strings.TrimSpace(u), "/")
	u = strings.TrimSuffix(u, "/notify")
	return strings.TrimRight(u, "/")
}

// channelView 生成单通道的 API 视图：密钥打码 + 入站路径 + 是否已配置密钥。
func (u *webUI) channelView(ch Channel, defID, publicBase string) map[string]any {
	m := ch.masked()
	path := "/notify/" + ch.ID
	view := map[string]any{
		"id":                      ch.ID,
		"name":                    ch.Name,
		"type":                    ch.Type,
		"enabled":                 ch.Enabled,
		"created_at":              ch.CreatedAt,
		"is_default":              ch.ID == defID,
		"inbound_path":            path,
		"token_masked":            m.Token,
		"has_token":               ch.hasToken(),
		"signing_secret_masked":   m.SigningSecret,
		"has_signing_secret":      ch.hasSigningSecret(),
		"timestamp_tolerance_sec": ch.TimestampToleranceSec,
		"app_id":                  ch.QQ.AppID,
		"app_secret_masked":       m.QQ.AppSecret,
		"target_type":             ch.QQ.TargetType,
		"target_openid":           ch.QQ.TargetOpenID,
		"target_openid_tail":      openidTail(ch.QQ.TargetOpenID),
		"sandbox":                 ch.QQ.Sandbox,
		"api_base":                ch.qqConfig().APIBase,
		"lan_base":                lanAPIBase(),
		"public_url":              "",
	}
	if base := normalizePublicBase(publicBase); base != "" {
		view["public_url"] = base + path
	}
	return view
}

// openidTail 取 OpenID 末 6 位用于列表展示（避免整串暴露在页面上）。
func openidTail(id string) string {
	r := []rune(id)
	if len(r) <= 6 {
		return string(r)
	}
	return "..." + string(r[len(r)-6:])
}

// handleChannelsList GET /api/channels
func (u *webUI) handleChannelsList(w http.ResponseWriter, _ *http.Request) {
	st := u.loadState()
	defID := u.channels.Default()
	base := strings.TrimRight(strings.TrimSpace(st.WebhookURL), "/")
	list := u.channels.List()
	out := make([]map[string]any, 0, len(list))
	for _, ch := range list {
		out = append(out, u.channelView(ch, defID, base))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"default_channel": defID,
		"channels":        out,
	})
}

// channelQQReq 更新请求里的 QQ 段（指针为空表示沿用原值）。
type channelQQReq struct {
	AppID        *string `json:"app_id"`
	AppSecret    *string `json:"app_secret"`
	TargetType   *string `json:"target_type"`
	TargetOpenID *string `json:"target_openid"`
	Sandbox      *bool   `json:"sandbox"`
	APIBase      *string `json:"api_base"`
}

// channelReq 通道新增/修改请求。指针为空 = 保留原值（避免把打码占位符写回）。
type channelReq struct {
	ID                    *string       `json:"id"`
	Name                  *string       `json:"name"`
	Type                  *string       `json:"type"`
	Enabled               *bool         `json:"enabled"`
	Token                 *string       `json:"token"`
	SigningSecret         *string       `json:"signing_secret"`
	TimestampToleranceSec *int          `json:"timestamp_tolerance_sec"`
	QQ                    *channelQQReq `json:"qq"`
}

func decodeChannelReq(w http.ResponseWriter, r *http.Request) (channelReq, error) {
	var req channelReq
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, fmt.Errorf("请求体无效或过大")
	}
	return req, nil
}

// applyReq 把请求字段覆盖到通道上（未提供的字段保持原值）。
func (c *Channel) applyReq(req channelReq) {
	if req.Name != nil {
		c.Name = *req.Name
	}
	if req.Type != nil {
		c.Type = *req.Type
	}
	if req.Enabled != nil {
		c.Enabled = *req.Enabled
	}
	if req.Token != nil {
		c.Token = *req.Token
	}
	if req.SigningSecret != nil {
		c.SigningSecret = *req.SigningSecret
	}
	if req.TimestampToleranceSec != nil {
		c.TimestampToleranceSec = *req.TimestampToleranceSec
	}
	if q := req.QQ; q != nil {
		if q.AppID != nil {
			c.QQ.AppID = *q.AppID
		}
		if q.AppSecret != nil {
			c.QQ.AppSecret = *q.AppSecret
		}
		if q.TargetType != nil {
			c.QQ.TargetType = *q.TargetType
		}
		if q.TargetOpenID != nil {
			c.QQ.TargetOpenID = *q.TargetOpenID
		}
		if q.Sandbox != nil {
			c.QQ.Sandbox = *q.Sandbox
		}
		if q.APIBase != nil {
			c.QQ.APIBase = *q.APIBase
		}
	}
}

// handleChannelCreate POST /api/channels
func (u *webUI) handleChannelCreate(w http.ResponseWriter, r *http.Request) {
	req, err := decodeChannelReq(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if req.ID == nil || strings.TrimSpace(*req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少通道 ID"})
		return
	}
	var ch Channel
	ch.ID = *req.ID
	ch.Enabled = true
	ch.Type = ChannelTypeGeneric
	ch.applyReq(req)
	// 新建时未指定 token 就自动生成一个，避免出现“裸奔”通道
	if req.Token == nil {
		tok, err := randomToken()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		ch.Token = tok
	}
	// 飞牛通道：显式给了容差就用它（0 表示不校验时间），没给才补默认值
	if ch.Type == ChannelTypeFnOS && req.TimestampToleranceSec == nil && ch.hasSigningSecret() {
		ch.TimestampToleranceSec = defaultFnOSTolerance
	}
	created, err := u.channels.Create(ch)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	u.clients.Drop(created.ID)
	st := u.loadState()
	u.hub.Broadcast("channels", map[string]any{"action": "create", "id": created.ID})
	log.Printf("新增通道 %s（类型 %s），入站路径 /notify/%s", created.ID, created.Type, created.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"channel": u.channelView(created, u.channels.Default(), strings.TrimRight(st.WebhookURL, "/")),
	})
}

// handleChannelUpdate PUT /api/channels/{id}
func (u *webUI) handleChannelUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur, ok := u.channels.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "通道不存在: " + id})
		return
	}
	req, err := decodeChannelReq(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cur.applyReq(req)
	updated, err := u.channels.Update(id, cur)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 凭证/目标可能变化 → 丢掉旧客户端，下次推送按需重建
	u.clients.Drop(updated.ID)
	st := u.loadState()
	u.hub.Broadcast("channels", map[string]any{"action": "update", "id": updated.ID})
	log.Printf("通道 %s 配置已更新", updated.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"channel": u.channelView(updated, u.channels.Default(), strings.TrimRight(st.WebhookURL, "/")),
	})
}

// handleChannelDelete DELETE /api/channels/{id}
func (u *webUI) handleChannelDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := u.channels.Delete(id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	u.clients.Drop(id)
	u.hub.Broadcast("channels", map[string]any{"action": "delete", "id": id})
	log.Printf("通道 %s 已删除", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleChannelSetDefault POST /api/channels/{id}/default
func (u *webUI) handleChannelSetDefault(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := u.channels.SetDefault(id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	u.hub.Broadcast("channels", map[string]any{"action": "default", "id": id})
	log.Printf("默认通道已切换为 %s（POST /notify 将发往此通道）", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "default_channel": id})
}

// handleChannelReveal POST /api/channels/{id}/secret — 返回真实密钥值（打码字段的显示按钮用）
func (u *webUI) handleChannelReveal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ch, ok := u.channels.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "通道不存在: " + id})
		return
	}
	var req struct {
		Field string `json:"field"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req)
	var val string
	switch strings.TrimSpace(req.Field) {
	case "token":
		val = ch.Token
	case "signing_secret":
		val = ch.SigningSecret
	case "app_secret":
		val = ch.QQ.AppSecret
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "field 必须是 token / signing_secret / app_secret"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "field": req.Field, "value": val})
}

// handleChannelTokenReset POST /api/channels/{id}/token/reset — 为指定通道生成新 token
func (u *webUI) handleChannelTokenReset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	candidate, err := randomToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if _, err := u.channels.SetToken(id, candidate); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	log.Printf("通道 %s 的入站 token 已重置", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channel": id, "token": candidate})
}

// handleChannelTest POST /api/channels/{id}/test — 走该通道的真实 QQ 调用发一条测试消息
func (u *webUI) handleChannelTest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ch, ok := u.channels.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "通道不存在: " + id})
		return
	}
	text := fmt.Sprintf("【通道测试】%s\n%s 这一条来自 Web 控制台的测试消息。",
		ch.Name, time.Now().Format("2006-01-02 15:04:05"))

	qq, err := u.clients.Client(ch)
	if err != nil {
		u.store.AddChannel(ch.ID, "WebUI测试", "error", err.Error(), text, nil, "")
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := qq.SendText(r.Context(), text); err != nil {
		u.store.AddChannel(ch.ID, "WebUI测试", "error", err.Error(), text, nil, "")
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	u.store.AddChannel(ch.ID, "WebUI测试", "ok", "", text, nil, "")
	log.Printf("通道 %s 测试消息已发送", ch.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
