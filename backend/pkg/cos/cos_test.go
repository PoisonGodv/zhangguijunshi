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
