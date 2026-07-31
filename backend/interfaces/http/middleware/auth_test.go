package middleware

// auth_test.go exercises the Auth middleware's ver-claim enforcement
// (CONTRACT §20.5). A fake tokenVerifier returns a preset *jwt.Claims (the
// middleware does not care about the token string here), and a fake
// versionChecker returns a preset current version. Asserts the allow/deny
// matrix: stale token (claims.Version < current) -> 40101; fresh -> pass;
// cache miss (current==0) -> pass; checker error -> pass; nil checker ->
// pass (backward compatible); no token -> 40101.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/pkg/jwt"
)

// fakeVerifier returns a preset claims pair, ignoring the token string.
type fakeVerifier struct {
	claims *jwt.Claims
	err    error
}

func (f fakeVerifier) Verify(string) (*jwt.Claims, error) { return f.claims, f.err }

// fakeChecker returns a preset (version, err) and records that it was called.
type fakeChecker struct {
	v      int64
	err    error
	called bool
	scene  string
	userID int64
}

func (f *fakeChecker) CurrentVersion(_ context.Context, scene string, userID int64) (int64, error) {
	f.called = true
	f.scene = scene
	f.userID = userID
	return f.v, f.err
}

// runAuth mounts Auth on a tiny router with a 200 terminal handler and issues a
// GET with a fake Bearer token. Returns the recorder.
func runAuth(t *testing.T, verifier tokenVerifier, checker versionChecker) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	r.GET("/x", Auth(verifier, nil, checker), func(c *gin.Context) { c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer faketoken")
	r.ServeHTTP(w, req)
	return w
}

// mkClaims builds a *jwt.Claims with the promoted Subject field (embedded
// RegisteredClaims) set. Struct literals cannot set promoted fields, so this
// helper constructs then assigns.
func mkClaims(scene, subject string, version int64) *jwt.Claims {
	c := &jwt.Claims{Scene: scene, Version: version}
	c.Subject = subject
	return c
}

func TestAuth_FreshVersionPasses(t *testing.T) {
	claims := mkClaims(jwt.SceneBoss, "u_2001", 2)
	ch := &fakeChecker{v: 2} // current == claims.Version -> not stale
	w := runAuth(t, fakeVerifier{claims: claims}, ch)
	require.Equal(t, http.StatusOK, w.Code)
	require.True(t, ch.called)
	require.Equal(t, jwt.SceneBoss, ch.scene)
	require.Equal(t, int64(2001), ch.userID, "userID parsed from subject u_2001")
}

func TestAuth_StaleVersionRejected(t *testing.T) {
	claims := mkClaims(jwt.SceneBoss, "u_2001", 1)
	ch := &fakeChecker{v: 3} // current(3) > claims.Version(1) -> stale -> 40101
	w := runAuth(t, fakeVerifier{claims: claims}, ch)
	require.Equal(t, http.StatusUnauthorized, w.Code) // ErrTokenInvalid -> 401
}

// TestAuth_StaleVersionZeroRejected proves the no-short-circuit decision: a
// ver-0 token is rejected once the account has been bumped to >= 1. This is the
// first-password-change case (version 0 -> 1).
func TestAuth_StaleVersionZeroRejected(t *testing.T) {
	claims := mkClaims(jwt.SceneBoss, "u_2001", 0)
	ch := &fakeChecker{v: 1} // bumped to 1; old ver-0 token must fail
	w := runAuth(t, fakeVerifier{claims: claims}, ch)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuth_CacheMissPasses(t *testing.T) {
	claims := mkClaims(jwt.SceneBoss, "u_2001", 2)
	ch := &fakeChecker{v: 0} // miss -> allow (availability over strictness)
	w := runAuth(t, fakeVerifier{claims: claims}, ch)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestAuth_CheckerErrorPasses(t *testing.T) {
	claims := mkClaims(jwt.SceneBoss, "u_2001", 2)
	// A Redis flap (checker error) MUST NOT lock the user out -> allow.
	ch := &fakeChecker{v: 99, err: errors.New("redis down")}
	w := runAuth(t, fakeVerifier{claims: claims}, ch)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestAuth_NilCheckerPasses(t *testing.T) {
	// Backward compatible: a nil checker disables the ver check entirely.
	claims := mkClaims(jwt.SceneBoss, "u_2001", 2)
	w := runAuth(t, fakeVerifier{claims: claims}, nil)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestAuth_NoTokenRejected(t *testing.T) {
	r := gin.New()
	r.GET("/x", Auth(fakeVerifier{claims: mkClaims(jwt.SceneBoss, "u_1", 0)}, nil, nil), func(c *gin.Context) { c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil) // no Authorization header
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuth_BadSignatureRejected(t *testing.T) {
	// Verify error -> 40101 before the ver check ever runs.
	r := gin.New()
	called := false
	r.GET("/x", Auth(fakeVerifier{err: errors.New("bad sig")}, nil, &fakeChecker{v: 9}), func(c *gin.Context) {
		called = true
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer faketoken")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.False(t, called, "terminal handler must not run on a failed Verify")
}
