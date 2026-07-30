package handler

// asset.go 提供 boss 侧图片上传 handler：接收 multipart 文件，经 COS 客户端
// 上传后返回可访问 URL。COS 未配置（handler 构造时注入 nil）时 Upload 返回
// ErrInternal 且 message 含 "COS 未配置"，保持 nil-safe 不 panic。
// 路由由主线程注册，本文件只实现方法。

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/pkg/cos"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
)

// maxUploadBytes 是单次上传允许的最大文件体积（10MB）。
const maxUploadBytes = 10 << 20

// AssetHandler 处理静态资源（图片）上传。
type AssetHandler struct {
	cos *cos.Client
}

// NewAssetHandler 构造 AssetHandler。cos 可为 nil（未配置时 Upload 报错）。
func NewAssetHandler(c *cos.Client) *AssetHandler {
	return &AssetHandler{cos: c}
}

// Upload 处理 POST 图片上传：multipart 字段名 "file"。
// 成功返回 {"url": ...}。
func (h *AssetHandler) Upload(c *gin.Context) {
	if h.cos == nil {
		httpx.FailError(c, fmt.Errorf("%w: COS 未配置", errcode.ErrInternal))
		return
	}

	file, err := c.FormFile("file")
	if err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	if file.Size > maxUploadBytes {
		httpx.Fail(c, errcode.ErrParam)
		return
	}

	src, err := file.Open()
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	defer src.Close()

	contentType := file.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	key := buildObjectKey(file.Filename)
	url, err := h.cos.PutObject(c.Request.Context(), key, contentType, src)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, gin.H{"url": url})
}

// buildObjectKey 生成对象 key：uploads/<yyyy>/<mm>/<unixnano>-<清理后原名>。
// 原名只保留字母数字 . _ -，其余替换为 '-'，并做路径清洗，避免目录穿越。
func buildObjectKey(orig string) string {
	now := time.Now()
	base := filepath.Base(orig)
	base = sanitizeFilename(base)
	if base == "" || base == "." || base == "-" {
		base = "file"
	}
	return fmt.Sprintf("uploads/%04d/%02d/%d-%s",
		now.Year(), int(now.Month()), now.UnixNano(), base)
}

// sanitizeFilename 保留安全字符，其余替换为 '-'。
func sanitizeFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
