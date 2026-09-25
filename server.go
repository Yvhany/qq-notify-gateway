package main

import (
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
}

// newMux 组装网关 HTTP 路由。
func newMux(cfg Config, qq *QQClient) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /notify", func(w http.ResponseWriter, r *http.Request) {
		handleNotify(cfg, qq, w, r)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func handleNotify(cfg Config, qq *QQClient, w http.ResponseWriter, r *http.Request) {
	if cfg.GatewayToken != "" && r.Header.Get("X-Gateway-Token") != cfg.GatewayToken {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "invalid gateway token"})
		return
	}

	// base64 图片最多约为原图 4/3，32MB 上限足够 20MB 图片
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	var req notifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": "请求体过大"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败: " + err.Error()})
		return
	}

	imageData, err := decodeImagePayload(req.Image)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	imageName := ""
	if len(imageData) > 0 {
		if len(imageData) > maxImageBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"ok": false,
				"error": fmt.Sprintf("图片 %d 字节超过上限 %d", len(imageData), maxImageBytes),
			})
			return
		}
		mime := http.DetectContentType(imageData)
		switch mime {
		case "image/jpeg":
			imageName = "notify.jpg"
		case "image/png":
			imageName = "notify.png"
		default:
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{
				"ok": false, "error": "仅支持 JPEG/PNG 图片，实际类型: " + mime,
			})
			return
		}
	}

	text := mergeText(req.Title, req.Content)
	if text == "" && len(imageData) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "title、content、image 不能全部为空"})
		return
	}

	if text != "" {
		if err := qq.SendText(r.Context(), text); err != nil {
			log.Printf("发送文本失败: %v", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}
	if len(imageData) > 0 {
		fileInfo, err := qq.UploadImage(r.Context(), imageName, imageData)
		if err != nil {
			log.Printf("上传图片失败: %v", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if err := qq.SendImage(r.Context(), fileInfo); err != nil {
			log.Printf("发送图片失败: %v", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
