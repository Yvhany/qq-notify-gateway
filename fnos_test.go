package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 黄金样本：按真实抓包的报文结构固化（密钥、设备名已脱敏，签名值按脱敏后的
// 报文重算）。签名方案就是靠这组结构反推出来的，改动签名算法会立刻在这里失败。
const (
	goldenSecret    = "demo-signing-secret"
	goldenTimestamp = "1794192150"
	goldenSignature = "sha256=0zncyGWbOsxUhpsU+v+peM5FPI4TkKDi8oHI2Mqs3cU="

	// 紧凑 JSON（无空格），字段顺序与飞牛实际发送一致。
	goldenBody = `{"eventId":"webhook-test-1794192139319","providerType":"","providerId":"","eventType":"","occurredAtMs":0,"level":"normal","title":"webhook-测试推送","body":"设备名称：demo-nas\n触发告警：webhook-测试推送\n告警详情：这是一条测试消息。\n触发时间：2026-11-09 10:42:19 +08:00","data":null}`
)

// TestFnOSGoldenSignature 黄金用例：必须与真实抓包的签名逐字节一致。
func TestFnOSGoldenSignature(t *testing.T) {
	if n := len([]byte(goldenBody)); n != 323 {
		t.Fatalf("黄金样本字节数应为 323（等于抓包 content-length），实际 %d —— "+
			"说明紧凑 JSON 形态被改动，签名必然对不上", n)
	}
	got := fnosSignature(goldenSecret, goldenTimestamp, []byte(goldenBody))
	if got != goldenSignature {
		t.Fatalf("签名不匹配黄金样本\n期望: %s\n实际: %s", goldenSignature, got)
	}
}

// TestVerifyFnOSSignature 校验规则：拼接方式、密钥、时间戳容差。
func TestVerifyFnOSSignature(t *testing.T) {
	body := []byte(goldenBody)
	now := time.Unix(1794192150, 0)

	if err := verifyFnOSSignature(goldenSecret, goldenTimestamp, goldenSignature, body, 300, now); err != nil {
		t.Fatalf("合法签名应通过: %v", err)
	}

	// 签名对象是 时间戳 + "\n" + body：不带换行符的构造必须被拒绝
	mac := hmac.New(sha256.New, []byte(goldenSecret))
	mac.Write([]byte(goldenTimestamp))
	mac.Write(body)
	withoutNewline := fnosSignaturePrefix + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if err := verifyFnOSSignature(goldenSecret, goldenTimestamp, withoutNewline, body, 300, now); err == nil {
		t.Error("时间戳与 body 之间没有换行符的签名不应通过")
	}

	t.Run("密钥错误", func(t *testing.T) {
		if err := verifyFnOSSignature("wrong-secret", goldenTimestamp, goldenSignature, body, 300, now); err == nil {
			t.Error("错误密钥应失败")
		}
	})
	t.Run("内容被篡改", func(t *testing.T) {
		tampered := []byte(strings.Replace(goldenBody, "测试推送", "测试推送!", 1))
		if err := verifyFnOSSignature(goldenSecret, goldenTimestamp, goldenSignature, tampered, 300, now); err == nil {
			t.Error("改动一个字节应失败")
		}
	})
	t.Run("时间戳被改动", func(t *testing.T) {
		if err := verifyFnOSSignature(goldenSecret, "1794192151", goldenSignature, body, 300, now); err == nil {
			t.Error("时间戳参与签名，改动后应失败")
		}
	})
	t.Run("缺时间戳头", func(t *testing.T) {
		if err := verifyFnOSSignature(goldenSecret, "", goldenSignature, body, 300, now); err == nil {
			t.Error("缺少时间戳应失败")
		}
	})
	t.Run("缺签名头", func(t *testing.T) {
		if err := verifyFnOSSignature(goldenSecret, goldenTimestamp, "", body, 300, now); err == nil {
			t.Error("缺少签名应失败")
		}
	})
	t.Run("前缀错误", func(t *testing.T) {
		bad := strings.TrimPrefix(goldenSignature, fnosSignaturePrefix)
		if err := verifyFnOSSignature(goldenSecret, goldenTimestamp, bad, body, 300, now); err == nil {
			t.Error("不以 sha256= 开头应失败")
		}
	})
	t.Run("时间戳过期", func(t *testing.T) {
		later := now.Add(301 * time.Second)
		if err := verifyFnOSSignature(goldenSecret, goldenTimestamp, goldenSignature, body, 300, later); err == nil {
			t.Error("超出容差应失败（防重放）")
		}
		// 容差为 0 表示不校验时间
		if err := verifyFnOSSignature(goldenSecret, goldenTimestamp, goldenSignature, body, 0, later); err != nil {
			t.Errorf("容差 0 时应跳过时间校验: %v", err)
		}
	})
}

// TestFnOSPayloadMessage 只原样转发 title + body，不解读 level。
func TestFnOSPayloadMessage(t *testing.T) {
	cases := []struct {
		name string
		p    fnosPayload
		want string
	}{
		{"标题+正文", fnosPayload{Title: "告警", Body: "详情"}, "告警\n详情"},
		{"仅标题", fnosPayload{Title: "告警", Body: "  "}, "告警"},
		{"仅正文", fnosPayload{Body: "详情"}, "详情"},
		{"都空", fnosPayload{}, ""},
	}
	for _, tc := range cases {
		if got := tc.p.message(); got != tc.want {
			t.Errorf("%s: 期望 %q，得到 %q", tc.name, tc.want, got)
		}
	}
}

// TestEventDedup 占位/完成/释放三态的行为与容量上限。
func TestEventDedup(t *testing.T) {
	d := newEventDedup(3)

	res, finish := d.Claim("a")
	if res != claimOK || finish == nil {
		t.Fatal("首次应占位成功")
	}
	if r, _ := d.Claim("a"); r != claimInFlight {
		t.Error("投递中的同一 eventId 应返回 claimInFlight（并发撞车）")
	}
	finish(true)
	if d.Len() != 1 || d.Inflight() != 0 {
		t.Errorf("完成登记后应是 done=1 inflight=0，实际 done=%d inflight=%d", d.Len(), d.Inflight())
	}
	if r, _ := d.Claim("a"); r != claimDone {
		t.Error("已完成的 eventId 应返回 claimDone")
	}

	// 投递失败：释放占位，必须允许重试真正重新投递
	res, finish = d.Claim("b")
	if res != claimOK {
		t.Fatal("b 应占位成功")
	}
	finish(false)
	if d.Len() != 1 {
		t.Errorf("失败不应登记为完成，实际 done=%d", d.Len())
	}
	if d.Inflight() != 0 {
		t.Errorf("失败后 inflight 应清空，实际 %d", d.Inflight())
	}
	retryRes, retryFinish := d.Claim("b")
	if retryRes != claimOK {
		t.Fatal("失败后必须允许重试占位，否则告警会永久丢失")
	}
	retryFinish(true)

	// 空 eventId 无法判重，一律放行
	if r, fin := d.Claim(""); r != claimOK || fin == nil {
		t.Error("空 eventId 应放行")
	}

	// 容量上限：超出后最旧的被淘汰，可以重新进来
	for _, id := range []string{"c", "d"} {
		r, fin := d.Claim(id)
		if r != claimOK {
			t.Fatalf("%s 应占位成功", id)
		}
		fin(true)
	}
	if d.Len() != 3 {
		t.Fatalf("容量应为 3，实际 %d", d.Len())
	}
	r, fin := d.Claim("e")
	if r != claimOK {
		t.Fatal("e 应占位成功")
	}
	fin(true)
	if d.Len() != 3 {
		t.Errorf("超容量后仍应为 3，实际 %d", d.Len())
	}
	if r, _ := d.Claim("a"); r != claimOK {
		t.Error("a 已被淘汰，应可重新占位")
	}
}

// fnosReq 构造一个带正确签名的飞牛请求。
func fnosReq(path, secret string, body []byte, ts string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	if ts != "" && secret != "" {
		r.Header.Set(fnosTSHeader, ts)
		r.Header.Set(fnosSigHeader, fnosSignature(secret, ts, body))
	}
	return r
}

// TestFnOSChannelEndToEnd 端到端：签名通过 → 推送 QQ → 记录带通道归属。
func TestFnOSChannelEndToEnd(t *testing.T) {
	var got map[string]any
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/users/OPENID_FNOS/messages" {
			t.Errorf("应使用飞牛通道自己的目标，实际路径 %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"m1"}`))
	})
	env.addFnOSChannel(t, "fnos", goldenSecret, 300)

	// 用当前时间签名，避免时间戳容差把请求判过期
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	code, resp := doJSON(t, env.handler, fnosReq("/notify/fnos", goldenSecret, []byte(goldenBody), ts))
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d: %v", code, resp)
	}
	if got["content"] != "webhook-测试推送\n设备名称：demo-nas\n触发告警：webhook-测试推送\n"+
		"告警详情：这是一条测试消息。\n触发时间：2026-11-09 10:42:19 +08:00" {
		t.Errorf("推送内容错误: %q", got["content"])
	}

	total, items := env.store.ListByChannel("fnos", 0, 10)
	if total != 1 || items[0].Status != "ok" || items[0].Channel != "fnos" {
		t.Errorf("记录应归属 fnos 通道且成功: total=%d items=%+v", total, items)
	}
}

// TestFnOSSignatureRejected 验签失败/缺头/过期 → 401，且不发起 QQ 调用、不入库。
func TestFnOSSignatureRejected(t *testing.T) {
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		t.Error("验签失败时不应调用 QQ")
	})
	env.addFnOSChannel(t, "fnos", goldenSecret, 300)
	body := []byte(goldenBody)
	fresh := strconv.FormatInt(time.Now().Unix(), 10)

	cases := []struct {
		name string
		req  *http.Request
	}{
		{"缺签名头", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/notify/fnos", strings.NewReader(goldenBody))
			r.Header.Set(fnosTSHeader, fresh)
			return r
		}()},
		{"密钥不对", fnosReq("/notify/fnos", "other-secret", body, fresh)},
		{"时间戳过期", fnosReq("/notify/fnos", goldenSecret, body, "1000000000")},
		{"空签名", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/notify/fnos", strings.NewReader(goldenBody))
			r.Header.Set(fnosTSHeader, fresh)
			r.Header.Set(fnosSigHeader, "")
			return r
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := doJSON(t, env.handler, tc.req)
			if code != http.StatusUnauthorized {
				t.Errorf("期望 401，得到 %d", code)
			}
		})
	}
	if total, _ := env.store.ListByChannel("fnos", 0, 10); total != 0 {
		t.Errorf("验签失败不应入库，实际 %d 条", total)
	}
}

// TestFnOSRetryAfterFailureDelivers 回归：投递失败后，飞牛的重试必须真正重新投递。
// 若把 eventId 在发送前就登记为完成，重试会被当成重复请求吞掉，告警永久丢失。
func TestFnOSRetryAfterFailureDelivers(t *testing.T) {
	var calls int
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":500,"message":"boom"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"m1"}`))
	})
	env.addFnOSChannel(t, "fnos", goldenSecret, 300)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	code1, _ := doJSON(t, env.handler, fnosReq("/notify/fnos", goldenSecret, []byte(goldenBody), ts))
	if code1 != http.StatusBadGateway {
		t.Fatalf("首次投递失败应回 502（让飞牛重试），得到 %d", code1)
	}
	code2, resp2 := doJSON(t, env.handler, fnosReq("/notify/fnos", goldenSecret, []byte(goldenBody), ts))
	if code2 != http.StatusOK {
		t.Fatalf("重试应真正重新投递并成功，得到 %d: %v", code2, resp2)
	}
	if resp2["duplicated"] == true {
		t.Error("失败后的重试不应被判定为重复，否则告警永久丢失")
	}
	if calls != 2 {
		t.Errorf("QQ 应被调用 2 次（失败一次 + 重试一次），实际 %d", calls)
	}
	if total, items := env.store.ListByChannel("fnos", 0, 10); total != 2 || items[0].Status != "ok" {
		t.Errorf("应留下 1 条失败 + 1 条成功记录: total=%d", total)
	}
}

// TestFnOSDedupPerChannel 回归：去重窗口必须按通道隔离，
// A 通道的 eventId 不能把 B 通道的同名事件吞掉。
func TestFnOSDedupPerChannel(t *testing.T) {
	var calls int
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"m1"}`))
	})
	env.addFnOSChannel(t, "fnos-a", "", 0)
	env.addFnOSChannel(t, "fnos-b", "", 0)

	codeA, _ := doJSON(t, env.handler, fnosReq("/notify/fnos-a", "", []byte(goldenBody), ""))
	if codeA != http.StatusOK {
		t.Fatalf("A 通道推送失败: %d", codeA)
	}
	codeB, respB := doJSON(t, env.handler, fnosReq("/notify/fnos-b", "", []byte(goldenBody), ""))
	if codeB != http.StatusOK || respB["duplicated"] == true {
		t.Errorf("B 通道的同名 eventId 不应被 A 吞掉: code=%d resp=%v", codeB, respB)
	}
	if calls != 2 {
		t.Errorf("两条通道各应投递 1 次，实际 QQ 被调用 %d 次", calls)
	}
}

// TestFnOSInFlightReturns503 并发撞车时必须回 503 而不是 200：
// 若先到的那次最终失败，回 200 就等于告诉飞牛「成功了」而实际没发出去。
func TestFnOSInFlightReturns503(t *testing.T) {
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		t.Error("正在投递中时不应发起新的 QQ 调用")
	})
	env.addFnOSChannel(t, "fnos", "", 0)

	// 手动占位，模拟「同一 eventId 正在投递中」
	const eid = "inflight-1"
	res, finish := env.notifier.dedup.For("fnos").Claim(eid)
	if res != claimOK {
		t.Fatal("测试占位应成功")
	}
	defer finish(false)

	body := []byte(`{"eventId":"` + eid + `","title":"并发撞车","body":"详情"}`)
	code, resp := doJSON(t, env.handler, fnosReq("/notify/fnos", "", body, ""))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("投递中应回 503，得到 %d: %v", code, resp)
	}
}

// TestFnOSNoSecretSkipsVerification 未配置签名密钥时不校验（首次接入用）。
func TestFnOSNoSecretSkipsVerification(t *testing.T) {
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"m1"}`))
	})
	env.addFnOSChannel(t, "fnos-open", "", 0)

	req := httptest.NewRequest(http.MethodPost, "/notify/fnos-open", strings.NewReader(goldenBody))
	code, resp := doJSON(t, env.handler, req)
	if code != http.StatusOK {
		t.Fatalf("未配密钥时应放行，得到 %d: %v", code, resp)
	}
}

// TestFnOSIdempotent 同一 eventId 重复推送只发一次。
func TestFnOSIdempotent(t *testing.T) {
	var calls int
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"m1"}`))
	})
	env.addFnOSChannel(t, "fnos", goldenSecret, 300)
	fresh := strconv.FormatInt(time.Now().Unix(), 10)

	if code, _ := doJSON(t, env.handler, fnosReq("/notify/fnos", goldenSecret, []byte(goldenBody), fresh)); code != 200 {
		t.Fatalf("首次推送应 200，得到 %d", code)
	}
	code, resp := doJSON(t, env.handler, fnosReq("/notify/fnos", goldenSecret, []byte(goldenBody), fresh))
	if code != http.StatusOK {
		t.Errorf("重复推送应返回 200（幂等），得到 %d", code)
	}
	if resp["duplicated"] != true {
		t.Errorf("应标记 duplicated=true，得到 %v", resp)
	}
	if calls != 1 {
		t.Errorf("重复 eventId 只应推送 1 次，实际 %d 次", calls)
	}
}
