package wxsdk

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetAccessToken_Success(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		q := r.URL.Query()
		if q.Get("grant_type") != "client_credential" || q.Get("appid") != "APP" || q.Get("secret") != "SEC" {
			t.Errorf("unexpected query: %v", q)
		}
		_, _ = w.Write([]byte(`{"access_token":"tok-1","expires_in":7200}`))
	}))
	defer srv.Close()

	c := NewTokenCache(WithTokenBaseURL(srv.URL))
	tok, err := c.GetAccessToken(context.Background(), "APP", "SEC")
	if err != nil {
		t.Fatalf("GetAccessToken: %v", err)
	}
	if tok != "tok-1" {
		t.Fatalf("token mismatch: %q", tok)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected 1 upstream hit, got %d", got)
	}
}

func TestGetAccessToken_CacheHit(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"access_token":"tok-cached","expires_in":7200}`))
	}))
	defer srv.Close()

	c := NewTokenCache(WithTokenBaseURL(srv.URL))
	for i := 0; i < 3; i++ {
		tok, err := c.GetAccessToken(context.Background(), "APP", "SEC")
		if err != nil {
			t.Fatalf("GetAccessToken #%d: %v", i, err)
		}
		if tok != "tok-cached" {
			t.Fatalf("token mismatch #%d: %q", i, tok)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected cache to serve repeats (1 upstream hit), got %d", got)
	}
}

func TestGetAccessToken_ExpiryRefresh(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		// expires_in=7200 but the skew (5min) is applied; advancing the clock
		// past the skew-adjusted expiry forces a refetch.
		_, _ = w.Write([]byte(`{"access_token":"tok-` + string(rune('0'+n)) + `","expires_in":7200}`))
	}))
	defer srv.Close()

	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	c := NewTokenCache(WithTokenBaseURL(srv.URL), WithTokenClock(clock))

	if _, err := c.GetAccessToken(context.Background(), "APP", "SEC"); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Still within the skew-adjusted lifetime (7200s - 300s) -> cache hit.
	now = now.Add(1 * time.Hour)
	if _, err := c.GetAccessToken(context.Background(), "APP", "SEC"); err != nil {
		t.Fatalf("within-life: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected 1 hit within lifetime, got %d", got)
	}
	// Past the skew-adjusted expiry -> refetch.
	now = now.Add(2 * time.Hour)
	if _, err := c.GetAccessToken(context.Background(), "APP", "SEC"); err != nil {
		t.Fatalf("after-expiry: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("expected refetch after expiry (2 hits), got %d", got)
	}
}

func TestGetAccessToken_Invalidate(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":7200}`))
	}))
	defer srv.Close()

	c := NewTokenCache(WithTokenBaseURL(srv.URL))
	if _, err := c.GetAccessToken(context.Background(), "APP", "SEC"); err != nil {
		t.Fatalf("first: %v", err)
	}
	c.Invalidate("APP", "SEC")
	if _, err := c.GetAccessToken(context.Background(), "APP", "SEC"); err != nil {
		t.Fatalf("after-invalidate: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("expected refetch after invalidate (2 hits), got %d", got)
	}
}

func TestGetAccessToken_ErrCode(t *testing.T) {
	srv := newFakeServer(t, http.StatusOK, `{"errcode":40013,"errmsg":"invalid appid"}`, nil)
	defer srv.Close()

	c := NewTokenCache(WithTokenBaseURL(srv.URL))
	_, err := c.GetAccessToken(context.Background(), "APP", "SEC")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if apiErr.ErrCode != 40013 || apiErr.IsTokenInvalid() {
		t.Fatalf("errcode/tokeninvalid mismatch: %+v", apiErr)
	}
}

func TestGetAccessToken_TokenInvalidCodes(t *testing.T) {
	for _, code := range []int{40001, 40014, 42001, 41001} {
		e := &APIError{ErrCode: code}
		if !e.IsTokenInvalid() {
			t.Fatalf("expected %d to be token-invalid", code)
		}
	}
}

func TestGetAccessToken_RequiresArgs(t *testing.T) {
	c := NewTokenCache()
	if _, err := c.GetAccessToken(context.Background(), "", "s"); err == nil {
		t.Fatal("expected error for empty appid")
	}
	if _, err := c.GetAccessToken(context.Background(), "a", ""); err == nil {
		t.Fatal("expected error for empty secret")
	}
}

func TestSendSubscribeMessage_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("access_token") != "TOK" {
			t.Errorf("missing access_token: %v", r.URL.Query())
		}
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "/cgi-bin/message/subscribe/send") {
			t.Errorf("unexpected method/path: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	err := c.SendSubscribeMessage(context.Background(), "TOK", SubscribeMessageInput{
		OpenID:     "oABC",
		TemplateID: "tpl-1",
		Page:       "pages/index",
		Data:       map[string]DataItem{"thing1": {Value: "hi"}},
	})
	if err != nil {
		t.Fatalf("SendSubscribeMessage: %v", err)
	}
}

func TestSendSubscribeMessage_ErrCode(t *testing.T) {
	srv := newFakeServer(t, http.StatusOK, `{"errcode":43101,"errmsg":"user refuse to accept the msg"}`, nil)
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	err := c.SendSubscribeMessage(context.Background(), "TOK", SubscribeMessageInput{
		OpenID:     "oABC",
		TemplateID: "tpl-1",
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if apiErr.ErrCode != 43101 {
		t.Fatalf("errcode mismatch: %+v", apiErr)
	}
}

func TestSendSubscribeMessage_RequiresArgs(t *testing.T) {
	c := New()
	if err := c.SendSubscribeMessage(context.Background(), "", SubscribeMessageInput{OpenID: "o", TemplateID: "t"}); err == nil {
		t.Fatal("expected error for empty token")
	}
	if err := c.SendSubscribeMessage(context.Background(), "TOK", SubscribeMessageInput{TemplateID: "t"}); err == nil {
		t.Fatal("expected error for empty openid")
	}
	if err := c.SendSubscribeMessage(context.Background(), "TOK", SubscribeMessageInput{OpenID: "o"}); err == nil {
		t.Fatal("expected error for empty template_id")
	}
}
