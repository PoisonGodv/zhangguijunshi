package jwt

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

func TestDevKey_Deterministic(t *testing.T) {
	k1, err := DevKey("seed-A")
	if err != nil {
		t.Fatalf("DevKey: %v", err)
	}
	k2, err := DevKey("seed-A")
	if err != nil {
		t.Fatalf("DevKey again: %v", err)
	}
	// Same seed -> same public point X coordinate.
	if k1.PublicKey.X.Cmp(k2.PublicKey.X) != 0 || k1.PublicKey.Y.Cmp(k2.PublicKey.Y) != 0 {
		t.Fatal("DevKey not deterministic across calls with same seed")
	}
	k3, err := DevKey("seed-B")
	if err != nil {
		t.Fatalf("DevKey B: %v", err)
	}
	if k3.PublicKey.X.Cmp(k1.PublicKey.X) == 0 {
		t.Fatal("DevKey different seed produced same key")
	}
}

func TestDevKey_EmptySeed(t *testing.T) {
	if _, err := DevKey(""); err == nil {
		t.Fatal("expected error for empty seed")
	}
}

func TestLoadPrivateKeyPEM_RoundTrip(t *testing.T) {
	priv, err := DevKey("pem-rt")
	if err != nil {
		t.Fatalf("DevKey: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})

	got, err := LoadPrivateKeyPEM(pemBytes)
	if err != nil {
		t.Fatalf("LoadPrivateKeyPEM: %v", err)
	}
	if got.D.Cmp(priv.D) != 0 {
		t.Fatal("round-trip key mismatch")
	}
}

func TestLoadPrivateKeyPEM_RejectsNonP256(t *testing.T) {
	// Generate a P-521 key and try to load it -> must be rejected.
	priv := mustGenP521(t)
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if _, err := LoadPrivateKeyPEM(pemBytes); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("expected ErrInvalidKey, got %v", err)
	}
}

func TestNew_RequiresPrivateKey(t *testing.T) {
	if _, err := New(Config{AccessTTL: time.Minute}); err == nil {
		t.Fatal("expected error for nil private key")
	}
}

func TestSignVerify_HappyPath(t *testing.T) {
	mgr := newTestManager(t, "iss-test", time.Minute)
	tok, claims, err := mgr.Sign(context.Background(), IssueInput{
		Subject: "u_1", Audience: []string{"boss"}, Scene: SceneBoss,
		TenantID: 42, StoreID: 7, Role: "owner", OpenID: "o_abc", Version: 3,
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if claims.ID == "" {
		t.Fatal("jti empty")
	}
	if claims.ExpiresAt == nil || claims.ExpiresAt.Before(time.Now()) {
		t.Fatal("bad exp")
	}
	if !strings.HasPrefix(tok, "eyJ") {
		t.Fatalf("token does not look like JWT: %s", tok[:10])
	}

	got, err := mgr.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.TenantID != 42 || got.Scene != SceneBoss || got.Subject != "u_1" ||
		got.StoreID != 7 || got.Role != "owner" || got.OpenID != "o_abc" || got.Version != 3 {
		t.Fatalf("claims mismatch: %+v", got)
	}
	if got.ID != claims.ID {
		t.Fatalf("jti mismatch: %s vs %s", got.ID, claims.ID)
	}
}

func TestSign_KIDInHeader(t *testing.T) {
	priv, _ := DevKey("kid")
	mgr, err := New(Config{PrivateKey: priv, KeyID: "zgjs-2026-07", Issuer: "iss", AccessTTL: time.Minute})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tok, _, err := mgr.Sign(context.Background(), IssueInput{Subject: "u_1"})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// Decode header segment to assert kid presence.
	hdrSeg := strings.Split(tok, ".")[0]
	hdrJSON, err := base64.RawURLEncoding.DecodeString(hdrSeg)
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if !strings.Contains(string(hdrJSON), `"kid":"zgjs-2026-07"`) {
		t.Fatalf("kid header missing from %s", hdrJSON)
	}
}

func TestVerify_Expired(t *testing.T) {
	priv, _ := DevKey("exp")
	mgr, _ := New(Config{PrivateKey: priv, Issuer: "iss", AccessTTL: time.Minute})
	expired := signAt(t, priv, "iss", time.Now().Add(-2*time.Minute), time.Now().Add(-time.Minute))
	if _, err := mgr.Verify(expired); !errors.Is(err, ErrExpired) {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestVerify_WrongIssuer(t *testing.T) {
	priv, _ := DevKey("iss-bad")
	mgr, _ := New(Config{PrivateKey: priv, Issuer: "real-iss", AccessTTL: time.Minute})
	tok := signAt(t, priv, "other-iss", time.Now(), time.Now().Add(time.Minute))
	if _, err := mgr.Verify(tok); !errors.Is(err, ErrWrongIssuer) {
		t.Fatalf("expected ErrWrongIssuer, got %v", err)
	}
}

func TestVerify_BadSignature(t *testing.T) {
	privA, _ := DevKey("A")
	privB, _ := DevKey("B")
	mgrA, _ := New(Config{PrivateKey: privA, Issuer: "iss", AccessTTL: time.Minute})
	tok := signAt(t, privB, "iss", time.Now(), time.Now().Add(time.Minute)) // signed by B
	if _, err := mgrA.Verify(tok); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature, got %v", err)
	}
}

func TestVerify_RejectsWrongAlgorithm(t *testing.T) {
	priv, _ := DevKey("alg")
	mgr, _ := New(Config{PrivateKey: priv, Issuer: "iss", AccessTTL: time.Minute})
	// Build a forged HS256 token (header/body/empty-sig) and verify the parser
	// rejects it because ES256 is the only allowed method.
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	forged := enc(`{"alg":"HS256","typ":"JWT"}`) + "." +
		enc(`{"sub":"u_1","iss":"iss","exp":`+itoa(time.Now().Add(time.Minute).Unix())+`}`) + ".bogus"
	if _, err := mgr.Verify(forged); err == nil {
		t.Fatal("expected error for non-ES256 token")
	}
}

func TestVerify_AudienceEnforced(t *testing.T) {
	priv, _ := DevKey("aud")
	verifier, err := NewVerifier(&priv.PublicKey, "", "iss", []string{"boss"})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	signer, _ := New(Config{PrivateKey: priv, Issuer: "iss", AccessTTL: time.Minute})
	tok, _, err := signer.Sign(context.Background(), IssueInput{Subject: "u_1", Audience: []string{"admin"}})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := verifier.Verify(tok); !errors.Is(err, ErrWrongAudience) {
		t.Fatalf("expected ErrWrongAudience, got %v", err)
	}
}

// signAt builds a signed ES256 token directly so tests can pin iat/exp/iss
// independently of Signer (used to exercise Verify's validation paths).
func signAt(t *testing.T, priv *ecdsa.PrivateKey, issuer string, iat, exp time.Time) string {
	t.Helper()
	claims := &Claims{
		RegisteredClaims: jwtv5.RegisteredClaims{
			Issuer:    issuer,
			Subject:   "u_test",
			IssuedAt:  jwtv5.NewNumericDate(iat),
			ExpiresAt: jwtv5.NewNumericDate(exp),
			ID:        "fixed-jti",
		},
	}
	tok := jwtv5.NewWithClaims(jwtv5.SigningMethodES256, claims)
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("signAt: %v", err)
	}
	return s
}

func newTestManager(t *testing.T, issuer string, ttl time.Duration) *Manager {
	t.Helper()
	priv, err := DevKey("test-" + issuer)
	if err != nil {
		t.Fatalf("DevKey: %v", err)
	}
	mgr, err := New(Config{PrivateKey: priv, Issuer: issuer, AccessTTL: ttl})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return mgr
}

func mustGenP521(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	// DevKey forces P-256, so for a non-P256 rejection test we need P-521.
	k, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	if err != nil {
		t.Fatalf("gen p521: %v", err)
	}
	return k
}

// itoa is a strconv-free int64->string for embedding exp in forged tokens.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
