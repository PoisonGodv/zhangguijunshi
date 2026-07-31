package wxsdk

// token.go adds the access_token capability to the minimal WeChat client
// (tech plan §4.3 reach / subscribe-message delivery). The Mini Program
// subscribe-message send API authenticates with a global access_token obtained
// from GET /cgi-bin/token?grant_type=client_credential. That token is valid for
// ~2h (WeChat returns expires_in seconds) and is rate-limited, so callers MUST
// cache it rather than fetching one per message; TokenCache does exactly that
// with an in-process mutex-guarded cache that expires entries ~5min early to
// avoid racing the server-side expiry.
//
// The style mirrors client.go: net/http directly, functional Options, and an
// injectable baseURL / *http.Client / clock so tests run against an httptest
// fake with deterministic time.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// tokenPath is the access_token endpoint.
const tokenPath = "/cgi-bin/token"

// tokenExpirySkew is how long before the server-side expiry a cached token is
// considered stale. Refreshing ~5min early avoids sending with a token that
// dies mid-flight.
const tokenExpirySkew = 5 * time.Minute

// APIError carries a WeChat errcode/errmsg returned by the management APIs
// (access_token, subscribe-message send). Callers map well-known token codes
// via IsTokenInvalid; everything else is a generic upstream fault.
type APIError struct {
	ErrCode int
	ErrMsg  string
	// Op names the operation that failed (e.g. "get_access_token",
	// "send_subscribe_message") for readable error strings.
	Op string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("wxsdk: %s failed (errcode=%d errmsg=%q)", e.Op, e.ErrCode, e.ErrMsg)
}

// IsTokenInvalid reports whether errcode indicates an invalid/expired
// access_token (40001 invalid credential, 40014 invalid access_token, 42001
// access_token expired, 40001/41001 missing). A caller that sees one of these
// may invalidate its cached token and retry once.
func (e *APIError) IsTokenInvalid() bool {
	switch e.ErrCode {
	case 40001, 40014, 42001, 41001:
		return true
	}
	return false
}

// TokenOption configures a TokenCache.
type TokenOption func(*TokenCache)

// WithTokenBaseURL overrides the API root (for tests / proxies). Empty is
// ignored, keeping the official endpoint.
func WithTokenBaseURL(u string) TokenOption {
	return func(c *TokenCache) {
		if u != "" {
			c.baseURL = u
		}
	}
}

// WithTokenHTTPClient injects a custom *http.Client (e.g. for test transport
// control). nil is ignored.
func WithTokenHTTPClient(h *http.Client) TokenOption {
	return func(c *TokenCache) {
		if h != nil {
			c.http = h
		}
	}
}

// WithTokenClock overrides the cache clock (deterministic expiry tests). nil is
// ignored, keeping time.Now.
func WithTokenClock(f func() time.Time) TokenOption {
	return func(c *TokenCache) {
		if f != nil {
			c.now = f
		}
	}
}

// WithTokenTimeout sets the per-request timeout used when no explicit
// *http.Client is injected. It has no effect if WithTokenHTTPClient is supplied.
func WithTokenTimeout(d time.Duration) TokenOption {
	return func(c *TokenCache) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// tokenKey identifies one cached token by the credential pair it was fetched
// with, so a single TokenCache can serve multiple tenants (each AppID/Secret).
type tokenKey struct {
	appID  string
	secret string
}

// cachedToken is one cache entry: the token plus the (skew-adjusted) instant it
// is considered stale.
type cachedToken struct {
	token  string
	expiry time.Time
}

// TokenCache fetches and caches WeChat global access_tokens in-process. It is
// concurrency-safe: concurrent callers for the same credential pair serialise
// on a single mutex, so a cache miss triggers at most one upstream fetch at a
// time (thundering-herd safe within the process). The cache is per-process by
// design — at POC scale that is sufficient; a multi-instance deployment can
// later swap in a Redis-backed implementation behind the same GetAccessToken
// signature.
type TokenCache struct {
	baseURL string
	http    *http.Client
	timeout time.Duration
	now     func() time.Time

	mu      sync.Mutex
	entries map[tokenKey]cachedToken
}

// NewTokenCache constructs a TokenCache. Defaults: official baseURL, 5s
// timeout, time.Now clock. All overridable via TokenOptions.
func NewTokenCache(opts ...TokenOption) *TokenCache {
	c := &TokenCache{
		baseURL: DefaultBaseURL,
		timeout: 5 * time.Second,
		now:     time.Now,
		entries: make(map[tokenKey]cachedToken),
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: c.timeout}
	}
	return c
}

// tokenResponse mirrors the raw WeChat JSON. On success errcode is absent (0);
// on failure access_token is empty and errcode/errmsg are set.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	ErrCode     int    `json:"errcode"`
	ErrMsg      string `json:"errmsg"`
}

// GetAccessToken returns a valid access_token for (appID, secret), fetching and
// caching a fresh one only when the cache is empty or stale. The returned token
// is safe to use immediately; the cache expires entries tokenExpirySkew before
// the server-reported deadline.
//
// See https://developers.weixin.qq.com/miniprogram/dev/OpenApiDoc/mp-access-token/getStableAccessToken.html
func (c *TokenCache) GetAccessToken(ctx context.Context, appID, secret string) (string, error) {
	if appID == "" || secret == "" {
		return "", fmt.Errorf("wxsdk: app_id and secret are both required")
	}
	key := tokenKey{appID: appID, secret: secret}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if entry, ok := c.entries[key]; ok && now.Before(entry.expiry) {
		return entry.token, nil
	}

	token, expiresIn, err := c.fetch(ctx, appID, secret)
	if err != nil {
		return "", err
	}

	// Cache with the skew-adjusted expiry. Guard against a tiny/negative
	// expires_in (a misbehaving upstream) by flooring the lifetime at 1s so we
	// never store an already-stale entry that would refetch on every call.
	lifetime := time.Duration(expiresIn)*time.Second - tokenExpirySkew
	if lifetime < time.Second {
		lifetime = time.Second
	}
	c.entries[key] = cachedToken{token: token, expiry: now.Add(lifetime)}
	return token, nil
}

// Invalidate drops the cached token for (appID, secret) so the next
// GetAccessToken refetches. Callers use it after an IsTokenInvalid APIError to
// force a fresh token before retrying a send. Absent entry is a no-op.
func (c *TokenCache) Invalidate(appID, secret string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, tokenKey{appID: appID, secret: secret})
}

// fetch performs the one upstream GET /cgi-bin/token call and returns the token
// plus its server-reported lifetime in seconds. Caller holds c.mu.
func (c *TokenCache) fetch(ctx context.Context, appID, secret string) (string, int, error) {
	q := url.Values{}
	q.Set("grant_type", "client_credential")
	q.Set("appid", appID)
	q.Set("secret", secret)

	endpoint := c.baseURL + tokenPath + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", 0, fmt.Errorf("wxsdk: build token request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("wxsdk: call get_access_token: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16)) // 64 KiB cap; response is tiny
	if err != nil {
		return "", 0, fmt.Errorf("wxsdk: read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("wxsdk: get_access_token HTTP %d: %s", resp.StatusCode, string(body))
	}

	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, fmt.Errorf("wxsdk: decode token response: %w (body=%q)", err, string(body))
	}
	if parsed.ErrCode != 0 {
		return "", 0, &APIError{ErrCode: parsed.ErrCode, ErrMsg: parsed.ErrMsg, Op: "get_access_token"}
	}
	if parsed.AccessToken == "" {
		return "", 0, fmt.Errorf("wxsdk: get_access_token returned empty token (body=%q)", string(body))
	}
	return parsed.AccessToken, parsed.ExpiresIn, nil
}
