package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
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
	ui := newWebUI(cfg, store, hub, dataDir)

	// botgo token source：atomic 缓存 + singleflight + 后台自动刷新
	tokenSource := token.NewQQBotTokenSource(&token.QQBotCredentials{
		AppID:     cfg.AppID,
		AppSecret: cfg.Secret,
	})
	if err := token.StartRefreshAccessToken(ctx, tokenSource); err != nil {
		log.Fatalf("初始化 AccessToken 失败: %v", err)
	}

	qq := NewQQClient(cfg, tokenSource, 60*time.Second)
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           newMux(cfg, qq, ui),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("qq-notify-gateway 监听 %s，目标=%s openid=%s api=%s",
		cfg.ListenAddr, cfg.TargetType, cfg.TargetOpenID, cfg.APIBase)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("服务退出: %v", err)
	}
}
