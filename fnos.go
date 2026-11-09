package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 飞牛（fnOS）Webhook 签名约定（由真实抓包样本反推并验证）：
//
//	x-sac-timestamp: <unix 秒>
//	x-sac-signature: sha256=<base64(HMAC-SHA256(密钥, 时间戳+"\n"+原始请求体))>
//
// 请求体是紧凑 JSON（无空格），字段顺序固定。签名对象是**原始字节**，
// 因此必须在校验前保留原始 body，不能先 Unmarshal 再 Marshal。
const (
	fnosSignaturePrefix = "sha256="
	fnosTSHeader        = "X-Sac-Timestamp"
	fnosSigHeader       = "X-Sac-Signature"
	fnosDedupCapacity   = 500
)

// fnosPayload 飞牛 Webhook 的固定请求体结构。
type fnosPayload struct {
	EventID      string          `json:"eventId"`
	ProviderType string          `json:"providerType"`
	ProviderID   string          `json:"providerId"`
	EventType    string          `json:"eventType"`
	OccurredAtMs int64           `json:"occurredAtMs"`
	Level        string          `json:"level"`
	Title        string          `json:"title"`
	Body         string          `json:"body"`
	Data         json.RawMessage `json:"data"`
}

// message 把飞牛字段拼成要推送的文本：标题 + 空行分隔的正文。
// level/eventType/data 本期只透传不解读（用户要求原样转发 title + body）。
func (p fnosPayload) message() string {
	title := strings.TrimSpace(p.Title)
	body := strings.TrimSpace(p.Body)
	switch {
	case title != "" && body != "":
		return title + "\n" + body
	case title != "":
		return title
	default:
		return body
	}
}

// fnosSignature 计算期望签名值（含 "sha256=" 前缀）。
func fnosSignature(secret, timestamp string, rawBody []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("\n"))
	mac.Write(rawBody)
	return fnosSignaturePrefix + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// verifyFnOSSignature 校验请求头签名与时间戳。
// toleranceSec <= 0 时只校验签名、不校验时间。
func verifyFnOSSignature(secret, timestamp, gotSig string, rawBody []byte, toleranceSec int, now time.Time) error {
	timestamp = strings.TrimSpace(timestamp)
	gotSig = strings.TrimSpace(gotSig)
	if timestamp == "" {
		return fmt.Errorf("缺少 %s 请求头", fnosTSHeader)
	}
	if gotSig == "" {
		return fmt.Errorf("缺少 %s 请求头", fnosSigHeader)
	}
	if !strings.HasPrefix(gotSig, fnosSignaturePrefix) {
		return fmt.Errorf("签名格式错误：应以 %q 开头", fnosSignaturePrefix)
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("时间戳不是合法整数: %q", timestamp)
	}
	if toleranceSec > 0 {
		skew := now.Unix() - ts
		if skew < 0 {
			skew = -skew
		}
		if skew > int64(toleranceSec) {
			return fmt.Errorf("时间戳超出容差（偏差 %d 秒 > %d 秒）", skew, toleranceSec)
		}
	}
	want := fnosSignature(secret, timestamp, rawBody)
	if !hmac.Equal([]byte(want), []byte(gotSig)) {
		return fmt.Errorf("签名校验失败")
	}
	return nil
}

// eventDedup 单条通道的幂等窗口。
//
// 分两种状态，这是本类型的关键：
//   - inflight：正在投递，用于挡住并发重复请求；
//   - done：已投递成功，用于挡住飞牛的重试。
//
// 必须先 Claim（占位）→ 投递 → 成功才 finish(true) 登记完成；失败则 finish(false)
// 释放占位。若在投递前就登记完成，投递失败后飞牛的重试会被当成重复请求吞掉，
// 告警就永久丢失了。
type eventDedup struct {
	mu       sync.Mutex
	cap      int
	order    []string // done 的登记顺序，用于按容量淘汰
	done     map[string]struct{}
	inflight map[string]struct{}
}

func newEventDedup(capacity int) *eventDedup {
	if capacity <= 0 {
		capacity = fnosDedupCapacity
	}
	return &eventDedup{
		cap:      capacity,
		done:     make(map[string]struct{}, capacity),
		inflight: make(map[string]struct{}),
	}
}

// claimResult Claim 的三种结果。
type claimResult int

const (
	claimOK       claimResult = iota // 占位成功，可以投递
	claimInFlight                    // 同一 eventId 正在投递中
	claimDone                        // 同一 eventId 已投递成功
)

// Claim 尝试占用 eventID。
//   - claimOK：占位成功，调用方必须在处理结束后调用 finish(ok)；
//   - claimInFlight：同一事件正在投递中，调用方应回 503 让对方稍后重试
//     （不能回 200，否则并发撞车时若先到的那次最终失败，事件就丢了）；
//   - claimDone：已投递成功，直接回幂等响应。
//
// 空 eventID 无法判重，一律放行并返回空操作。
func (d *eventDedup) Claim(eventID string) (claimResult, func(ok bool)) {
	if eventID == "" {
		return claimOK, func(bool) {}
	}
	d.mu.Lock()
	if _, ok := d.done[eventID]; ok {
		d.mu.Unlock()
		return claimDone, nil
	}
	if _, ok := d.inflight[eventID]; ok {
		d.mu.Unlock()
		return claimInFlight, nil
	}
	d.inflight[eventID] = struct{}{}
	d.mu.Unlock()

	return claimOK, func(ok bool) {
		d.mu.Lock()
		defer d.mu.Unlock()
		delete(d.inflight, eventID)
		if !ok {
			return // 投递失败：释放占位，允许重试真正重新投递
		}
		if _, exist := d.done[eventID]; exist {
			return
		}
		d.done[eventID] = struct{}{}
		d.order = append(d.order, eventID)
		if len(d.order) > d.cap {
			drop := d.order[0]
			d.order = d.order[1:]
			delete(d.done, drop)
		}
	}
}

// Len 当前已登记完成的数量（测试用）。
func (d *eventDedup) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.order)
}

// Inflight 当前正在投递的数量（测试用）。
func (d *eventDedup) Inflight() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.inflight)
}

// dedupSet 按通道隔离的去重窗口集合。
// 去重必须 per-channel：两条通道各自的 eventId 空间互不相干，
// 共用一份会让 A 通道的事件把 B 通道的同名事件吞掉。
type dedupSet struct {
	mu  sync.Mutex
	cap int
	m   map[string]*eventDedup
}

func newDedupSet(capacity int) *dedupSet {
	return &dedupSet{cap: capacity, m: make(map[string]*eventDedup)}
}

// For 取指定通道的去重窗口（不存在则创建）。
func (s *dedupSet) For(channelID string) *eventDedup {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.m[channelID]; ok {
		return d
	}
	d := newEventDedup(s.cap)
	s.m[channelID] = d
	return d
}
