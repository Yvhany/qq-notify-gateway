package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestChannelHubLazyAndRebuild 懒创建、复用、凭证变化后重建。
func TestChannelHubLazyAndRebuild(t *testing.T) {
	builds := 0
	h := newChannelHub(context.Background(), time.Second)
	h.build = func(_ context.Context, ch Channel) (*QQClient, context.CancelFunc, error) {
		builds++
		return &QQClient{}, func() {}, nil
	}
	ch := Channel{ID: "a", QQ: ChannelQQ{AppID: "1", AppSecret: "s", TargetType: "c2c", TargetOpenID: "OID1"}}

	first, err := h.Client(ch)
	if err != nil {
		t.Fatal(err)
	}
	if builds != 1 {
		t.Fatalf("首次应创建 1 次，实际 %d", builds)
	}
	second, _ := h.Client(ch)
	if first != second {
		t.Error("相同凭证应复用同一个客户端")
	}
	if builds != 1 {
		t.Errorf("复用不应重建，实际构建 %d 次", builds)
	}

	ch.QQ.AppSecret = "changed"
	third, _ := h.Client(ch)
	if third == first {
		t.Error("凭证变化应重建客户端")
	}
	if builds != 2 {
		t.Errorf("应重建 1 次（共 2 次），实际 %d", builds)
	}

	// Drop 后再取会重建
	h.Drop("a")
	if h.Size() != 0 {
		t.Errorf("Drop 后缓存应为空，实际 %d", h.Size())
	}
	if _, err := h.Client(ch); err != nil {
		t.Fatal(err)
	}
	if builds != 3 {
		t.Errorf("Drop 后应重建（共 3 次），实际 %d", builds)
	}
	h.Close()
	if h.Size() != 0 {
		t.Error("Close 后缓存应为空")
	}
}

// TestChannelHubIsolation 一个通道构建失败不应影响其他通道。
func TestChannelHubIsolation(t *testing.T) {
	h := newChannelHub(context.Background(), time.Second)
	h.build = func(_ context.Context, ch Channel) (*QQClient, context.CancelFunc, error) {
		if ch.ID == "broken" {
			return nil, nil, errors.New("凭证无效")
		}
		return &QQClient{}, func() {}, nil
	}
	defer h.Close()

	broken := Channel{ID: "broken", QQ: ChannelQQ{AppID: "x", AppSecret: "y", TargetType: "c2c", TargetOpenID: "OIDB"}}
	good := Channel{ID: "good", QQ: ChannelQQ{AppID: "x", AppSecret: "y", TargetType: "c2c", TargetOpenID: "OIDG"}}

	if _, err := h.Client(broken); err == nil {
		t.Fatal("凭证无效的通道应返回错误")
	}
	if _, err := h.Client(good); err != nil {
		t.Fatalf("其他通道不应受影响: %v", err)
	}
	if h.Size() != 1 {
		t.Errorf("失败的通道不应进入缓存，实际缓存 %d", h.Size())
	}
}

// TestMultiChannelRouting /notify 走默认通道，/notify/<id> 走指定通道，
// 未知/停用通道返回 404 且不入库为成功。
func TestMultiChannelRouting(t *testing.T) {
	var paths []string
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"m"}`))
	})
	// 追加第二条通道，目标不同
	if _, err := env.channels.Create(Channel{
		ID: "second", Name: "第二通道", Type: ChannelTypeGeneric, Enabled: true, Token: "tok2",
		QQ: ChannelQQ{AppID: "a2", AppSecret: "s2", TargetType: "c2c", TargetOpenID: "OPENID_SECOND", APIBase: env.mock.URL},
	}); err != nil {
		t.Fatal(err)
	}

	// /notify → 默认通道（OPENID_TEST）
	if code, _ := doJSON(t, env.handler, notifyReq(`{"content":"to-default"}`, "")); code != 200 {
		t.Fatalf("默认通道推送失败: %d", code)
	}
	// /notify/second → 第二通道
	r := notifyReq(`{"content":"to-second"}`, "tok2")
	r.URL.Path = "/notify/second"
	if code, _ := doJSON(t, env.handler, r); code != 200 {
		t.Fatalf("指定通道推送失败: %d", code)
	}
	if len(paths) != 2 {
		t.Fatalf("应调用 QQ 2 次，实际 %d", len(paths))
	}
	if paths[0] != "/v2/users/OPENID_TEST/messages" {
		t.Errorf("默认通道目标错误: %s", paths[0])
	}
	if paths[1] != "/v2/users/OPENID_SECOND/messages" {
		t.Errorf("第二通道目标错误: %s", paths[1])
	}

	// 未知通道 → 404
	r = notifyReq(`{"content":"x"}`, "")
	r.URL.Path = "/notify/nope"
	if code, _ := doJSON(t, env.handler, r); code != http.StatusNotFound {
		t.Errorf("未知通道应 404，得到 %d", code)
	}

	// 停用通道 → 404
	ch, _ := env.channels.Get("second")
	ch.Enabled = false
	if _, err := env.channels.Update("second", ch); err != nil {
		t.Fatal(err)
	}
	r = notifyReq(`{"content":"x"}`, "tok2")
	r.URL.Path = "/notify/second"
	if code, _ := doJSON(t, env.handler, r); code != http.StatusNotFound {
		t.Errorf("停用通道应 404，得到 %d", code)
	}
	if len(paths) != 2 {
		t.Errorf("404 场景不应调用 QQ，实际总调用 %d", len(paths))
	}
}

// TestRecordsAndStatsPerChannel 记录与统计按通道隔离。
func TestRecordsAndStatsPerChannel(t *testing.T) {
	env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"m"}`))
	})
	if _, err := env.channels.Create(Channel{
		ID: "fnos", Name: "飞牛", Type: ChannelTypeFnOS, Enabled: true,
		QQ: ChannelQQ{AppID: "a2", AppSecret: "s2", TargetType: "c2c", TargetOpenID: "OPENID_FNOS", APIBase: env.mock.URL},
	}); err != nil {
		t.Fatal(err)
	}

	// default 一条、fnos 两条
	doJSON(t, env.handler, notifyReq(`{"content":"默认通道消息"}`, ""))
	for _, txt := range []string{"飞牛告警一", "飞牛告警二"} {
		body := `{"eventId":"evt-` + txt + `","title":"` + txt + `","body":"详情"}`
		r := notifyAndDo(t, env, "/notify/fnos", body)
		if r != 200 {
			t.Fatalf("飞牛推送失败: %d", r)
		}
	}

	totalAll, _ := env.store.ListByChannel("", 0, 50)
	if totalAll != 3 {
		t.Fatalf("全部记录应为 3，实际 %d", totalAll)
	}
	if totalFnos, items := env.store.ListByChannel("fnos", 0, 50); totalFnos != 2 {
		t.Errorf("fnos 通道应为 2 条，实际 %d", totalFnos)
	} else {
		for _, it := range items {
			if it.Channel != "fnos" {
				t.Errorf("记录通道归属错误: %+v", it)
			}
		}
	}
	if totalDefault, _ := env.store.ListByChannel(defaultChannelID, 0, 50); totalDefault != 1 {
		t.Errorf("default 通道应为 1 条，实际 %d", totalDefault)
	}

	// 统计按通道过滤
	if st := env.store.StatsByChannel("fnos"); st.TotalCount != 2 {
		t.Errorf("fnos 统计应为 2，实际 %d", st.TotalCount)
	}
	if st := env.store.StatsByChannel(""); st.TotalCount != 3 {
		t.Errorf("全部统计应为 3，实际 %d", st.TotalCount)
	}

	// HTTP 接口过滤
	rec := httptestGet(t, env, "/api/records?channel=fnos")
	if !strings.Contains(rec, `"total":2`) {
		t.Errorf("/api/records?channel=fnos 应返回 total=2: %s", rec)
	}
}

// TestChannelCreateFnOSDefaultTolerance 新建飞牛通道的容差语义：
// 未显式提供时补默认 300 秒；显式给 0 表示「不校验时间」，必须保留。
func TestChannelCreateFnOSDefaultTolerance(t *testing.T) {
	cases := []struct {
		name string
		id   string
		body string
		want int
	}{
		{
			"未给容差", "f1",
			`{"id":"f1","name":"a","type":"fnos","signing_secret":"k","qq":{"app_id":"x","app_secret":"y","target_type":"c2c","target_openid":"OID1"}}`,
			300,
		},
		{
			"显式 0", "f2",
			`{"id":"f2","name":"b","type":"fnos","signing_secret":"k","timestamp_tolerance_sec":0,"qq":{"app_id":"x","app_secret":"y","target_type":"c2c","target_openid":"OID2"}}`,
			0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "c2c", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			})
			r := httptest.NewRequest(http.MethodPost, "/api/channels", strings.NewReader(tc.body))
			code, resp := doJSON(t, env.handler, r)
			if code != http.StatusOK {
				t.Fatalf("创建失败 %d: %v", code, resp)
			}
			ch, ok := env.channels.Get(tc.id)
			if !ok {
				t.Fatalf("通道 %s 未创建", tc.id)
			}
			if ch.TimestampToleranceSec != tc.want {
				t.Errorf("容差应为 %d，实际 %d", tc.want, ch.TimestampToleranceSec)
			}
		})
	}
}

// notifyAndDo 发一条入站请求到指定路径，返回状态码。
func notifyAndDo(t *testing.T, env *testEnv, path, body string) int {
	t.Helper()
	r := notifyReq(body, "")
	r.URL.Path = path
	code, _ := doJSON(t, env.handler, r)
	return code
}

// httptestGet 发一个 GET 并返回响应体文本。
func httptestGet(t *testing.T, env *testEnv, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Body.String()
}
