package cos

import (
	"net/http"
	"time"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// Client 是一个无状态的腾讯云 COS 客户端，纯 net/http + HMAC-SHA1 签名。
// 默认 baseURL 形如 https://<bucket>.cos.<region>.myqcloud.com；配置里显式给
// Endpoint 时优先用之（可用于自定义域名 / 私有部署 / 测试假对象存储）。
type Client struct {
	cfg     config.COSConfig
	hc      *http.Client
	baseURL string
}

// New 构造 Client。默认 http.Client 超时 30s。
func New(cfg config.COSConfig) *Client {
	return &Client{
		cfg:     cfg,
		hc:      &http.Client{Timeout: 30 * time.Second},
		baseURL: resolveBaseURL(cfg),
	}
}

// resolveBaseURL 计算 baseURL：Endpoint 非空直接用，否则按标准 COS 域名拼接。
func resolveBaseURL(cfg config.COSConfig) string {
	if cfg.Endpoint != "" {
		return cfg.Endpoint
	}
	return "https://" + cfg.Bucket + ".cos." + cfg.Region + ".myqcloud.com"
}

// WithHTTPClient 注入自定义 *http.Client（便于测试控制 transport/超时）。
// 返回自身以支持链式。
func (c *Client) WithHTTPClient(hc *http.Client) *Client {
	if hc != nil {
		c.hc = hc
	}
	return c
}

// WithBaseURL 覆盖 baseURL（测试指向 httptest 假对象存储）。返回自身。
func (c *Client) WithBaseURL(u string) *Client {
	if u != "" {
		c.baseURL = u
	}
	return c
}

// objectURL 拼出对象的完整 URL（key 前补 leading slash，逐段百分号编码）。
// 返回编码后的 URL，既作为请求 URL（http 客户端据此生成与之一致的 RequestURI），
// 也作为 PutObject 的返回值交给调用方直接使用，避免含空格/中文等字符的 key 产出
// 不可直接请求的裸路径。
func (c *Client) objectURL(key string) string {
	base := c.baseURL
	// 去掉末尾斜杠再拼接，避免双斜杠。
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	return base + escapedObjectPath(key)
}
