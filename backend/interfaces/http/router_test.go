package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/httpx"
)

func init() { gin.SetMode(gin.TestMode) }

func newTestRouter() *gin.Engine {
	return New(Deps{Cfg: &config.Config{App: config.AppConfig{Env: "test"}}})
}

func decodeEnvelope(t *testing.T, body []byte) httpx.Envelope {
	t.Helper()
	var env httpx.Envelope
	require.NoError(t, json.Unmarshal(body, &env))
	return env
}

// TestHealthz proves the global middleware chain (recover→requestid→logging)
// terminates in a success envelope with a populated trace_id.
func TestHealthz(t *testing.T) {
	r := newTestRouter()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	require.Equal(t, http.StatusOK, w.Code)
	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, 0, env.Code)
	assert.NotEmpty(t, env.TraceID, "trace_id must be populated")
}

// TestRequestIDPropagated: an inbound X-Request-Id is echoed back in the
// response header and the envelope trace_id.
func TestRequestIDPropagated(t *testing.T) {
	r := newTestRouter()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "req-abc-123")
	r.ServeHTTP(w, req)

	assert.Equal(t, "req-abc-123", w.Header().Get("X-Request-Id"))
	assert.Equal(t, "req-abc-123", decodeEnvelope(t, w.Body.Bytes()).TraceID)
}

// TestWhoami_RejectsWithoutTenant is the HTTP-level串户 guard test: a request
// that resolves no tenant MUST be rejected with ErrTenantCross (40302), proving
// the guard cannot be bypassed even by a direct handler hit.
func TestWhoami_RejectsWithoutTenant(t *testing.T) {
	r := newTestRouter()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/boss/whoami", nil))

	require.Equal(t, http.StatusForbidden, w.Code)
	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, 40302, env.Code, "must be ErrTenantCross code 40302")
	assert.Nil(t, env.Data)
}

// TestWhoami_AllowsWithTenant: with a resolved tenant the guard passes and the
// handler echoes the tenant_id from context (never from the body).
func TestWhoami_AllowsWithTenant(t *testing.T) {
	r := newTestRouter()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/boss/whoami", nil)
	req.Header.Set("X-Mock-Tenant", "1001")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, 0, env.Code)
	data, ok := env.Data.(map[string]any)
	require.True(t, ok, "data must be the whoami object")
	assert.EqualValues(t, 1001, data["tenant_id"])
}

// TestMetrics: /metrics is wired and serves the default Prometheus collector.
func TestMetrics(t *testing.T) {
	r := newTestRouter()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusOK, w.Code)
}
