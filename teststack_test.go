package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// testStack 一套完整网关装配：通道配置 + 客户端池 + 记录 + UI + 路由。
type testStack struct {
	cfg      Config
	store    *RecordStore
	ui       *webUI
	channels *channelStore
	clients  *channelHub
	notifier *notifier
	handler  http.Handler
	dir      string
	mock     *httptest.Server
}

// newTestStack 用一条 default 的 generic 通道搭起全套件；
// QQ 出站由 qqHandler 承接，token 用固定值注入，不真的申请 AccessToken。
func newTestStack(t *testing.T, targetType string, qqHandler http.HandlerFunc) *testStack {
	t.Helper()
	mock := httptest.NewServer(qqHandler)
	t.Cleanup(mock.Close)

	cfg := Config{
		AppID:        "test-app-id",
		Secret:       "test-secret",
		TargetType:   targetType,
		TargetOpenID: "OPENID_TEST",
		ListenAddr:   ":0",
		APIBase:      mock.URL,
	}
	dir := t.TempDir()
	channels, err := LoadChannels(dir, cfg, "")
	if err != nil {
		t.Fatal(err)
	}

	clients := newChannelHub(context.Background(), 5*time.Second)
	clients.build = func(_ context.Context, ch Channel) (*QQClient, context.CancelFunc, error) {
		c := ch.qqConfig()
		return NewQQClient(c, staticTokenSource{}, 5*time.Second,
			newTargetState(c.TargetType, c.TargetOpenID)), func() {}, nil
	}
	t.Cleanup(clients.Close)

	store, err := NewRecordStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	listener := newOpenIDListener(hub)
	ui := newWebUI(cfg, store, hub, dir, channels, clients, listener, newRetentionState(0))
	n := newNotifier(channels, clients, store)

	return &testStack{
		cfg:      cfg,
		store:    store,
		ui:       ui,
		channels: channels,
		clients:  clients,
		notifier: n,
		handler:  newMux(n, ui),
		dir:      dir,
		mock:     mock,
	}
}

// setToken 设置指定通道的入站 token（空字符串用默认通道）。
func (s *testStack) setToken(t *testing.T, channelID, tok string) {
	t.Helper()
	if channelID == "" {
		channelID = s.channels.Default()
	}
	if _, err := s.channels.SetToken(channelID, tok); err != nil {
		t.Fatal(err)
	}
}

// addFnOSChannel 追加一条飞牛通道，返回其 ID。
func (s *testStack) addFnOSChannel(t *testing.T, id, signingSecret string, tolerance int) string {
	t.Helper()
	_, err := s.channels.Create(Channel{
		ID:                    id,
		Name:                  "飞牛 NAS",
		Type:                  ChannelTypeFnOS,
		Enabled:               true,
		SigningSecret:         signingSecret,
		TimestampToleranceSec: tolerance,
		QQ: ChannelQQ{
			AppID:        "fnos-app-id",
			AppSecret:    "fnos-secret",
			TargetType:   "c2c",
			TargetOpenID: "OPENID_FNOS",
			APIBase:      s.mock.URL,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
