package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestComputeDigests(t *testing.T) {
	data := make([]byte, md510mBoundary+100)
	for i := range data {
		data[i] = byte(i % 251)
	}
	dg := computeDigests(data)
	if len(dg.MD5) != 32 || len(dg.SHA1) != 40 || len(dg.MD5_10M) != 32 {
		t.Fatalf("摘要长度错误: %+v", dg)
	}
	// md5_10m 应等于前 10002432 字节的 MD5，而非整文件
	if dg.MD5_10M == dg.MD5 {
		t.Error("文件超过 10MB 时 md5_10m 不应等于整文件 md5")
	}
	// 小文件时 md5_10m 等于整文件 md5
	small := computeDigests([]byte("tiny"))
	if small.MD5_10M != small.MD5 {
		t.Error("小文件 md5_10m 应等于整文件 md5")
	}
}

func TestChunkPlan(t *testing.T) {
	data := make([]byte, 2500)
	parts := []preparePart{
		{Index: 1, PresignedURL: "u1", BlockSize: "1000"},
		{Index: 0, PresignedURL: "u0", BlockSize: "1000"},
		{Index: 2, PresignedURL: "u2", BlockSize: "1000"},
	}
	chunks, err := chunkPlan(data, parts, "1000")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 || len(chunks[0]) != 1000 || len(chunks[1]) != 1000 || len(chunks[2]) != 500 {
		t.Errorf("分片大小错误: %d", len(chunks))
	}
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	if total != len(data) {
		t.Errorf("分片未覆盖全部数据: %d/%d", total, len(data))
	}

	if _, err := chunkPlan(data, nil, "1000"); err == nil {
		t.Error("空分片列表应报错")
	}
	if _, err := chunkPlan(data[:10], parts, "1000"); err == nil {
		t.Error("分片超出数据应报错")
	}
}

// TestUploadImageFlow 在 mock 服务器上完整走通
// prepare → PUT 分片 → part_finish → merge → 返回 file_info。
func TestUploadImageFlow(t *testing.T) {
	const blockSize = "1024"
	data := make([]byte, 3000) // 3 片
	for i := range data {
		data[i] = byte(i)
	}

	var (
		mu        sync.Mutex
		putParts  = map[int][]byte{}
		finished  = map[int]bool{}
		mergeBody map[string]any
	)

	var mock *httptest.Server
	mock = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/presigned":
			idx := r.URL.Query().Get("part")
			body, _ := io.ReadAll(r.Body)
			n := 0
			_, _ = fmt.Sscanf(idx, "%d", &n)
			mu.Lock()
			putParts[n] = body
			mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.URL.Path == "/v2/users/OPENID_TEST/upload_prepare":
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req["file_type"] != float64(1) {
				t.Errorf("file_type 错误: %v", req["file_type"])
			}
			if req["file_size"] != "3000" {
				t.Errorf("file_size 错误: %v", req["file_size"])
			}
			resp := map[string]any{
				"upload_id": "up_1",
				"block_size": blockSize,
				"parts": []map[string]any{
					{"index": 0, "presigned_url": mock.URL + "/presigned?part=0", "block_size": blockSize},
					{"index": 1, "presigned_url": mock.URL + "/presigned?part=1", "block_size": blockSize},
					{"index": 2, "presigned_url": mock.URL + "/presigned?part=2", "block_size": blockSize},
				},
				"upload_config": map[string]any{"concurrency": 2, "retry_delay": 10},
			}
			writeJSON(w, http.StatusOK, resp)

		case r.URL.Path == "/v2/users/OPENID_TEST/upload_part_finish":
			var req struct {
				UploadID  string `json:"upload_id"`
				PartIndex int    `json:"part_index"`
				BlockSize string `json:"block_size"`
				MD5       string `json:"md5"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.UploadID != "up_1" || req.MD5 == "" || req.BlockSize == "" {
				t.Errorf("part_finish 参数错误: %+v", req)
			}
			mu.Lock()
			finished[req.PartIndex] = true
			mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{})

		case r.URL.Path == "/v2/users/OPENID_TEST/files":
			_ = json.NewDecoder(r.Body).Decode(&mergeBody)
			writeJSON(w, http.StatusOK, map[string]any{
				"file_uuid": "fu", "file_info": "FI_BASE64_TOKEN", "ttl": 600,
			})

		default:
			t.Errorf("未预期的请求: %s %s", r.Method, r.URL.Path)
			writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "message": "not found"})
		}
	}))
	defer mock.Close()

	cfg := Config{
		AppID: "a", Secret: "s", TargetType: "c2c",
		TargetOpenID: "OPENID_TEST", APIBase: mock.URL,
	}
	qq := NewQQClient(cfg, staticTokenSource{}, 5*time.Second)
	fileInfo, err := qq.UploadImage(t.Context(), "notify.jpg", data)
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if fileInfo != "FI_BASE64_TOKEN" {
		t.Errorf("file_info 错误: %q", fileInfo)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(putParts) != 3 {
		t.Errorf("期望 PUT 3 个分片，实际 %d", len(putParts))
	}
	if len(finished) != 3 {
		t.Errorf("期望 3 次 part_finish，实际 %d", len(finished))
	}
	// 分片内容正确拼回原始数据
	if string(putParts[0])+string(putParts[1])+string(putParts[2]) != string(data) {
		t.Error("分片内容与原始数据不一致")
	}
	if mergeBody["upload_id"] != "up_1" || mergeBody["srv_send_msg"] != false {
		t.Errorf("合并请求错误: %v", mergeBody)
	}
}

// TestUploadThenSendImage 上传成功后发送 msg_type 7。
func TestUploadThenSendImage(t *testing.T) {
	var sentMsg map[string]any
	var mock *httptest.Server
	mock = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/users/OPENID_TEST/upload_prepare":
			writeJSON(w, http.StatusOK, map[string]any{
				"upload_id": "up_1", "block_size": "64",
				"parts": []map[string]any{
					{"index": 0, "presigned_url": mock.URL + "/p0", "block_size": "64"},
				},
				"upload_config": map[string]any{"concurrency": 1},
			})
		case "/p0":
			w.WriteHeader(http.StatusOK)
		case "/v2/users/OPENID_TEST/upload_part_finish":
			writeJSON(w, http.StatusOK, map[string]any{})
		case "/v2/users/OPENID_TEST/files":
			writeJSON(w, http.StatusOK, map[string]any{"file_info": "FI_X"})
		case "/v2/users/OPENID_TEST/messages":
			_ = json.NewDecoder(r.Body).Decode(&sentMsg)
			writeJSON(w, http.StatusOK, map[string]any{"id": "m1"})
		default:
			t.Errorf("未预期请求: %s", r.URL.Path)
			writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "message": "nf"})
		}
	}))
	defer mock.Close()

	cfg := Config{AppID: "a", Secret: "s", TargetType: "c2c", TargetOpenID: "OPENID_TEST", APIBase: mock.URL}
	qq := NewQQClient(cfg, staticTokenSource{}, 5*time.Second)
	img := pngMagic(64) // 恰好一个分片
	fi, err := qq.UploadImage(t.Context(), "n.png", img)
	if err != nil {
		t.Fatal(err)
	}
	if err := qq.SendImage(t.Context(), fi); err != nil {
		t.Fatal(err)
	}
	if sentMsg["msg_type"] != float64(7) {
		t.Errorf("msg_type 错误: %v", sentMsg["msg_type"])
	}
	media, _ := sentMsg["media"].(map[string]any)
	if media == nil || media["file_info"] != "FI_X" {
		t.Errorf("media 错误: %v", sentMsg["media"])
	}
}

// TestUploadPrepareHTTPError 非 2xx 应转为 QQAPIError。
func TestUploadPrepareHTTPError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, map[string]any{"code": 11253, "message": "应用无接口访问权限"})
	}))
	defer mock.Close()

	cfg := Config{AppID: "a", Secret: "s", TargetType: "c2c", TargetOpenID: "OPENID_TEST", APIBase: mock.URL}
	qq := NewQQClient(cfg, staticTokenSource{}, 5*time.Second)
	_, err := qq.UploadImage(t.Context(), "n.png", []byte{0x89, 'P', 'N', 'G'})
	if err == nil {
		t.Fatal("期望错误")
	}
	var apiErr *QQAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望 QQAPIError，得到 %T: %v", err, err)
	}
	if apiErr.Code != 11253 {
		t.Errorf("code 错误: %d", apiErr.Code)
	}
}

// TestSendTextBusinessError 200 但 code!=0 也应报错。
func TestSendTextBusinessError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"code": 40034100, "message": "主动消息发送超过频控限制"})
	}))
	defer mock.Close()
	cfg := Config{AppID: "a", Secret: "s", TargetType: "c2c", TargetOpenID: "OPENID_TEST", APIBase: mock.URL}
	qq := NewQQClient(cfg, staticTokenSource{}, 5*time.Second)
	err := qq.SendText(t.Context(), "hello")
	var apiErr *QQAPIError
	if !errors.As(err, &apiErr) || apiErr.Code != 40034100 {
		t.Fatalf("期望频控错误，得到: %v", err)
	}
}

func TestDecodeThenDetect(t *testing.T) {
	img := pngMagic(64)
	enc := base64.StdEncoding.EncodeToString(img)
	got, err := decodeImagePayload(enc)
	if err != nil {
		t.Fatal(err)
	}
	if http.DetectContentType(got) != "image/png" {
		t.Errorf("MIME 识别错误: %s", http.DetectContentType(got))
	}
}
