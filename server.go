package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// notifyRequest 入站通知负载，对应三月七小助手 Webhook 渠道 body 模板。
type notifyRequest struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Image   string `json:"image"` // base64，可带 data URI 前缀
	Source  string `json:"source"` // 可选来源标识，缺省 WebHook
}

// newMux 组装网关 HTTP 路由：入站推送 + Web UI。
func newMux(tok *tokenState, qq *QQClient, ui *webUI) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /notify", func(w http.ResponseWriter, r *http.Request) {
		handleNotify(tok, qq, ui, w, r)
	})

	// Web UI（内嵌前端与 API）
	mux.HandleFunc("GET /api/stats", ui.handleStats)
	mux.HandleFunc("GET /api/records", ui.handleRecords)
	mux.HandleFunc("GET /api/records/{id}/image", ui.handleRecordImage)
	mux.HandleFunc("GET /api/config", ui.handleConfig)
	mux.HandleFunc("GET /api/logs", ui.handleLogs)
	mux.HandleFunc("GET /api/webhook", ui.handleWebhookGet)
	mux.HandleFunc("PUT /api/webhook", ui.handleWebhookPut)
	mux.HandleFunc("GET /api/openid", ui.handleOpenIDGet)
	mux.HandleFunc("POST /api/openid/listen", ui.handleOpenIDListen)
	mux.HandleFunc("POST /api/openid/stop", ui.handleOpenIDStop)
	mux.HandleFunc("PUT /api/target", ui.handleTargetPut)
	mux.HandleFunc("POST /api/token/reset", ui.handleTokenReset)
	mux.HandleFunc("GET /api/ws", ui.hub.ServeWS)
	mux.HandleFunc("GET /", ui.handleIndex)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func handleNotify(tok *tokenState, qq *QQClient, ui *webUI, w http.ResponseWriter, r *http.Request) {
	// 入站校验：token 非空时必须携带正确请求头（生产启动即自动生成，必然非空）
	if tok != nil {
		if effective := tok.Snapshot(); effective != "" &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Gateway-Token")), []byte(effective)) != 1 {
			// 鉴权失败视为攻击噪声，不入推送记录
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "invalid gateway token"})
			return
		}
	}

	var (
		req           notifyRequest
		imageData     []byte
		imageExt      string
		recordContent string
	)

	// 统一出口：写记录（含 WS 广播）后应答
	respond := func(code int, errMsg string) {
		if ui != nil {
			status := "ok"
			if code != http.StatusOK {
				status = "error"
			}
			src := strings.TrimSpace(req.Source)
			if src == "" {
				src = "WebHook"
			}
			ui.store.Add(src, status, errMsg, recordContent, imageData, imageExt)
		}
		payload := map[string]any{"ok": code == http.StatusOK}
		if errMsg != "" {
			payload["error"] = errMsg
		}
		writeJSON(w, code, payload)
	}

	// base64 图片最多约为原图 4/3，32MB 上限足够 20MB 图片
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			respond(http.StatusRequestEntityTooLarge, "请求体过大")
			return
		}
		respond(http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}

	text := mergeText(req.Title, req.Content)
	recordContent = text

	imageData, err := decodeImagePayload(req.Image)
	if err != nil {
		respond(http.StatusBadRequest, err.Error())
		return
	}
	if len(imageData) > 0 {
		if len(imageData) > maxImageBytes {
			respond(http.StatusRequestEntityTooLarge,
				fmt.Sprintf("图片 %d 字节超过上限 %d", len(imageData), maxImageBytes))
			return
		}
		mime := http.DetectContentType(imageData)
		switch mime {
		case "image/jpeg":
			imageExt = ".jpg"
		case "image/png":
			imageExt = ".png"
		default:
			respond(http.StatusUnsupportedMediaType, "仅支持 JPEG/PNG 图片，实际类型: "+mime)
			return
		}
	}

	if text == "" && len(imageData) == 0 {
		respond(http.StatusBadRequest, "title、content、image 不能全部为空")
		return
	}

	if text != "" {
		if err := qq.SendText(r.Context(), text); err != nil {
			log.Printf("发送文本失败: %v", err)
			respond(http.StatusBadGateway, err.Error())
			return
		}
	}
	if len(imageData) > 0 {
		fileInfo, err := qq.UploadImage(r.Context(), "notify"+imageExt, imageData)
		if err != nil {
			log.Printf("上传图片失败: %v", err)
			respond(http.StatusBadGateway, err.Error())
			return
		}
		if err := qq.SendImage(r.Context(), fileInfo); err != nil {
			log.Printf("发送图片失败: %v", err)
			respond(http.StatusBadGateway, err.Error())
			return
		}
	}
	respond(http.StatusOK, "")
}

// mergeText 非空部分按行合并。
func mergeText(title, content string) string {
	title = strings.TrimSpace(title)
	content = strings.TrimSpace(content)
	switch {
	case title != "" && content != "":
		return title + "\n" + content
	case title != "":
		return title
	default:
		return content
	}
}
