package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 通道类型：generic = 原有 {title,content,image} 契约；fnos = 飞牛（fnOS）Webhook 契约。
const (
	ChannelTypeGeneric = "generic"
	ChannelTypeFnOS    = "fnos"

	channelFileVersion    = 1
	defaultChannelID      = "default"
	defaultFnOSTolerance  = 300 // 秒
	maxChannelNameRunes   = 32
	maxChannelSecretChars = 512
)

var channelIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ChannelQQ 单个通道使用的 QQ 机器人凭证与推送目标。
// 与全局 Config 解耦：每个通道可以绑定自己的机器人。
type ChannelQQ struct {
	AppID        string `json:"app_id"`
	AppSecret    string `json:"app_secret"`
	TargetType   string `json:"target_type"`   // c2c | group
	TargetOpenID string `json:"target_openid"` // 单聊 user_openid 或群 group_openid
	Sandbox      bool   `json:"sandbox,omitempty"`
	APIBase      string `json:"api_base,omitempty"` // 空 = 按 sandbox 取默认域名
}

// Channel 一条推送通道：独立入站地址 + 独立鉴权 + 独立机器人 + 独立目标。
type Channel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"` // generic | fnos
	Enabled       bool   `json:"enabled"`
	Token         string `json:"token,omitempty"`          // generic 入站校验 token
	SigningSecret string `json:"signing_secret,omitempty"` // fnos HMAC 签名密钥
	// TimestampToleranceSec 仅 fnos 生效：时间戳容差秒数，0 表示不校验时间
	TimestampToleranceSec int       `json:"timestamp_tolerance_sec,omitempty"`
	QQ                    ChannelQQ `json:"qq"`
	CreatedAt             string    `json:"created_at,omitempty"`
}

// qqConfig 转成 QQClient 需要的 Config 视图（只用到 AppID/APIBase 等出站字段）。
func (c Channel) qqConfig() Config {
	base := strings.TrimRight(strings.TrimSpace(c.QQ.APIBase), "/")
	if base == "" {
		if c.QQ.Sandbox {
			base = "https://sandbox.api.sgroup.qq.com"
		} else {
			base = "https://api.sgroup.qq.com"
		}
	}
	return Config{
		AppID:        c.QQ.AppID,
		Secret:       c.QQ.AppSecret,
		TargetType:   c.QQ.TargetType,
		TargetOpenID: c.QQ.TargetOpenID,
		APIBase:      base,
		Sandbox:      c.QQ.Sandbox,
	}
}

// fingerprint 用于判断两个通道是否需要重建 QQ 客户端（凭证或目标变化才重建）。
func (c Channel) fingerprint() string {
	return strings.Join([]string{
		c.QQ.AppID, c.QQ.AppSecret, c.QQ.TargetType, c.QQ.TargetOpenID,
		fmt.Sprintf("%t", c.QQ.Sandbox), c.qqConfig().APIBase,
	}, "\x00")
}

// masked 返回打码副本：密钥类字段只保留首尾各 2 位（过短则全星号）。
func (c Channel) masked() Channel {
	m := c
	m.Token = maskSecret(c.Token)
	m.SigningSecret = maskSecret(c.SigningSecret)
	m.QQ.AppSecret = maskSecret(c.QQ.AppSecret)
	return m
}

// maskSecret 密钥打码：<=4 位全星号，否则首2+****+尾2。
func maskSecret(v string) string {
	if v == "" {
		return ""
	}
	r := []rune(v)
	if len(r) <= 4 {
		return "****"
	}
	return string(r[:2]) + "****" + string(r[len(r)-2:])
}

// HasSecret 判断通道是否配置了对应密钥（打码后仍可据此渲染 UI）。
func (c Channel) hasToken() bool         { return strings.TrimSpace(c.Token) != "" }
func (c Channel) hasSigningSecret() bool { return strings.TrimSpace(c.SigningSecret) != "" }

// normalize 校验并规范化通道字段（就地修改），失败返回错误。
func (c *Channel) normalize() error {
	c.ID = strings.TrimSpace(c.ID)
	c.Name = strings.TrimSpace(c.Name)
	c.Type = strings.ToLower(strings.TrimSpace(c.Type))
	c.Token = strings.TrimSpace(c.Token)
	c.SigningSecret = strings.TrimSpace(c.SigningSecret)
	c.QQ.AppID = strings.TrimSpace(c.QQ.AppID)
	c.QQ.AppSecret = strings.TrimSpace(c.QQ.AppSecret)
	c.QQ.TargetType = strings.ToLower(strings.TrimSpace(c.QQ.TargetType))
	c.QQ.TargetOpenID = strings.TrimSpace(c.QQ.TargetOpenID)
	c.QQ.APIBase = strings.TrimRight(strings.TrimSpace(c.QQ.APIBase), "/")

	if !channelIDPattern.MatchString(c.ID) {
		return fmt.Errorf("通道 ID 只能用 1-32 位小写字母、数字、下划线或短横线，且以字母或数字开头")
	}
	if c.Name == "" {
		c.Name = c.ID
	}
	if len([]rune(c.Name)) > maxChannelNameRunes {
		return fmt.Errorf("通道名称不能超过 %d 个字", maxChannelNameRunes)
	}
	switch c.Type {
	case "":
		c.Type = ChannelTypeGeneric
	case ChannelTypeGeneric, ChannelTypeFnOS:
	default:
		return fmt.Errorf("通道类型只能是 generic 或 fnos")
	}
	if c.QQ.AppID == "" {
		return fmt.Errorf("缺少 QQ AppID")
	}
	if c.QQ.AppSecret == "" {
		return fmt.Errorf("缺少 QQ AppSecret")
	}
	if c.QQ.TargetType == "" {
		c.QQ.TargetType = "c2c"
	}
	if c.QQ.TargetType != "c2c" && c.QQ.TargetType != "group" {
		return fmt.Errorf("推送目标类型只能是 c2c 或 group")
	}
	if !validOpenID(c.QQ.TargetOpenID) {
		return fmt.Errorf("推送目标 OpenID 格式无效")
	}
	if len(c.Token) > maxChannelSecretChars || len(c.SigningSecret) > maxChannelSecretChars {
		return fmt.Errorf("密钥长度不能超过 %d 字符", maxChannelSecretChars)
	}
	if c.Type == ChannelTypeFnOS {
		if c.TimestampToleranceSec < 0 || c.TimestampToleranceSec > 86400 {
			return fmt.Errorf("时间戳容差需在 0-86400 秒之间")
		}
		// 注意：0 表示「不校验时间」，由调用方显式表达，这里不做任何改写。
		// 新建通道时的默认值（300 秒）在 handleChannelCreate 里补，避免吞掉用户显式的 0。
	} else {
		c.SigningSecret = ""
		c.TimestampToleranceSec = 0
	}
	if c.CreatedAt == "" {
		c.CreatedAt = time.Now().Format(time.RFC3339)
	}
	return nil
}

// channelFile channels.json 的磁盘结构。
type channelFile struct {
	Version        int       `json:"version"`
	DefaultChannel string    `json:"default_channel"`
	Channels       []Channel `json:"channels"`
}

// channelStore 通道配置的唯一真源，落盘 DATA_DIR/channels.json（0600）。
type channelStore struct {
	mu   sync.RWMutex
	path string
	f    channelFile
}

// LoadChannels 载入通道配置；文件不存在时用旧的 .env 配置迁移出 default 通道。
// 文件存在但损坏时返回错误（静默降级会让多通道悄悄变成单通道，风险高于停机）。
func LoadChannels(dataDir string, legacy Config, legacyToken string) (*channelStore, error) {
	path := filepath.Join(dataDir, "channels.json")
	s := &channelStore{path: path}

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("读取通道配置失败: %w", err)
		}
		persisted := loadStateFrom(dataDir)
		f, merr := migrateChannelFile(legacy, legacyToken, persisted)
		if merr != nil {
			return nil, merr
		}
		s.f = f
		if err := s.saveLocked(); err != nil {
			return nil, fmt.Errorf("写入初始通道配置失败: %w", err)
		}
		return s, nil
	}
	var f channelFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("解析通道配置 %s 失败: %w", path, err)
	}
	if len(f.Channels) == 0 {
		return nil, fmt.Errorf("通道配置 %s 中没有任何通道", path)
	}
	seen := make(map[string]struct{}, len(f.Channels))
	for i := range f.Channels {
		if err := f.Channels[i].normalize(); err != nil {
			return nil, fmt.Errorf("通道配置 %s 第 %d 条无效: %w", path, i+1, err)
		}
		if _, dup := seen[f.Channels[i].ID]; dup {
			return nil, fmt.Errorf("通道配置 %s 中通道 ID 重复: %s", path, f.Channels[i].ID)
		}
		seen[f.Channels[i].ID] = struct{}{}
	}
	if f.DefaultChannel == "" || !containsChannel(f.Channels, f.DefaultChannel) {
		f.DefaultChannel = f.Channels[0].ID
	}
	s.f = f
	return s, nil
}

// migrateChannelFile 从旧的单通道持久化状态生成 default 通道。
//
// 迁移必须**枚举全部旧来源**，否则会静默改掉用户已有的配置：
//   - 推送目标：webui.json 里的值（用户可在 UI 运行期修改）优先于 .env 初值，
//     这与单通道时代 main 的启动逻辑一致；该值非法时回落 .env（旧代码也是记警告后
//     沿用 .env 初值）；
//   - 入站 token：同理，webui.json 优先于 .env 的 GATEWAY_TOKEN。
//
// 产物必须过一遍 normalize：否则首启能写出非法的 channels.json，第二次启动直接
// Fatalf，表现为「升级后第一天正常、重启就起不来」。
func migrateChannelFile(legacy Config, legacyToken string, persisted stateFile) (channelFile, error) {
	apiBase := ""
	if legacy.APIBase != "" && legacy.APIBase != "https://api.sgroup.qq.com" {
		apiBase = legacy.APIBase
	}
	token := strings.TrimSpace(persisted.GatewayToken)
	if token == "" {
		token = strings.TrimSpace(legacyToken)
	}

	build := func(targetType, targetOpenID string) (Channel, error) {
		ch := Channel{
			ID:        defaultChannelID,
			Name:      "默认通道",
			Type:      ChannelTypeGeneric,
			Enabled:   true,
			Token:     token,
			CreatedAt: time.Now().Format(time.RFC3339),
			QQ: ChannelQQ{
				AppID:        legacy.AppID,
				AppSecret:    legacy.Secret,
				TargetType:   targetType,
				TargetOpenID: targetOpenID,
				Sandbox:      legacy.Sandbox,
				APIBase:      apiBase,
			},
		}
		if err := ch.normalize(); err != nil {
			return Channel{}, err
		}
		return ch, nil
	}

	if id := strings.TrimSpace(persisted.TargetOpenID); id != "" {
		typ := strings.TrimSpace(persisted.TargetType)
		if typ == "" {
			typ = legacy.TargetType
		}
		if ch, err := build(typ, id); err == nil {
			return channelFile{Version: channelFileVersion, DefaultChannel: ch.ID, Channels: []Channel{ch}}, nil
		}
		// 持久化目标不合法：退回 .env 初值，而不是让服务起不来
	}
	ch, err := build(legacy.TargetType, legacy.TargetOpenID)
	if err != nil {
		return channelFile{}, fmt.Errorf("旧配置无法迁移为通道（请检查 .env 与 webui.json 中的凭证/目标）: %w", err)
	}
	return channelFile{Version: channelFileVersion, DefaultChannel: ch.ID, Channels: []Channel{ch}}, nil
}

func containsChannel(list []Channel, id string) bool {
	for _, c := range list {
		if c.ID == id {
			return true
		}
	}
	return false
}

// saveLocked 原子落盘（调用方需持有写锁或独占）。
func (s *channelStore) saveLocked() error {
	data, err := json.MarshalIndent(s.f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Default 返回默认通道 ID。
func (s *channelStore) Default() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.f.DefaultChannel
}

// Get 按 ID 取通道（返回副本，调用方可安全使用）。
func (s *channelStore) Get(id string) (Channel, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.f.Channels {
		if c.ID == id {
			return c, true
		}
	}
	return Channel{}, false
}

// List 返回全部通道副本（保持配置顺序）。
func (s *channelStore) List() []Channel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Channel, len(s.f.Channels))
	copy(out, s.f.Channels)
	return out
}

// IDs 返回全部通道 ID。
func (s *channelStore) IDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.f.Channels))
	for i, c := range s.f.Channels {
		out[i] = c.ID
	}
	return out
}

// Create 新增通道。
func (s *channelStore) Create(ch Channel) (Channel, error) {
	if err := ch.normalize(); err != nil {
		return Channel{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if containsChannel(s.f.Channels, ch.ID) {
		return Channel{}, fmt.Errorf("通道 ID 已存在: %s", ch.ID)
	}
	prev := s.f
	s.f.Channels = append(s.f.Channels, ch)
	if err := s.saveLocked(); err != nil {
		s.f = prev
		return Channel{}, err
	}
	return ch, nil
}

// Update 覆盖指定通道（ID 不可改）。
func (s *channelStore) Update(id string, ch Channel) (Channel, error) {
	ch.ID = id
	if err := ch.normalize(); err != nil {
		return Channel{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i := range s.f.Channels {
		if s.f.Channels[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Channel{}, fmt.Errorf("通道不存在: %s", id)
	}
	prev := s.f
	ch.CreatedAt = s.f.Channels[idx].CreatedAt
	if ch.CreatedAt == "" {
		ch.CreatedAt = time.Now().Format(time.RFC3339)
	}
	s.f.Channels[idx] = ch
	if err := s.saveLocked(); err != nil {
		s.f = prev
		return Channel{}, err
	}
	return ch, nil
}

// Delete 删除通道；默认通道不允许删除，且至少要保留一条。
func (s *channelStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f.DefaultChannel == id {
		return fmt.Errorf("默认通道不允许删除，请先把其他通道设为默认")
	}
	idx := -1
	for i := range s.f.Channels {
		if s.f.Channels[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("通道不存在: %s", id)
	}
	if len(s.f.Channels) == 1 {
		return fmt.Errorf("至少要保留一个通道")
	}
	prev := s.f
	kept := make([]Channel, 0, len(s.f.Channels)-1)
	kept = append(kept, s.f.Channels[:idx]...)
	kept = append(kept, s.f.Channels[idx+1:]...)
	s.f.Channels = kept
	if err := s.saveLocked(); err != nil {
		s.f = prev
		return err
	}
	return nil
}

// SetDefault 切换默认通道（决定 POST /notify 落到哪条通道）。
func (s *channelStore) SetDefault(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !containsChannel(s.f.Channels, id) {
		return fmt.Errorf("通道不存在: %s", id)
	}
	prev := s.f
	s.f.DefaultChannel = id
	if err := s.saveLocked(); err != nil {
		s.f = prev
		return err
	}
	return nil
}

// SetToken 重置指定通道的入站 token。
func (s *channelStore) SetToken(id, token string) (Channel, error) {
	ch, ok := s.Get(id)
	if !ok {
		return Channel{}, fmt.Errorf("通道不存在: %s", id)
	}
	ch.Token = token
	return s.Update(id, ch)
}

// SetTarget 修改指定通道的推送目标。
func (s *channelStore) SetTarget(id, typ, openID string) (Channel, error) {
	ch, ok := s.Get(id)
	if !ok {
		return Channel{}, fmt.Errorf("通道不存在: %s", id)
	}
	ch.QQ.TargetType = typ
	ch.QQ.TargetOpenID = openID
	return s.Update(id, ch)
}
