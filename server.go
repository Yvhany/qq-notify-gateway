package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// notifyRequest generic 型入站负载，对应三月七小助手 Webhook 渠道 body 模板。
// Message 仅在顶层字段缺失时作为兜底来源，见 normalize。
type notifyRequest struct {
	Title   string         `json:"title"`
	Content string         `json:"content"`
	Image   string         `json:"image"`  // base64，可带 data URI 前缀
	Source  string         `json:"source"` // 可选来源标识，缺省 WebHook
	Message *fknockMessage `json:"message"`
}

// fknockMessage FnKnock「标准通知 Body」的 message 段。
// 该格式把标题和正文嵌在 message 里，顶层只有 source/provider_type 之类元数据。
type fknockMessage struct {
	Title        string       `json:"title"`
	BodyText     string       `json:"body_text"`
	BodyMarkdown string       `json:"body_markdown"`
	Summary      string       `json:"summary"`
	Severity     string       `json:"severity"`
	Facts        []fknockFact `json:"facts"`
}

// fknockFact 键值对形式的结构化字段，如「登录 IP：192.0.2.10」。
type fknockFact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// normalize 把 FnKnock 的嵌套 body 摊平到 Title/Content：
// 顶层字段优先，逐项兜底；facts 以「标签：值」逐行追加，空标签或空值跳过。
func (r *notifyRequest) normalize() {
	m := r.Message
	if m == nil {
		return
	}
	if strings.TrimSpace(r.Title) == "" {
		r.Title = strings.TrimSpace(m.Title)
	}
	if strings.TrimSpace(r.Content) == "" {
		// 优先纯文本：QQ 不渲染 markdown
		r.Content = firstNonEmpty(m.BodyText, m.BodyMarkdown, m.Summary)
	}

	lines := make([]string, 0, len(m.Facts))
	for _, f := range m.Facts {
		label := strings.TrimSpace(f.Label)
		value := strings.TrimSpace(f.Value)
		if label == "" || value == "" {
			continue
		}
		lines = append(lines, label+"："+value)
	}
	if len(lines) == 0 {
		return
	}
	base := strings.TrimSpace(r.Content)
	facts := strings.Join(lines, "\n")
	if base == "" {
		r.Content = facts
		return
	}
	r.Content = base + "\n\n" + facts
}

// firstNonEmpty 返回第一个 trim 后非空的字符串，全空返回 ""。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// notifier 入站推送编排：通道查找 → 按类型解析/鉴权 → QQ 发送 → 落记录。
type notifier struct {
	channels *channelStore
	clients  *channelHub
	dedup    *dedupSet // 按通道隔离的幂等窗口
	store    *RecordStore
}

func newNotifier(chs *channelStore, clients *channelHub, store *RecordStore) *notifier {
	return &notifier{
		channels: chs,
		clients:  clients,
		dedup:    newDedupSet(fnosDedupCapacity),
		store:    store,
	}
}

var errBodyTooLarge = errors.New("请求体过大")

// routeFilter 在共享 mux 之上做端口级路由裁剪：
// API 端口只放行 /notify 与 /notify/{id}，UI 端口屏蔽它们（防打错口造成"假成功"）。
func routeFilter(next http.Handler, allow func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allow(r) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isAPIPath API 端口放行的入站路径：/notify[/<通道>] 与 /hook[/...]（测试收件箱）。
// UI 端口一律屏蔽这些路径，避免有人打到 UI 口还以为推送成功。
func isAPIPath(path string) bool {
	return path == "/notify" || strings.HasPrefix(path, "/notify/") ||
		path == "/hook" || strings.HasPrefix(path, "/hook/")
}

// newMux 组装网关 HTTP 路由：入站推送 + Web UI（两个监听器共用）。
func newMux(n *notifier, ui *webUI) http.Handler {
	mux := http.NewServeMux()

	// 入站：/notify 走默认通道，/notify/<id> 走指定通道
	mux.HandleFunc("POST /notify", func(w http.ResponseWriter, r *http.Request) {
		handleNotify(n, "", w, r)
	})
	mux.HandleFunc("POST /notify/{id}", func(w http.ResponseWriter, r *http.Request) {
		handleNotify(n, r.PathValue("id"), w, r)
	})

	// 测试收件箱：任意方法、任意子路径，只记录不推送。
	// 必须按方法逐个注册：无方法模式 "/hook/" 会与 "GET /" 产生冲突（方法集不同但路径更泛）。
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		mux.HandleFunc(m+" /hook/{slug}", ui.hook.ServeHTTP)
		mux.HandleFunc(m+" /hook/{slug}/{rest...}", ui.hook.ServeHTTP)
	}

	// Web UI（内嵌前端与 API）
	mux.HandleFunc("GET /api/stats", ui.handleStats)
	mux.HandleFunc("GET /api/records", ui.handleRecords)
	mux.HandleFunc("GET /api/records/{id}/image", ui.handleRecordImage)
	mux.HandleFunc("GET /api/records/{id}", ui.handleRecordGet)
	mux.HandleFunc("POST /api/records/purge", ui.handleRecordsPurge)
	mux.HandleFunc("POST /api/retention", ui.handleRetentionPost)
	mux.HandleFunc("GET /api/config", ui.handleConfig)
	mux.HandleFunc("GET /api/logs", ui.handleLogs)
	mux.HandleFunc("GET /api/webhook", ui.handleWebhookGet)
	mux.HandleFunc("PUT /api/webhook", ui.handleWebhookPut)
	mux.HandleFunc("GET /api/openid", ui.handleOpenIDGet)
	mux.HandleFunc("POST /api/openid/listen", ui.handleOpenIDListen)
	mux.HandleFunc("POST /api/openid/stop", ui.handleOpenIDStop)
	mux.HandleFunc("PUT /api/target", ui.handleTargetPut)
	mux.HandleFunc("POST /api/token/reset", ui.handleTokenReset)

	// 通道管理
	mux.HandleFunc("GET /api/channels", ui.handleChannelsList)
	mux.HandleFunc("POST /api/channels", ui.handleChannelCreate)
	mux.HandleFunc("PUT /api/channels/{id}", ui.handleChannelUpdate)
	mux.HandleFunc("DELETE /api/channels/{id}", ui.handleChannelDelete)
	mux.HandleFunc("POST /api/channels/{id}/test", ui.handleChannelTest)
	mux.HandleFunc("POST /api/channels/{id}/default", ui.handleChannelSetDefault)
	mux.HandleFunc("POST /api/channels/{id}/secret", ui.handleChannelReveal)
	mux.HandleFunc("POST /api/channels/{id}/token/reset", ui.handleChannelTokenReset)

	mux.HandleFunc("GET /api/ws", ui.hub.ServeWS)
	mux.HandleFunc("GET /api/hook", ui.handleHookState)
	mux.HandleFunc("POST /api/hook/new", ui.handleHookNew)
	mux.HandleFunc("POST /api/hook/revoke", ui.handleHookRevoke)
	mux.HandleFunc("GET /api/hook/entry/{id}/image", ui.handleHookEntryImage)
	mux.HandleFunc("GET /api/hook/entry/{id}/body", ui.handleHookEntryBody)
	mux.HandleFunc("GET /", ui.handleIndex)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// handleNotify 入站入口：解析通道并按类型分派。
func handleNotify(n *notifier, id string, w http.ResponseWriter, r *http.Request) {
	if id == "" {
		id = n.channels.Default()
	}
	ch, ok := n.channels.Get(id)
	if !ok {
		// 未知通道：只写系统日志，不落推送记录。
		// 该接口无鉴权，若对任意路径都写记录，任何人都能无限刷大 records.jsonl。
		log.Printf("收到未知通道的推送请求: %q（来自 %s）", id, r.RemoteAddr)
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "通道不存在: " + id})
		return
	}
	if !ch.Enabled {
		// 已配置但被停用属于用户可见的配置问题，落记录便于排查（ID 必须真实存在才会走到这里）
		n.store.AddChannel(id, "WebHook", "error", "通道已停用: "+id, "", nil, "")
		log.Printf("通道 %s 已停用，拒绝推送", id)
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "通道已停用: " + id})
		return
	}
	switch ch.Type {
	case ChannelTypeFnOS:
		handleFnOSNotify(n, ch, w, r)
	default:
		handleGenericNotify(n, ch, w, r)
	}
}

// readRawBody 读入原始请求体（保留字节用于验签），超限返回 errBodyTooLarge。
func readRawBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errBodyTooLarge
		}
		return nil, err
	}
	return data, nil
}

// handleGenericNotify 原有契约：{title,content,image} + X-Gateway-Token。
func handleGenericNotify(n *notifier, ch Channel, w http.ResponseWriter, r *http.Request) {
	// 入站校验：通道配了 token 就必须携带正确请求头
	if tok := strings.TrimSpace(ch.Token); tok != "" &&
		subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Gateway-Token")), []byte(tok)) != 1 {
		// 鉴权失败视为攻击噪声，不入推送记录
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "invalid gateway token"})
		return
	}

	var (
		req           notifyRequest
		imageData     []byte
		imageExt      string
		recordContent string
		detail        *RequestDetail
	)

	// 统一出口：写记录（含 WS 广播）后应答
	respond := func(code int, errMsg string) {
		status := "ok"
		if code != http.StatusOK {
			status = "error"
		}
		src := strings.TrimSpace(req.Source)
		if src == "" {
			src = "WebHook"
		}
		n.store.AddChannelDetail(ch.ID, src, status, errMsg, recordContent, imageData, imageExt, detail)
		payload := map[string]any{"ok": code == http.StatusOK}
		if errMsg != "" {
			payload["error"] = errMsg
		}
		writeJSON(w, code, payload)
	}

	// base64 图片最多约为原图 4/3，32MB 上限足够 20MB 图片
	raw, err := readRawBody(w, r, 32<<20)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			respond(http.StatusRequestEntityTooLarge, "请求体过大")
			return
		}
		respond(http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		detail = captureDetail(r, raw)
		respond(http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}
	detail = captureDetail(r, raw)
	// 兼容 FnKnock 的「标准通知 Body」：正文在 message 段里
	req.normalize()

	text := mergeText(req.Title, req.Content)
	recordContent = text

	imageData, err = decodeImagePayload(req.Image)
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

	qq, err := n.clients.Client(ch)
	if err != nil {
		log.Printf("通道 %s 获取 QQ 客户端失败: %v", ch.ID, err)
		respond(http.StatusBadGateway, err.Error())
		return
	}

	if text != "" {
		if err := qq.SendText(r.Context(), text); err != nil {
			log.Printf("通道 %s 发送文本失败: %v", ch.ID, err)
			respond(http.StatusBadGateway, err.Error())
			return
		}
	}
	if len(imageData) > 0 {
		fileInfo, err := qq.UploadImage(r.Context(), "notify"+imageExt, imageData)
		if err != nil {
			log.Printf("通道 %s 上传图片失败: %v", ch.ID, err)
			respond(http.StatusBadGateway, err.Error())
			return
		}
		if err := qq.SendImage(r.Context(), fileInfo); err != nil {
			log.Printf("通道 %s 发送图片失败: %v", ch.ID, err)
			respond(http.StatusBadGateway, err.Error())
			return
		}
	}
	respond(http.StatusOK, "")
}

// handleFnOSNotify 飞牛契约：固定结构 payload + x-sac-signature 签名校验。
func handleFnOSNotify(n *notifier, ch Channel, w http.ResponseWriter, r *http.Request) {
	raw, err := readRawBody(w, r, 32<<20)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": "请求体过大"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "读取请求体失败: " + err.Error()})
		return
	}

	// 验签在解析之前：签名对象是原始字节，且失败请求不应入库
	if ch.hasSigningSecret() {
		if err := verifyFnOSSignature(
			ch.SigningSecret,
			r.Header.Get(fnosTSHeader),
			r.Header.Get(fnosSigHeader),
			raw,
			ch.TimestampToleranceSec,
			time.Now(),
		); err != nil {
			log.Printf("飞牛通道 %s 验签失败: %v", ch.ID, err)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}

	// 验签通过后即可留快照：验签失败属攻击噪声，照旧不入库
	detail := captureDetail(r, raw)
	addRec := func(status, errMsg, text string) {
		n.store.AddChannelDetail(ch.ID, "fnos", status, errMsg, text, nil, "", detail)
	}

	var p fnosPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		addRec("error", "JSON 解析失败: "+err.Error(), "")
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败: " + err.Error()})
		return
	}

	text := p.message()
	if text == "" {
		addRec("error", "title 与 body 均为空", "")
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "title 与 body 不能同时为空"})
		return
	}

	// 幂等：飞牛推送失败会重试。
	// 必须先占位再投递：占位成功才允许发送，投递成功后登记完成，失败则释放占位
	// 让重试真正重新投递——若投递前就登记完成，一次失败就会让告警永久丢失。
	dedup := n.dedup.For(ch.ID)
	res, finish := dedup.Claim(p.EventID)
	switch res {
	case claimDone:
		log.Printf("飞牛通道 %s 收到重复 eventId=%s，已忽略", ch.ID, p.EventID)
		addRec("ok", "重复 eventId，已忽略", text)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duplicated": true})
		return
	case claimInFlight:
		// 同一事件正在投递中。不能回 200：若先到的那次最终失败，回 200 就等于
		// 告诉飞牛「成功了」而实际没发出去。回 503 让它稍后再试。
		log.Printf("飞牛通道 %s 的 eventId=%s 正在投递中，请稍后重试", ch.ID, p.EventID)
		w.Header().Set("Retry-After", "2")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "同一 eventId 正在投递中，请稍后重试",
		})
		return
	}

	// 兜底：无论从哪条路径返回，都必须结算占位，避免 panic 或漏写导致占位永久泄漏
	delivered := false
	defer func() { finish(delivered) }()

	qq, err := n.clients.Client(ch)
	if err != nil {
		log.Printf("飞牛通道 %s 获取 QQ 客户端失败: %v", ch.ID, err)
		addRec("error", err.Error(), text)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := qq.SendText(r.Context(), text); err != nil {
		log.Printf("飞牛通道 %s 发送失败: %v", ch.ID, err)
		addRec("error", err.Error(), text)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	delivered = true
	addRec("ok", "", text)
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
