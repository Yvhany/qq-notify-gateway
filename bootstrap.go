package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/tencent-connect/botgo"
	"github.com/tencent-connect/botgo/dto"
	"github.com/tencent-connect/botgo/event"
	"github.com/tencent-connect/botgo/token"
)

type capture struct {
	kind string // c2c | group
	id   string
}

// extractOpenID 从 WS 事件帧的原始 JSON 里解析 openid。
// 帧结构为 {"op":0,"t":"...","d":{...}}，openid 位于 d 内。
func extractOpenID(kind string, raw []byte) string {
	var frame struct {
		D json.RawMessage `json:"d"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil || len(frame.D) == 0 {
		return ""
	}
	if kind == "c2c" {
		var payload struct {
			Author struct {
				UserOpenID string `json:"user_openid"`
			} `json:"author"`
		}
		if err := json.Unmarshal(frame.D, &payload); err != nil {
			return ""
		}
		return payload.Author.UserOpenID
	}
	var payload struct {
		GroupOpenID string `json:"group_openid"`
	}
	if err := json.Unmarshal(frame.D, &payload); err != nil {
		return ""
	}
	return payload.GroupOpenID
}

// runBootstrap 一次性 WS 长连接抓取 openid：qq-notify-gateway -bootstrap
// 只需要 QQ_APP_ID / QQ_SECRET；收到一条消息后打印 openid 并退出。
func runBootstrap(ctx context.Context) {
	appID := strings.TrimSpace(os.Getenv("QQ_APP_ID"))
	secret := strings.TrimSpace(os.Getenv("QQ_SECRET"))
	if appID == "" || secret == "" {
		log.Fatalln("-bootstrap 需要环境变量 QQ_APP_ID 与 QQ_SECRET")
	}

	tokenSource := token.NewQQBotTokenSource(&token.QQBotCredentials{
		AppID:     appID,
		AppSecret: secret,
	})
	if err := token.StartRefreshAccessToken(ctx, tokenSource); err != nil {
		log.Fatalf("初始化 AccessToken 失败: %v", err)
	}

	captured := make(chan capture, 8)
	intent := event.RegisterHandlers(
		event.ReadyHandler(func(_ *dto.WSPayload, _ *dto.WSReadyData) {
			log.Println("WS 长连接就绪（READY）")
		}),
		event.ErrorNotifyHandler(func(err error) {
			log.Printf("WS 连接错误: %v", err)
		}),
		event.C2CMessageEventHandler(func(p *dto.WSPayload, _ *dto.WSC2CMessageData) error {
			if id := extractOpenID("c2c", p.RawMessage); id != "" {
				captured <- capture{kind: "c2c", id: id}
			}
			return nil
		}),
		event.GroupATMessageEventHandler(func(p *dto.WSPayload, _ *dto.WSGroupATMessageData) error {
			if id := extractOpenID("group", p.RawMessage); id != "" {
				captured <- capture{kind: "group", id: id}
			}
			return nil
		}),
	)

	api := botgo.NewOpenAPI(appID, tokenSource)
	apInfo, err := api.WS(ctx, nil, "")
	if err != nil {
		log.Fatalf("获取 WS 接入点失败: %v", err)
	}
	// Start 在拉起分片协程后会阻塞，放后台执行
	go func() {
		if err := botgo.NewSessionManager().Start(apInfo, tokenSource, &intent); err != nil {
			log.Printf("WS 会话启动失败: %v", err)
		}
	}()

	log.Println("等待事件……请用你的 QQ 给机器人发一条私聊消息（抓 c2c openid），")
	log.Println("或在群里 @机器人 说句话（抓 group openid）。Ctrl+C 取消。")

	select {
	case c := <-captured:
		fmt.Printf("CAPTURED type=%s id=%s\n", c.kind, c.id)
		os.Exit(0)
	case <-ctx.Done():
		log.Println("已取消")
		os.Exit(1)
	case <-time.After(10 * time.Minute):
		log.Println("10 分钟内未收到消息，超时退出")
		os.Exit(2)
	}
}
