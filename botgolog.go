package main

import (
	"fmt"
	"log"

	botgolog "github.com/tencent-connect/botgo/log"
)

// quietBotgoLogger 接管 botgo SDK 的日志输出。
//
// 背景：botgo 在 Debug 级别会直接打印 getNewToken 的请求体与响应体，
// 其中包含 AppSecret 与 AccessToken；而本项目的日志会写入 data/logs/gateway.log
// 并在 Web UI「系统日志」页实时展示，等于把机器人凭证摊开给任何能打开 UI 的人。
// 因此这里丢弃 Debug/Info（均为调试信息），只把 Warn/Error 转进我们自己的日志。
type quietBotgoLogger struct{}

func (quietBotgoLogger) Debug(...any)                {}
func (quietBotgoLogger) Debugf(string, ...any)       {}
func (quietBotgoLogger) Info(...any)                 {}
func (quietBotgoLogger) Infof(string, ...any)        {}
func (quietBotgoLogger) Sync() error                 { return nil }
func (l quietBotgoLogger) Warn(v ...any)             { log.Print("botgo: " + fmt.Sprint(v...)) }
func (l quietBotgoLogger) Error(v ...any)            { log.Print("botgo: " + fmt.Sprint(v...)) }
func (l quietBotgoLogger) Warnf(f string, v ...any)  { l.Warn(fmt.Sprintf(f, v...)) }
func (l quietBotgoLogger) Errorf(f string, v ...any) { l.Error(fmt.Sprintf(f, v...)) }

// installQuietBotgoLogger 关闭 SDK 的调试日志（含凭证），必须在创建 token source 前调用。
func installQuietBotgoLogger() {
	botgolog.DefaultLogger = quietBotgoLogger{}
}
