package cos

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultDownloadTTL 是 SignedURL 未显式给过期时间时采用的默认有效期。
const defaultDownloadTTL = 15 * time.Minute

// PutObject 以 PUT 上传对象到 COS，成功返回对象 URL。
// 参与签名的 header 取 host 与 content-type（COS 要求 host 必须参与签名）。
// 非 2xx 返回带响应体片段的错误。
func (c *Client) PutObject(ctx context.Context, key, contentType string, r io.Reader) (string, error) {
	if c == nil {
		return "", fmt.Errorf("cos: client is nil")
	}
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("cos: object key is required")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	u := c.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, r)
	if err != nil {
		return "", fmt.Errorf("cos: build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)

	// 参与签名的 header：host 必填，content-type 一并纳入以提升一致性。
	signHeaders := map[string]string{
		"host":         req.URL.Host,
		"content-type": contentType,
	}
	now := time.Now()
	kt := keyTime(now, defaultSignWindow)
	auth := buildAuthorization(http.MethodPut, req.URL.Path, nil, signHeaders, c.cfg.SecretID, c.cfg.SecretKey, kt)
	req.Header.Set("Authorization", auth)

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("cos: put object: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12)) // 4KiB 片段足够定位错误
		return "", fmt.Errorf("cos: put object HTTP %d: %s", resp.StatusCode, string(body))
	}
	return u, nil
}

// SignedURL 生成一个临时下载 URL（GET），有效期 expiry；<=0 时用默认 15 分钟。
func (c *Client) SignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	if c == nil {
		return "", fmt.Errorf("cos: client is nil")
	}
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("cos: object key is required")
	}
	if expiry <= 0 {
		expiry = defaultDownloadTTL
	}
	// ctx 暂只用于占位/将来取消；纯签名不发请求。
	_ = ctx
	return signedURL(http.MethodGet, c.baseURL, key, nil, c.cfg.SecretID, c.cfg.SecretKey, expiry, time.Now()), nil
}
