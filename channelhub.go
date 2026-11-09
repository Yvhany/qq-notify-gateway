package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tencent-connect/botgo/token"
)

// channelEntry 缓存中的一条通道客户端及其 token 刷新协程的取消函数。
type channelEntry struct {
	fingerprint string
	qq          *QQClient
	cancel      context.CancelFunc
}

// clientBuilder 构造某通道的 QQClient，并返回用于停止 token 刷新的 cancel。
// 抽成函数类型是为了让测试注入固定 token，避免真的去申请 AccessToken。
type clientBuilder func(ctx context.Context, ch Channel) (*QQClient, context.CancelFunc, error)

// channelHub 每个通道一个独立 QQ 客户端 + 独立 token 刷新协程，按需创建。
// 任一通道的凭证/目标变化只重建它自己，其他通道不受影响。
type channelHub struct {
	mu      sync.Mutex
	entries map[string]*channelEntry
	ctx     context.Context
	timeout time.Duration
	build   clientBuilder
}

// newChannelHub 创建客户端池；ctx 为全部通道刷新协程的父上下文。
func newChannelHub(ctx context.Context, timeout time.Duration) *channelHub {
	h := &channelHub{
		entries: make(map[string]*channelEntry),
		ctx:     ctx,
		timeout: timeout,
	}
	h.build = buildBotgoClient(timeout)
	return h
}

// buildBotgoClient 生产实现：botgo token source + 自动刷新。
func buildBotgoClient(timeout time.Duration) clientBuilder {
	return func(ctx context.Context, ch Channel) (*QQClient, context.CancelFunc, error) {
		cfg := ch.qqConfig()
		src := token.NewQQBotTokenSource(&token.QQBotCredentials{
			AppID:     cfg.AppID,
			AppSecret: cfg.Secret,
		})
		chCtx, cancel := context.WithCancel(ctx)
		if err := token.StartRefreshAccessToken(chCtx, src); err != nil {
			cancel()
			return nil, nil, fmt.Errorf("通道 %s 初始化 AccessToken 失败: %w", ch.ID, err)
		}
		qq := NewQQClient(cfg, src, timeout, newTargetState(cfg.TargetType, cfg.TargetOpenID))
		return qq, cancel, nil
	}
}

// Client 取通道对应的 QQ 客户端；不存在或凭证变化时懒创建/重建。
func (h *channelHub) Client(ch Channel) (*QQClient, error) {
	fp := ch.fingerprint()

	h.mu.Lock()
	if e, ok := h.entries[ch.ID]; ok {
		if e.fingerprint == fp {
			qq := e.qq
			h.mu.Unlock()
			return qq, nil
		}
		// 凭证变了：先停旧的
		e.cancel()
		delete(h.entries, ch.ID)
	}
	build := h.build
	parent := h.ctx
	h.mu.Unlock()

	// 构建可能发起网络调用，放在锁外，避免阻塞其他通道
	qq, cancel, err := build(parent, ch)
	if err != nil {
		return nil, err
	}

	h.mu.Lock()
	if old, ok := h.entries[ch.ID]; ok { // 并发下别人先建好了
		if old.fingerprint == fp {
			h.mu.Unlock()
			cancel()
			return old.qq, nil
		}
		old.cancel()
	}
	h.entries[ch.ID] = &channelEntry{fingerprint: fp, qq: qq, cancel: cancel}
	h.mu.Unlock()
	return qq, nil
}

// Drop 丢弃通道客户端（通道删除后调用）。
func (h *channelHub) Drop(id string) {
	h.mu.Lock()
	if e, ok := h.entries[id]; ok {
		e.cancel()
		delete(h.entries, id)
	}
	h.mu.Unlock()
}

// Close 停止全部通道的 token 刷新协程。
func (h *channelHub) Close() {
	h.mu.Lock()
	for id, e := range h.entries {
		e.cancel()
		delete(h.entries, id)
	}
	h.mu.Unlock()
}

// Size 当前缓存的客户端数量（测试用）。
func (h *channelHub) Size() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.entries)
}
