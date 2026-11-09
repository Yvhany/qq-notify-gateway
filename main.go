package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tencent-connect/botgo/constant"
)

func main() {
	// 官方文档现行 token 端点为 api.bot.qq.com；老的 bots.qq.com 对新应用返回 100002
	constant.TokenDomain = "https://api.bot.qq.com"

	// 关闭 botgo SDK 的 Debug 日志：它会把 AppSecret 与 AccessToken 明文写进日志文件/WebUI
	installQuietBotgoLogger()

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

	// 单通道时代的 token 种子：只在**首次**生成 channels.json 时用作 default 通道的
	// token，之后以 channels.json 为准。channels.json 已存在时完全不碰 webui.json，
	// 避免每次启动都白写一次盘。
	persisted := loadStateFrom(dataDir)
	channelsPath := filepath.Join(dataDir, "channels.json")
	needMigration := false
	if _, statErr := os.Stat(channelsPath); os.IsNotExist(statErr) {
		needMigration = true
	}
	seedToken := strings.TrimSpace(persisted.GatewayToken)
	if needMigration {
		if seedToken == "" {
			seedToken = strings.TrimSpace(cfg.GatewayToken)
		}
		if seedToken == "" {
			v, err := randomToken()
			if err != nil {
				log.Fatalf("生成入站 token 失败: %v", err)
			}
			seedToken = v
		}
		if persisted.GatewayToken != seedToken {
			persisted.GatewayToken = seedToken
			if err := saveStateTo(dataDir, persisted); err != nil {
				log.Printf("警告: token 种子持久化失败: %v", err)
			}
		}
	}

	// 通道配置：唯一真源，落盘 DATA_DIR/channels.json
	channels, err := LoadChannels(dataDir, cfg, seedToken)
	if err != nil {
		log.Fatalf("加载通道配置失败: %v", err)
	}
	log.Printf("已加载 %d 个通道，默认通道=%s", len(channels.List()), channels.Default())

	// 迁移完成后清掉 webui.json 里的 token 种子：它只在首次生成 channels.json 时有意义，
	// 留着会变成一个「复活旧 token」的暗坑（channels.json 一旦丢失会拿它重建通道）。
	if strings.TrimSpace(persisted.GatewayToken) != "" {
		persisted.GatewayToken = ""
		if err := saveStateTo(dataDir, persisted); err != nil {
			log.Printf("警告: 清理 webui.json 中已失效的 token 种子失败: %v", err)
		}
	}

	// 每通道一个 QQ 客户端，懒创建
	clients := newChannelHub(ctx, 60*time.Second)
	defer clients.Close()

	// 保留策略：数据卷持久化值；>0 时启动即清理一次，并每 6 小时定期清理
	retention := newRetentionState(persisted.RetentionDays)
	if cutoff, ok := retentionCutoff(time.Now(), persisted.RetentionDays); ok {
		if n, err := store.PurgeBefore(cutoff); err != nil {
			log.Printf("警告: 启动清理失败: %v", err)
		} else if n > 0 {
			log.Printf("保留策略 %d 天：启动清理 %d 条", persisted.RetentionDays, n)
		}
	}
	store.SetOnPurge(func(n int) {
		hub.Broadcast("records_purged", map[string]any{"removed": n})
		hub.Broadcast("stats", store.Stats())
	})
	go func() {
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				days := retention.Snapshot()
				if cutoff, ok := retentionCutoff(time.Now(), days); ok {
					if n, err := store.PurgeBefore(cutoff); err != nil {
						log.Printf("定期清理失败: %v", err)
					} else if n > 0 {
						log.Printf("定期清理：删除 %d 条（保留 %d 天）", n, days)
					}
				}
			}
		}
	}()

	listener := newOpenIDListener(hub)
	notify := newNotifier(channels, clients, store)
	ui := newWebUI(cfg, store, hub, dataDir, channels, clients, listener, retention)
	core := newMux(notify, ui)

	// 双端口拆分：
	//   API 口（LISTEN_ADDR）   —— 仅 /notify 与 /notify/<通道>，按通道校验；反代可整口免登录放行
	//   UI  口（UI_LISTEN_ADDR）—— 页面/统计/记录/通道管理/日志/WS；由反代登录保护，网关不再加鉴权
	apiSrv := &http.Server{
		Addr: cfg.ListenAddr,
		Handler: routeFilter(core, func(r *http.Request) bool {
			return isAPIPath(r.URL.Path)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	var uiSrv *http.Server
	if cfg.UIListenAddr != "" {
		uiSrv = &http.Server{
			Addr: cfg.UIListenAddr,
			Handler: routeFilter(core, func(r *http.Request) bool {
				return !isAPIPath(r.URL.Path)
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

	log.Printf("API(推送)监听 %s —— POST /notify（默认通道）与 POST /notify/<通道>，按通道校验", cfg.ListenAddr)
	if uiSrv != nil {
		log.Printf("UI 监听 %s —— 页面/记录/通道管理/日志/WS（由反代登录保护）", cfg.UIListenAddr)
	} else {
		log.Printf("UI 监听已禁用（UI_LISTEN_ADDR 为空）")
	}
	for _, ch := range channels.List() {
		state := "启用"
		if !ch.Enabled {
			state = "停用"
		}
		log.Printf("  通道 %-12s 类型=%-7s %s 目标=%s/%s", ch.ID, ch.Type, state, ch.QQ.TargetType, openidTail(ch.QQ.TargetOpenID))
	}

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
