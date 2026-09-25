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

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
		Handler:           newMux(cfg, qq),
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
