package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// legacyCfg 模拟单通道时代的 .env 配置。
func legacyCfg() Config {
	return Config{
		AppID:        "legacy-app",
		Secret:       "legacy-secret",
		TargetType:   "c2c",
		TargetOpenID: "LEGACY_OPENID",
		APIBase:      "https://api.sgroup.qq.com",
	}
}

// TestLoadChannelsMigration 无 channels.json 时按 .env 迁移出 default 通道，
// 保证老调用方（三月七小助手）的地址与 token 完全不变。
func TestLoadChannelsMigration(t *testing.T) {
	dir := t.TempDir()
	chs, err := LoadChannels(dir, legacyCfg(), "seed-token-123")
	if err != nil {
		t.Fatal(err)
	}
	if got := chs.Default(); got != defaultChannelID {
		t.Errorf("默认通道应为 default，得到 %s", got)
	}
	ch, ok := chs.Get(defaultChannelID)
	if !ok {
		t.Fatal("应存在 default 通道")
	}
	if ch.Type != ChannelTypeGeneric || !ch.Enabled {
		t.Errorf("迁移出的通道应为启用的 generic：%+v", ch)
	}
	if ch.Token != "seed-token-123" {
		t.Errorf("token 应沿用旧的种子值，得到 %q", ch.Token)
	}
	if ch.QQ.AppID != "legacy-app" || ch.QQ.TargetOpenID != "LEGACY_OPENID" || ch.QQ.TargetType != "c2c" {
		t.Errorf("QQ 凭证/目标迁移错误: %+v", ch.QQ)
	}
	// 文件应已落盘；Unix 上还必须是 0600（内含密钥）。Windows 不支持 POSIX 权限位，跳过。
	path := filepath.Join(dir, "channels.json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("channels.json 应已生成: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("channels.json 权限应为 0600，实际 %o", perm)
		}
	}
	// 再次加载读到的是同一份，不会重复迁移
	chs2, err := LoadChannels(dir, legacyCfg(), "another-token")
	if err != nil {
		t.Fatal(err)
	}
	ch2, _ := chs2.Get(defaultChannelID)
	if ch2.Token != "seed-token-123" {
		t.Errorf("已存在的配置不应被重新迁移覆盖，得到 %q", ch2.Token)
	}
}

// TestLoadChannelsCorrupt 配置文件损坏必须拒绝启动，不能静默降级成单通道。
func TestLoadChannelsCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "channels.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadChannels(dir, legacyCfg(), "tok"); err == nil {
		t.Fatal("损坏的 channels.json 应返回错误")
	}

	// 空通道列表同样拒绝
	if err := os.WriteFile(filepath.Join(dir, "channels.json"), []byte(`{"version":1,"channels":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadChannels(dir, legacyCfg(), "tok"); err == nil {
		t.Fatal("没有任何通道时应返回错误")
	}
}

// TestChannelValidation 字段校验与默认值。
func TestChannelValidation(t *testing.T) {
	base := Channel{
		Name:    "测试",
		Type:    ChannelTypeGeneric,
		Enabled: true,
		QQ: ChannelQQ{
			AppID: "app", AppSecret: "secret",
			TargetType: "c2c", TargetOpenID: "OPENID_1",
		},
	}
	cases := []struct {
		name    string
		mutate  func(*Channel)
		wantErr bool
	}{
		{"合法", func(c *Channel) { c.ID = "ok-id_1" }, false},
		{"ID 大写", func(c *Channel) { c.ID = "Bad" }, true},
		{"ID 空", func(c *Channel) { c.ID = "" }, true},
		{"ID 过长", func(c *Channel) { c.ID = strings.Repeat("a", 33) }, true},
		{"ID 含空格", func(c *Channel) { c.ID = "a b" }, true},
		{"类型非法", func(c *Channel) { c.ID = "a"; c.Type = "http" }, true},
		{"缺 AppID", func(c *Channel) { c.ID = "a"; c.QQ.AppID = "" }, true},
		{"缺 AppSecret", func(c *Channel) { c.ID = "a"; c.QQ.AppSecret = "" }, true},
		{"目标类型非法", func(c *Channel) { c.ID = "a"; c.QQ.TargetType = "guild" }, true},
		{"OpenID 非法", func(c *Channel) { c.ID = "a"; c.QQ.TargetOpenID = "bad id" }, true},
		{"名称为空回落为 ID", func(c *Channel) { c.ID = "zzz"; c.Name = "" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.mutate(&c)
			err := c.normalize()
			if tc.wantErr && err == nil {
				t.Fatalf("应报错，实际通过（%+v）", c)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("不应报错: %v", err)
			}
		})
	}
}

// TestFnOSChannelNormalize 容差语义：0 表示「不校验时间」，normalize 不得改写。
func TestFnOSChannelNormalize(t *testing.T) {
	f := Channel{
		ID: "fnos", Type: ChannelTypeFnOS, SigningSecret: "k",
		QQ: ChannelQQ{AppID: "a", AppSecret: "s", TargetType: "c2c", TargetOpenID: "OID1"},
	}
	if err := f.normalize(); err != nil {
		t.Fatal(err)
	}
	if f.TimestampToleranceSec != 0 {
		t.Errorf("显式留 0 表示不校验时间，normalize 不应改写，实际 %d", f.TimestampToleranceSec)
	}

	g := Channel{
		ID: "g", Type: ChannelTypeGeneric, SigningSecret: "should-be-cleared",
		QQ: ChannelQQ{AppID: "a", AppSecret: "s", TargetType: "c2c", TargetOpenID: "OID1"},
	}
	if err := g.normalize(); err != nil {
		t.Fatal(err)
	}
	if g.SigningSecret != "" || g.TimestampToleranceSec != 0 {
		t.Errorf("generic 通道不应保留签名相关字段: %+v", g)
	}
}

// TestMigrationPrefersPersistedTarget 回归：迁移必须优先用 webui.json 里的目标，
// 否则老部署在 UI 里改过的目标会在升级后被静默改回 .env 初值（发错人）。
func TestMigrationPrefersPersistedTarget(t *testing.T) {
	dir := t.TempDir()
	// 模拟用户曾在 UI 里把目标改成群聊
	if err := saveStateTo(dir, stateFile{
		TargetType:   "group",
		TargetOpenID: "PERSISTED_GROUP_ID",
	}); err != nil {
		t.Fatal(err)
	}
	chs, err := LoadChannels(dir, legacyCfg(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := chs.Get(defaultChannelID)
	if ch.QQ.TargetType != "group" || ch.QQ.TargetOpenID != "PERSISTED_GROUP_ID" {
		t.Errorf("应以 webui.json 的持久化目标为准，实际 %+v", ch.QQ)
	}

	// 没有持久化目标时退回 .env
	dir2 := t.TempDir()
	chs2, err := LoadChannels(dir2, legacyCfg(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	ch2, _ := chs2.Get(defaultChannelID)
	if ch2.QQ.TargetType != "c2c" || ch2.QQ.TargetOpenID != "LEGACY_OPENID" {
		t.Errorf("无持久化目标时应沿用 .env，实际 %+v", ch2.QQ)
	}
}

// TestChannelStoreCRUD 增删改、默认通道约束与持久化。
func TestChannelStoreCRUD(t *testing.T) {
	dir := t.TempDir()
	chs, err := LoadChannels(dir, legacyCfg(), "tok")
	if err != nil {
		t.Fatal(err)
	}

	// 新增
	if _, err := chs.Create(Channel{
		ID: "fnos", Name: "飞牛", Type: ChannelTypeFnOS, Enabled: true,
		SigningSecret: "s1",
		QQ:            ChannelQQ{AppID: "a2", AppSecret: "s2", TargetType: "c2c", TargetOpenID: "OID2"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := chs.Create(Channel{
		ID: "fnos", Name: "重复", QQ: ChannelQQ{AppID: "a", AppSecret: "s", TargetType: "c2c", TargetOpenID: "OID3"},
	}); err == nil {
		t.Error("重复 ID 应报错")
	}

	// 默认通道不允许删除
	if err := chs.Delete(defaultChannelID); err == nil {
		t.Error("默认通道应禁止删除")
	}
	// 切换到 fnos 后，原 default 可删
	if err := chs.SetDefault("fnos"); err != nil {
		t.Fatal(err)
	}
	if err := chs.Delete(defaultChannelID); err != nil {
		t.Fatalf("切换默认后可删除旧通道: %v", err)
	}
	// 只剩一个时不允许再删
	if err := chs.Delete("fnos"); err == nil {
		t.Error("至少要保留一个通道")
	}

	// 持久化：重新加载后内容一致
	reloaded, err := LoadChannels(dir, legacyCfg(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Default(); got != "fnos" {
		t.Errorf("默认通道应持久化为 fnos，得到 %s", got)
	}
	if len(reloaded.List()) != 1 {
		t.Errorf("应剩 1 条通道，实际 %d", len(reloaded.List()))
	}
}

// TestChannelMasking 密钥打码规则。
func TestChannelMasking(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"ab":        "****",
		"abcd":      "****",
		"abcdef":    "ab****ef",
		"abcdefghij": "ab****ij",
	}
	for in, want := range cases {
		if got := maskSecret(in); got != want {
			t.Errorf("maskSecret(%q) = %q，期望 %q", in, got, want)
		}
	}
	ch := Channel{
		Token: "token-abcdef", SigningSecret: "sign-123456",
		QQ: ChannelQQ{AppSecret: "secret-xyz"},
	}
	m := ch.masked()
	if m.Token == ch.Token || m.SigningSecret == ch.SigningSecret || m.QQ.AppSecret == ch.QQ.AppSecret {
		t.Error("打码后不应保留原值")
	}
	// 打码不影响原对象
	if ch.Token != "token-abcdef" {
		t.Error("masked 不应修改原通道")
	}
}

// TestChannelFingerprint 凭证变化才重建客户端。
func TestChannelFingerprint(t *testing.T) {
	a := Channel{ID: "x", QQ: ChannelQQ{AppID: "a1", AppSecret: "s", TargetType: "c2c", TargetOpenID: "OID1"}}
	b := a
	if a.fingerprint() != b.fingerprint() {
		t.Error("相同凭证应得到相同指纹")
	}
	b.QQ.AppSecret = "other"
	if a.fingerprint() == b.fingerprint() {
		t.Error("密钥变化应改变指纹")
	}
	b = a
	b.QQ.TargetOpenID = "OID2"
	if a.fingerprint() == b.fingerprint() {
		t.Error("目标变化应改变指纹")
	}
	b = a
	b.Token = "irrelevant" // 入站 token 不影响出站客户端
	if a.fingerprint() != b.fingerprint() {
		t.Error("入站 token 不应影响出站客户端指纹")
	}
}
