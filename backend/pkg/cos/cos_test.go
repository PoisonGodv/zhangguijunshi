package cos

// cos_test.go verifies the pure COS HMAC-SHA1 signing + the PutObject /
// SignedURL client behaviour against an httptest fake object store. Tests live
// in package cos so they can call the unexported signing helpers to
// self-consistently recompute (and thus validate) the Authorization signature.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// parseAuth pulls the q-sign-* fields out of a COS Authorization header.
func parseAuth(t *testing.T, header string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, part := range strings.Split(header, "&") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 {
			v, _ := url.QueryUnescape(kv[1])
			out[kv[0]] = v
		}
	}
	return out
}

func TestPutObject_SignsAndReturnsURL(t *testing.T) {
	cfg := config.COSConfig{SecretID: "AKIDtest", SecretKey: "secrettest"}

	var (
		gotMethod  string
		gotPath    string
		gotCType   string
		gotAuth    string
		gotHostHdr string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCType = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		gotHostHdr = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(cfg).WithBaseURL(srv.URL)
	body := bytes.NewReader([]byte("hello-cos"))
	u, err := c.PutObject(context.Background(), "uploads/2026/07/img.png", "image/png", body)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if u != srv.URL+"/uploads/2026/07/img.png" {
		t.Errorf("url = %q, want %s/uploads/2026/07/img.png", u, srv.URL)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/uploads/2026/07/img.png" {
		t.Errorf("path = %q", gotPath)
	}
	if gotCType != "image/png" {
		t.Errorf("content-type = %q", gotCType)
	}

	// Authorization must be a well-formed q-sign block.
	a := parseAuth(t, gotAuth)
	if a["q-sign-algorithm"] != "sha1" {
		t.Errorf("q-sign-algorithm = %q", a["q-sign-algorithm"])
	}
	if a["q-ak"] != "AKIDtest" {
		t.Errorf("q-ak = %q", a["q-ak"])
	}
	if a["q-signature"] == "" {
		t.Error("q-signature empty")
	}
	if a["q-header-list"] == "" {
		t.Error("q-header-list empty (host must participate)")
	}

	// Self-consistent round trip: recompute the signature with the exact
	// key-time the client used and the same canonical headers, then compare.
	kt := a["q-sign-time"]
	headers := map[string]string{"host": gotHostHdr, "content-type": gotCType}
	wantSig, _, _ := signature(http.MethodPut, gotPath, nil, headers, cfg.SecretKey, kt)
	if wantSig != a["q-signature"] {
		t.Errorf("signature mismatch:\n got  %q\n want %q", a["q-signature"], wantSig)
	}
}

// TestPutObject_SpecialCharKey_SignsEscapedPath 是签名路径 bug 的回归测试：
// 旧实现用解码后的 req.URL.Path 计算签名，而 http 客户端实际发送的是百分号编码
// 的 RequestURI，导致含空格/中文等字符的 key 签名覆盖路径与线上路径不一致而校验
// 失败。这里让假对象存储按「线上真实路径」(r.URL.EscapedPath()) 复算签名来模拟
// COS 服务端的校验口径。
func TestPutObject_SpecialCharKey_SignsEscapedPath(t *testing.T) {
	cfg := config.COSConfig{SecretID: "AKIDtest", SecretKey: "secrettest"}

	keys := []string{
		"uploads/2026/07/a b.png", // 空格
		"uploads/2026/07/中文.png",  // 中文
		"uploads/2026/07/a&b.png", // &
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			var (
				gotRequestURI string
				gotEscaped    string
				gotAuth       string
				gotHostHdr    string
				gotCType      string
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotRequestURI = r.RequestURI
				gotEscaped = r.URL.EscapedPath()
				gotAuth = r.Header.Get("Authorization")
				gotHostHdr = r.Host
				gotCType = r.Header.Get("Content-Type")
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			c := New(cfg).WithBaseURL(srv.URL)
			u, err := c.PutObject(context.Background(), key, "image/png", strings.NewReader("x"))
			if err != nil {
				t.Fatalf("PutObject(%q): %v", key, err)
			}
			wantEscaped := escapedObjectPath(key)
			if gotEscaped != wantEscaped {
				t.Errorf("wire path = %q, want escaped %q", gotEscaped, wantEscaped)
			}
			if !strings.HasPrefix(gotRequestURI, wantEscaped) {
				t.Errorf("RequestURI = %q does not start with escaped path %q", gotRequestURI, wantEscaped)
			}
			if u != srv.URL+wantEscaped {
				t.Errorf("returned url = %q, want %s%s", u, srv.URL, wantEscaped)
			}

			// 签名必须恰好覆盖线上传输的编码路径（COS 服务端校验口径）。
			a := parseAuth(t, gotAuth)
			headers := map[string]string{"host": gotHostHdr, "content-type": gotCType}
			wantSig, _, _ := signature(http.MethodPut, gotEscaped, nil, headers, cfg.SecretKey, a["q-sign-time"])
			if wantSig != a["q-signature"] {
				t.Errorf("signature does not cover escaped path:\n got  %q\n want %q", a["q-signature"], wantSig)
			}
			// 对于编码后与解码后不同的 key，额外确认「按解码路径复算」会不匹配——
			// 证明本用例真能复现原 bug（否则该 key 无法区分新旧实现）。
			if decoded := objectPath(key); decoded != gotEscaped {
				if decSig, _, _ := signature(http.MethodPut, decoded, nil, headers, cfg.SecretKey, a["q-sign-time"]); decSig == a["q-signature"] {
					t.Errorf("signature unexpectedly matches decoded path; case would not catch the bug")
				}
			}
		})
	}
}

func TestPutObject_Non2xxError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<Error>AccessDenied</Error>"))
	}))
	defer srv.Close()

	c := New(config.COSConfig{SecretID: "ak", SecretKey: "sk"}).WithBaseURL(srv.URL)
	_, err := c.PutObject(context.Background(), "k", "text/plain", strings.NewReader("x"))
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention status: %v", err)
	}
}

func TestPutObject_EmptyKeyRejected(t *testing.T) {
	c := New(config.COSConfig{SecretID: "ak", SecretKey: "sk"})
	if _, err := c.PutObject(context.Background(), "  ", "text/plain", strings.NewReader("x")); err == nil {
		t.Fatal("expected error on empty key")
	}
}

func TestSignedURL_SelfConsistent(t *testing.T) {
	cfg := config.COSConfig{SecretID: "AKID", SecretKey: "sk", Region: "ap-guangzhou", Bucket: "mybucket"}
	c := New(cfg)

	raw, err := c.SignedURL(context.Background(), "uploads/2026/07/a.png", 0)
	if err != nil {
		t.Fatalf("SignedURL: %v", err)
	}
	if !strings.HasPrefix(raw, "https://mybucket.cos.ap-guangzhou.myqcloud.com/uploads/2026/07/a.png?") {
		t.Errorf("url base/path wrong: %q", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := u.Query()
	if q.Get("q-sign-algorithm") != "sha1" {
		t.Errorf("q-sign-algorithm = %q", q.Get("q-sign-algorithm"))
	}
	if q.Get("q-ak") != "AKID" {
		t.Errorf("q-ak = %q", q.Get("q-ak"))
	}
	if q.Get("q-signature") == "" {
		t.Error("q-signature empty")
	}
	// Recompute the signature over the object path (empty params/headers) and
	// compare — proves the URL was signed with the stated key-time.
	kt := q.Get("q-sign-time")
	wantSig, _, _ := signature(http.MethodGet, objectPath("uploads/2026/07/a.png"), nil, nil, cfg.SecretKey, kt)
	if wantSig != q.Get("q-signature") {
		t.Errorf("url signature mismatch:\n got  %q\n want %q", q.Get("q-signature"), wantSig)
	}
}

// TestSignedURL_SpecialCharKey 验证临时下载 URL 对含特殊字符的 key：签名覆盖的
// 路径与 URL 里实际拼出的路径一致（都是百分号编码形式），且可被 url.Parse 还原。
func TestSignedURL_SpecialCharKey(t *testing.T) {
	cfg := config.COSConfig{SecretID: "AKID", SecretKey: "sk", Region: "ap-guangzhou", Bucket: "mybucket"}
	c := New(cfg)

	keys := []string{
		"uploads/2026/07/a b.png",
		"uploads/2026/07/中文.png",
		"uploads/2026/07/a&b.png",
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			raw, err := c.SignedURL(context.Background(), key, 0)
			if err != nil {
				t.Fatalf("SignedURL(%q): %v", key, err)
			}
			wantEscaped := escapedObjectPath(key)
			if !strings.HasPrefix(raw, "https://mybucket.cos.ap-guangzhou.myqcloud.com"+wantEscaped+"?") {
				t.Errorf("url path not escaped as expected: %q (want prefix ...%s?)", raw, wantEscaped)
			}
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("parse url: %v", err)
			}
			if u.EscapedPath() != wantEscaped {
				t.Errorf("url EscapedPath = %q, want %q", u.EscapedPath(), wantEscaped)
			}
			// 签名覆盖的路径必须与 URL 实际路径一致。
			q := u.Query()
			wantSig, _, _ := signature(http.MethodGet, u.EscapedPath(), nil, nil, cfg.SecretKey, q.Get("q-sign-time"))
			if wantSig != q.Get("q-signature") {
				t.Errorf("url signature mismatch:\n got  %q\n want %q", q.Get("q-signature"), wantSig)
			}
		})
	}
}

func TestResolveBaseURL(t *testing.T) {
	if got := resolveBaseURL(config.COSConfig{Bucket: "b", Region: "r"}); got != "https://b.cos.r.myqcloud.com" {
		t.Errorf("default base url = %q", got)
	}
	if got := resolveBaseURL(config.COSConfig{Endpoint: "https://custom.example.com", Bucket: "b", Region: "r"}); got != "https://custom.example.com" {
		t.Errorf("endpoint override = %q", got)
	}
}

func TestNilClient(t *testing.T) {
	var c *Client
	if _, err := c.PutObject(context.Background(), "k", "t", strings.NewReader("x")); err == nil {
		t.Error("nil client PutObject should error")
	}
	if _, err := c.SignedURL(context.Background(), "k", 0); err == nil {
		t.Error("nil client SignedURL should error")
	}
}
