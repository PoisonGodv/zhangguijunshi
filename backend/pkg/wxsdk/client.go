// Package wxsdk is a minimal WeChat Mini Program client for the single call the
// M0-S2 auth flow needs: jscode2session (wx.login → openid/session_key).
//
// It speaks HTTP directly with net/http rather than pulling a full SDK (e.g.
// silenceper/wechat): code2session is one GET, the dependency surface should
// stay small at POC stage, and a plain client is trivial to point at an
// httptest fake. Broader WeChat APIs (subscribe messages, payment, phone
// decryption) will warrant a richer client later.
//
// Security note (tech plan §A.1.1): session_key NEVER leaves the server. The
// caller is responsible for caching it server-side and never returning it to
// the client; this package only returns it to the caller.
package wxsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DefaultBaseURL is the official WeChat open-platform API root.
const DefaultBaseURL = "https://api.weixin.qq.com"

// code2SessionPath is the jscode2session endpoint.
const code2SessionPath = "/sns/jscode2session"

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the API root (for tests / proxies).
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.baseURL = u
		}
	}
}

// WithHTTPClient injects a custom *http.Client (e.g. for test transport control).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithTimeout sets the per-request timeout used by the default client builder.
// It has no effect if WithHTTPClient is also supplied.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// Client is a stateless WeChat Mini Program client. Credentials are passed per
// call so the same client can serve multiple tenants (each with its own
// AppID/Secret) in production.
type Client struct {
	baseURL string
	http    *http.Client
	timeout time.Duration
}

// New constructs a Client. The default baseURL is the official endpoint and the
// default timeout is 5s; both are overridable via Options.
func New(opts ...Option) *Client {
	c := &Client{
		baseURL: DefaultBaseURL,
		timeout: 5 * time.Second,
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: c.timeout}
	}
	return c
}

// Session is the server-side result of jscode2session. SessionKey is secret —
// it must never be returned to the mini-program client.
type Session struct {
	OpenID     string
	SessionKey string
	UnionID    string
}

// Code2SessionError carries a WeChat errcode/errmsg. Callers map well-known
// codes (invalid/expired code) to errcode.ErrParam; others map to ErrUpstream.
type Code2SessionError struct {
	ErrCode int
	ErrMsg  string
}

func (e *Code2SessionError) Error() string {
	return fmt.Sprintf("wxsdk: code2session failed (errcode=%d errmsg=%q)", e.ErrCode, e.ErrMsg)
}

// IsWellKnownInvalid reports whether errcode indicates an invalid/expired code
// (40029 invalid code, 40163 code been used, 45011 frequency limit). Such
// errors are caller faults, not upstream outages.
func (e *Code2SessionError) IsWellKnownInvalid() bool {
	switch e.ErrCode {
	case 40029, 40163, 45011, 40226:
		return true
	}
	return false
}

// code2SessionResponse mirrors the raw WeChat JSON. errcode is absent on success.
type code2SessionResponse struct {
	OpenID     string `json:"openid"`
	SessionKey string `json:"session_key"`
	UnionID    string `json:"unionid"`
	ErrCode    int    `json:"errcode"`
	ErrMsg     string `json:"errmsg"`
}

// Code2Session exchanges a wx.login code for an OpenID + SessionKey.
// See https://developers.weixin.qq.com/miniprogram/dev/api-backend/open-api/login/auth.code2Session.html
func (c *Client) Code2Session(ctx context.Context, appID, secret, code string) (*Session, error) {
	if appID == "" || secret == "" || code == "" {
		return nil, fmt.Errorf("wxsdk: app_id, secret and code are all required")
	}

	q := url.Values{}
	q.Set("appid", appID)
	q.Set("secret", secret)
	q.Set("js_code", code)
	q.Set("grant_type", "authorization_code")

	endpoint := c.baseURL + code2SessionPath + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("wxsdk: build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wxsdk: call code2session: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16)) // 64 KiB cap; response is tiny
	if err != nil {
		return nil, fmt.Errorf("wxsdk: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wxsdk: code2session HTTP %d: %s", resp.StatusCode, string(body))
	}

	var parsed code2SessionResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("wxsdk: decode response: %w (body=%q)", err, string(body))
	}
	if parsed.ErrCode != 0 {
		return nil, &Code2SessionError{ErrCode: parsed.ErrCode, ErrMsg: parsed.ErrMsg}
	}
	if parsed.OpenID == "" || parsed.SessionKey == "" {
		return nil, fmt.Errorf("wxsdk: code2session returned no openid/session_key (body=%q)", string(body))
	}
	return &Session{
		OpenID:     parsed.OpenID,
		SessionKey: parsed.SessionKey,
		UnionID:    parsed.UnionID,
	}, nil
}
