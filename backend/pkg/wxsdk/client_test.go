package wxsdk

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newFakeServer(t *testing.T, status int, resp string, check func(q map[string]string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			q := map[string]string{}
			for k, vs := range r.URL.Query() {
				if len(vs) > 0 {
					q[k] = vs[0]
				}
			}
			check(q)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(resp))
	}))
}

func TestCode2Session_Success(t *testing.T) {
	srv := newFakeServer(t, http.StatusOK, `{"openid":"oABC","session_key":"sk123","unionid":"uXYZ"}`, func(q map[string]string) {
		if q["appid"] != "APP" || q["secret"] != "SEC" || q["js_code"] != "CODE" || q["grant_type"] != "authorization_code" {
			t.Errorf("unexpected query: %v", q)
		}
	})
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	sess, err := c.Code2Session(context.Background(), "APP", "SEC", "CODE")
	if err != nil {
		t.Fatalf("Code2Session: %v", err)
	}
	if sess.OpenID != "oABC" || sess.SessionKey != "sk123" || sess.UnionID != "uXYZ" {
		t.Fatalf("session mismatch: %+v", sess)
	}
}

func TestCode2Session_WxError(t *testing.T) {
	srv := newFakeServer(t, http.StatusOK, `{"errcode":40029,"errmsg":"invalid code"}`, nil)
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	_, err := c.Code2Session(context.Background(), "APP", "SEC", "bad")
	var wxErr *Code2SessionError
	if !errors.As(err, &wxErr) {
		t.Fatalf("expected *Code2SessionError, got %T %v", err, err)
	}
	if wxErr.ErrCode != 40029 || !wxErr.IsWellKnownInvalid() {
		t.Fatalf("errcode/wellknown mismatch: %+v", wxErr)
	}
}

func TestCode2Session_RequiresAllArgs(t *testing.T) {
	c := New()
	for _, tc := range []struct{ appid, secret, code string }{
		{"", "s", "c"}, {"a", "", "c"}, {"a", "s", ""},
	} {
		if _, err := c.Code2Session(context.Background(), tc.appid, tc.secret, tc.code); err == nil {
			t.Fatalf("expected error for %+v", tc)
		}
	}
}

func TestCode2Session_HTTP500(t *testing.T) {
	srv := newFakeServer(t, http.StatusInternalServerError, `boom`, nil)
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	_, err := c.Code2Session(context.Background(), "a", "s", "c")
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("expected HTTP 500 error, got %v", err)
	}
}

func TestCode2Session_MissingOpenID(t *testing.T) {
	// errcode 0 but no openid/session_key -> error.
	srv := newFakeServer(t, http.StatusOK, `{"openid":"","session_key":""}`, nil)
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	if _, err := c.Code2Session(context.Background(), "a", "s", "c"); err == nil {
		t.Fatal("expected error for empty openid/session_key")
	}
}

func TestCode2Session_Timeout(t *testing.T) {
	// Server that sleeps past the client timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithHTTPClient(&http.Client{Timeout: 20 * time.Millisecond}))
	_, err := c.Code2Session(context.Background(), "a", "s", "c")
	if err == nil {
		t.Fatal("expected timeout error")
	}
}
