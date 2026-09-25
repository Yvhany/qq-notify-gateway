package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tencent-connect/botgo/constant"
	"github.com/tencent-connect/botgo/token"
)

func main() {
	// 官方文档现行 token 端点为 api.bot.qq.com；老的 bots.qq.com 对新应用返回 100002
	constant.TokenDomain = "https://api.bot.qq.com"

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 一次性模式：抓取 openid 后退出
	if len(os.Args) > 1 && (os.Args[1] == "-bootstrap" || os.Args[1] == "--bootstrap") {
		runBootstrap(ctx)
		return
	}

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}

	// 数据层：记录持久化 + WS hub + 日志落盘（需在业务日志前就位）
	dataDir := getenvDefault("DATA_DIR", "data")
	store, err := NewRecordStore(dataDir)
	if err != nil {
		log.Fatalf("初始化记录存储失败: %v", err)
	}
	hub := NewHub()
	if err := setupLog(dataDir, hub); err != nil {
		log.Fatalf("初始化日志文件失败: %v", err)
	}
	store.SetOnChange(func(rec Record) {
		hub.Broadcast("record", rec)
		hub.Broadcast("stats", store.Stats())
	})

	// 持久化状态只读一次：目标覆盖 + 入站 token 解析共用
	persisted := loadStateFrom(dataDir)

	// 推送目标：.env 为初始值，数据卷 webui.json 的持久化目标优先（重启保持）
	target := newTargetState(cfg.TargetType, cfg.TargetOpenID)
	if persisted.TargetOpenID != "" {
		typ := persisted.TargetType
		if typ == "" {
			typ = cfg.TargetType
		}
		if err := target.Update(typ, persisted.TargetOpenID); err != nil {
			log.Printf("警告: 持久化目标覆盖失败（沿用 .env 初值）: %v", err)
		} else {
			log.Printf("已加载持久化推送目标: type=%s id=%s", typ, persisted.TargetOpenID)
		}
	}
	if typ, id := target.Snapshot(); typ != cfg.TargetType || id != cfg.TargetOpenID {
		log.Printf("生效推送目标: type=%s id=%s（初始 .env: type=%s id=%s）", typ, id, cfg.TargetType, cfg.TargetOpenID)
	}

	// 入站校验 token：webui.json → 环境变量 → 自动生成并持久化（token 值不入日志）
	tokVal := persisted.GatewayToken
	if tokVal == "" {
		tokVal = strings.TrimSpace(cfg.GatewayToken)
	}
	if tokVal == "" {
		v, err := randomToken()
		if err != nil {
			log.Fatalf("生成入站 token 失败: %v", err)
		}
		tokVal = v
	}
	if persisted.GatewayToken != tokVal {
		persisted.GatewayToken = tokVal
		if err := saveStateTo(dataDir, persisted); err != nil {
			log.Printf("警告: 入站 token 持久化失败（重启后将重新生成）: %v", err)
		}
	}
	tok := newTokenState(tokVal)
	log.Printf("入站校验 token 已启用（值不记入日志，可在系统配置页查看/重置）")

	listener := newOpenIDListener(hub)

	// botgo token source：atomic 缓存 + singleflight + 后台自动刷新
	tokenSource := token.NewQQBotTokenSource(&token.QQBotCredentials{
		AppID:     cfg.AppID,
		AppSecret: cfg.Secret,
	})
	if err := token.StartRefreshAccessToken(ctx, tokenSource); err != nil {
		log.Fatalf("初始化 AccessToken 失败: %v", err)
	}

	qq := NewQQClient(cfg, tokenSource, 60*time.Second, target)
	ui := newWebUI(cfg, store, hub, dataDir, target, qq, listener, tok)
	core := newMux(tok, qq, ui)

	// 双端口拆分：
	//   API 口（LISTEN_ADDR）   —— 仅 POST /notify，token 校验；反代可整口免登录放行
	//   UI  口（UI_LISTEN_ADDR）—— 页面/统计/记录/日志/WS；由反代登录保护，网关不再加鉴权
	apiSrv := &http.Server{
		Addr: cfg.ListenAddr,
		Handler: routeFilter(core, func(r *http.Request) bool {
			return r.URL.Path == "/notify"
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	var uiSrv *http.Server
	if cfg.UIListenAddr != "" {
		uiSrv = &http.Server{
			Addr: cfg.UIListenAddr,
			Handler: routeFilter(core, func(r *http.Request) bool {
				return r.URL.Path != "/notify"
			}),
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = apiSrv.Shutdown(shutdownCtx)
		if uiSrv != nil {
			_ = uiSrv.Shutdown(shutdownCtx)
		}
	}()

	typ, id := target.Snapshot()
	log.Printf("API(推送)监听 %s —— 仅 POST /notify，token 校验", cfg.ListenAddr)
	if uiSrv != nil {
		log.Printf("UI 监听 %s —— 页面/记录/日志/WS（由反代登录保护）", cfg.UIListenAddr)
	} else {
		log.Printf("UI 监听已禁用（UI_LISTEN_ADDR 为空）")
	}
	log.Printf("目标=%s openid=%s api=%s", typ, id, cfg.APIBase)

	errCh := make(chan error, 2)
	go func() {
		if err := apiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	if uiSrv != nil {
		go func() {
			if err := uiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	select {
	case err := <-errCh:
		log.Fatalf("服务退出: %v", err)
	case <-ctx.Done():
		log.Println("收到退出信号，正在关闭服务")
	}
}
