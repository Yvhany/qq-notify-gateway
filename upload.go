package main

import (
	"bytes"
	"context"
	"crypto/md5"  //nolint:gosec // QQ 接口要求 MD5 校验值
	"crypto/sha1" //nolint:gosec // QQ 接口要求 SHA1 校验值
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxImageBytes 官方图片软限制 20MB。
var maxImageBytes = 20 << 20

const (
	md510mBoundary = 10002432 // 官方 md5_10m 取前 10002432 字节
	uploadWorkers  = 4        // 分片并发上限
	uploadRetries  = 3
)

type preparePart struct {
	Index        int    `json:"index"`
	PresignedURL string `json:"presigned_url"`
	BlockSize    string `json:"block_size"`
}

type prepareResp struct {
	UploadID   string `json:"upload_id"`
	BlockSize  string `json:"block_size"`
	Parts      []preparePart
	UploadConfig struct {
		Concurrency int `json:"concurrency"`
		RetryDelay  int `json:"retry_delay"`
	} `json:"upload_config"`
}

// digestSet 文件级校验值。
type digestSet struct {
	MD5     string
	SHA1    string
	MD5_10M string
}

func computeDigests(data []byte) digestSet {
	md5Sum := md5.Sum(data)    //nolint:gosec
	sha1Sum := sha1.Sum(data)  //nolint:gosec
	head := data
	if len(head) > md510mBoundary {
		head = head[:md510mBoundary]
	}
	headMD5 := md5.Sum(head) //nolint:gosec
	return digestSet{
		MD5:     hex.EncodeToString(md5Sum[:]),
		SHA1:    hex.EncodeToString(sha1Sum[:]),
		MD5_10M: hex.EncodeToString(headMD5[:]),
	}
}

// chunkPlan 按 prepare 返回的分片信息切分数据。
// block_size 若大于剩余数据则取剩余（末片）。
func chunkPlan(data []byte, parts []preparePart, defaultBlockSize string) ([][]byte, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("预上传未返回任何分片")
	}
	sorted := append([]preparePart(nil), parts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Index < sorted[j].Index })
	chunks := make([][]byte, 0, len(sorted))
	offset := 0
	for _, p := range sorted {
		if offset >= len(data) {
			return nil, fmt.Errorf("分片计划超出数据长度")
		}
		sizeStr := p.BlockSize
		if sizeStr == "" {
			sizeStr = defaultBlockSize
		}
		size, err := strconv.Atoi(sizeStr)
		if err != nil || size <= 0 {
			return nil, fmt.Errorf("分片 %d 的 block_size 无效: %q", p.Index, sizeStr)
		}
		end := offset + size
		if end > len(data) {
			end = len(data)
		}
		chunks = append(chunks, data[offset:end])
		offset = end
	}
	if offset != len(data) {
		return nil, fmt.Errorf("分片计划未覆盖全部数据: %d/%d", offset, len(data))
	}
	return chunks, nil
}

// UploadImage 分片上传图片到当前目标（单聊或群聊），返回 file_info。
func (c *QQClient) UploadImage(ctx context.Context, filename string, data []byte) (string, error) {
	if len(data) > maxImageBytes {
		return "", fmt.Errorf("图片 %d 字节超过上限 %d", len(data), maxImageBytes)
	}
	if len(data) == 0 {
		return "", fmt.Errorf("图片数据为空")
	}
	dg := computeDigests(data)
	prepBody := map[string]any{
		"file_type": 1,
		"file_size": strconv.Itoa(len(data)),
		"file_name": filename,
		"md5":       dg.MD5,
		"sha1":      dg.SHA1,
		"md5_10m":   dg.MD5_10M,
	}
	var prep prepareResp
	if err := c.postJSON(ctx, c.targetPrefix()+"/upload_prepare", prepBody, &prep); err != nil {
		return "", fmt.Errorf("upload_prepare 失败: %w", err)
	}
	if prep.UploadID == "" {
		return "", fmt.Errorf("upload_prepare 未返回 upload_id")
	}

	chunks, err := chunkPlan(data, prep.Parts, prep.BlockSize)
	if err != nil {
		return "", err
	}

	concurrency := prep.UploadConfig.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > uploadWorkers {
		concurrency = uploadWorkers
	}
	retryDelay := time.Duration(prep.UploadConfig.RetryDelay) * time.Millisecond
	if prep.UploadConfig.RetryDelay <= 0 || prep.UploadConfig.RetryDelay > 10000 {
		retryDelay = 500 * time.Millisecond
	}

	// partFinish 依赖 index，用与 sorted 一致的顺序记录
	sorted := append([]preparePart(nil), prep.Parts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Index < sorted[j].Index })

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	sem := make(chan struct{}, concurrency)
	for i, p := range sorted {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, part preparePart, chunk []byte) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := c.uploadOnePart(ctx, prep.UploadID, part, chunk, retryDelay); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("分片 %d 失败: %w", part.Index, err)
				}
				mu.Unlock()
			}
		}(i, p, chunks[i])
	}
	wg.Wait()
	if firstErr != nil {
		return "", firstErr
	}

	var merged struct {
		FileInfo string `json:"file_info"`
		TTL      uint   `json:"ttl"`
	}
	mergeBody := map[string]any{
		"file_type":    1,
		"file_name":    filename,
		"upload_id":    prep.UploadID,
		"srv_send_msg": false,
	}
	if err := c.postJSON(ctx, c.targetPrefix()+"/files", mergeBody, &merged); err != nil {
		return "", fmt.Errorf("分片合并失败: %w", err)
	}
	if merged.FileInfo == "" {
		return "", fmt.Errorf("合并响应缺少 file_info")
	}
	return merged.FileInfo, nil
}

// uploadOnePart PUT 单个分片到预签名 URL，成功后调用 upload_part_finish。
func (c *QQClient) uploadOnePart(ctx context.Context, uploadID string, part preparePart, chunk []byte, retryDelay time.Duration) error {
	var lastErr error
	for attempt := 0; attempt < uploadRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 {
			select {
			case <-time.After(retryDelay * time.Duration(attempt)):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := c.putPresigned(ctx, part.PresignedURL, chunk); err != nil {
			lastErr = err
			continue
		}
		chunkMD5 := md5.Sum(chunk) //nolint:gosec
		finishBody := map[string]any{
			"upload_id":  uploadID,
			"part_index": part.Index,
			"block_size": strconv.Itoa(len(chunk)),
			"md5":        hex.EncodeToString(chunkMD5[:]),
		}
		if err := c.postJSON(ctx, c.targetPrefix()+"/upload_part_finish", finishBody, nil); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

// putPresigned 向预签名 URL PUT 分片（不带 QQ 鉴权头）。
func (c *QQClient) putPresigned(ctx context.Context, url string, chunk []byte) error {
	if url == "" {
		return fmt.Errorf("预签名 URL 为空")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(chunk))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(chunk))
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("PUT 分片失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("PUT 分片返回 %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// decodeImagePayload 解码入站 base64 图片，容忍 data URI 前缀。
func decodeImagePayload(raw string) ([]byte, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	if strings.HasPrefix(strings.ToLower(s), "data:") {
		if i := strings.IndexByte(s, ','); i >= 0 {
			s = s[i+1:]
		}
	}
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("图片 base64 解码失败: %w", err)
	}
	return data, nil
}
