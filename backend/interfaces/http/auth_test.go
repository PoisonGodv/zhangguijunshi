package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/auth"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/httpx"
	"github.com/zhangguijunshi/backend/pkg/jwt"
	"github.com/zhangguijunshi/backend/pkg/wxsdk"
)

// testWX is a controllable Code2Sessioner for router-level auth tests.
type testWX struct {
	openID string
	err    error
}

func (t testWX) Code2Session(_ context.Context, _, _, _ string) (*wxsdk.Session, error) {
	if t.err != nil {
		return nil, t.err
	}
	return &wxsdk.Session{OpenID: t.openID, SessionKey: "sk-server-side"}, nil
}

// newAuthedRouter builds a router with a real (dev-key) JWT stack + memory
// SessionStore + a testWX so the full login→me→logout flow runs over HTTP.
func newAuthedRouter(t *testing.T, wx testWX) (*gin.Engine, *jwt.Manager) {
	t.Helper()
	priv, err := jwt.DevKey("router-auth-test")
	require.NoError(t, err)
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, Issuer: "zgjs-test", AccessTTL: time.Minute})
	require.NoError(t, err)
	store := jwt.NewMemorySessionStore()
	t.Cleanup(func() { _ = store.Close() })
	repo := auth.NewMemoryConsumerRepo()
	svc := auth.New(auth.Config{
		AppID: "A", Secret: "S", AccessTTL: time.Minute, RefreshTTL: time.Hour,
	}, wx, mgr, store, repo)
	return New(Deps{
		Cfg:     &config.Config{App: config.AppConfig{Env: "test"}},
		JWT:     mgr,
		Store:   store,
		AuthSvc: svc,
	}), mgr
}

func doJSON(t *testing.T, r http.Handler, method, path string, body any, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func envData(t *testing.T, w *httptest.ResponseRecorder) (httpx.Envelope, map[string]any) {
	t.Helper()
	var env httpx.Envelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	if env.Data == nil {
		return env, nil
	}
	data, _ := env.Data.(map[string]any)
	return env, data
}

// TestWxLogin_FullFlow exercises login → me → refresh over HTTP.
func TestWxLogin_FullFlow(t *testing.T) {
	r, _ := newAuthedRouter(t, testWX{openID: "oXYZ"})

	// Login.
	w := doJSON(t, r, http.MethodPost, "/v1/wxshop/auth/login",
		map[string]any{"code": "C", "tenant_id": 50021}, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, data := envData(t, w)
	access, _ := data["access_token"].(string)
	refresh, _ := data["refresh_token"].(string)
	require.NotEmpty(t, access)
	require.NotEmpty(t, refresh)
	require.Equal(t, "Bearer", data["token_type"])
	// session_key must never leak into the response.
	require.NotContains(t, w.Body.String(), "session_key")

	// /me with the token returns the token's tenant and scene.
	w = doJSON(t, r, http.MethodGet, "/v1/wxshop/auth/me", nil, access)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, data = envData(t, w)
	assert.EqualValues(t, 50021, data["tenant_id"])
	assert.Equal(t, jwt.SceneConsumer, data["scene"])

	// Refresh works and rotates the refresh token.
	w = doJSON(t, r, http.MethodPost, "/v1/wxshop/auth/refresh",
		map[string]any{"refresh_token": refresh}, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, data = envData(t, w)
	require.NotEqual(t, refresh, data["refresh_token"], "refresh token must rotate")
}

// TestAuth_TenantFromTokenNotHeader is the HTTP-level串户 guard proof: the
// resolved tenant comes from the verified JWT and cannot be overridden by the
// X-Mock-Tenant header (or any request input).
func TestAuth_TenantFromTokenNotHeader(t *testing.T) {
	r, _ := newAuthedRouter(t, testWX{openID: "oXYZ"})

	w := doJSON(t, r, http.MethodPost, "/v1/wxshop/auth/login",
		map[string]any{"code": "C", "tenant_id": 50021}, "")
	_, data := envData(t, w)
	access := data["access_token"].(string)

	// Send the tenant-50021 token but lie via the mock header about tenant 9999.
	req := httptest.NewRequest(http.MethodGet, "/v1/wxshop/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("X-Mock-Tenant", "9999")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	_, data = envData(t, w)
	assert.EqualValues(t, 50021, data["tenant_id"], "tenant must come from JWT, not header")
}

func TestAuthMe_RejectsNoToken(t *testing.T) {
	r, _ := newAuthedRouter(t, testWX{openID: "oXYZ"})
	w := doJSON(t, r, http.MethodGet, "/v1/wxshop/auth/me", nil, "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	env, _ := envData(t, w)
	assert.Equal(t, 40101, env.Code)
}

func TestAuthMe_RejectsBadToken(t *testing.T) {
	r, _ := newAuthedRouter(t, testWX{openID: "oXYZ"})
	w := doJSON(t, r, http.MethodGet, "/v1/wxshop/auth/me", nil, "not-a-jwt")
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestWxLogin_BadCode: a wx caller-fault error maps to ErrParam (40001).
func TestWxLogin_BadCode(t *testing.T) {
	r, _ := newAuthedRouter(t, testWX{err: &wxsdk.Code2SessionError{ErrCode: 40029, ErrMsg: "invalid code"}})
	w := doJSON(t, r, http.MethodPost, "/v1/wxshop/auth/login",
		map[string]any{"code": "bad", "tenant_id": 1}, "")
	require.Equal(t, http.StatusBadRequest, w.Code)
	env, _ := envData(t, w)
	assert.Equal(t, 40001, env.Code)
}

// TestLogout_BlocksToken: after logout the access token's jti is blacklisted.
func TestLogout_BlocksToken(t *testing.T) {
	r, _ := newAuthedRouter(t, testWX{openID: "oXYZ"})

	w := doJSON(t, r, http.MethodPost, "/v1/wxshop/auth/login",
		map[string]any{"code": "C", "tenant_id": 7}, "")
	_, data := envData(t, w)
	access := data["access_token"].(string)
	refresh := data["refresh_token"].(string)

	// /me works before logout.
	w = doJSON(t, r, http.MethodGet, "/v1/wxshop/auth/me", nil, access)
	require.Equal(t, http.StatusOK, w.Code)

	// Logout (access via Authorization, refresh via header per tech plan).
	req := httptest.NewRequest(http.MethodPost, "/v1/wxshop/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("X-Refresh-Token", refresh)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// /me now rejected (jti blacklisted).
	w = doJSON(t, r, http.MethodGet, "/v1/wxshop/auth/me", nil, access)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// Refresh of the revoked token also rejected.
	w = doJSON(t, r, http.MethodPost, "/v1/wxshop/auth/refresh",
		map[string]any{"refresh_token": refresh}, "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
}
