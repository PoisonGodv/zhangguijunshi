package jwt

// This file implements the access-token Claims shape (tech plan §6.2.4) plus the
// ES256 Signer and Verifier. Claims are unified across all three client
// surfaces (wxshop/boss/admin): the only field that varies by surface is Scene
// and which of StoreID/Role/OpenID are populated.
//
// The refresh token is intentionally NOT a JWT — it is an opaque string backed
// by the SessionStore (pkg/jwt/session.go) so it can be revoked instantly,
// invalidated per-device, and counted for "online devices". Only the access
// token flows through Signer/Verifier here.

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

// Scene identifies which client surface a token belongs to.
const (
	SceneConsumer = "wxshop"
	SceneBoss     = "boss"
	SceneAdmin    = "admin"
)

// Sentinel errors. The middleware maps any Verify error to errcode.ErrTokenInvalid
// (40101); callers that need to distinguish expiry from tampering may use errors.Is.
var (
	ErrExpired       = errors.New("jwt: token expired")
	ErrNotYetValid   = errors.New("jwt: token not yet valid")
	ErrBadSignature  = errors.New("jwt: signature mismatch")
	ErrWrongIssuer   = errors.New("jwt: wrong issuer")
	ErrWrongAudience = errors.New("jwt: wrong audience")
	ErrMalformed     = errors.New("jwt: malformed token")
)

// Claims mirrors the access-token claims in tech plan §6.2.4. The embedded
// jwtv5.RegisteredClaims carries iss/sub/aud/iat/exp/jti; the custom fields
// carry the multi-tenant isolation key (TenantID), scoping (StoreID), RBAC
// (Role), WeChat identity (OpenID), token version (Version) and surface (Scene).
type Claims struct {
	Scene    string `json:"scene,omitempty"`     // wxshop | boss | admin
	TenantID int64  `json:"tenant_id,omitempty"` // == merchant_id; row-level isolation core
	StoreID  int64  `json:"store_id,omitempty"`  // current store (boss/wxshop; admin=0)
	Role     string `json:"role,omitempty"`      // owner|clerk|cashier|region_mgr|platform_ops
	OpenID   string `json:"openid,omitempty"`    // wxshop/boss scan login
	Version  int64  `json:"ver,omitempty"`       // bumped on password change / kick; old tokens fail ver check
	jwtv5.RegisteredClaims
}

// IssueInput is the caller-supplied payload for Signer.Sign; the signer fills
// iat/exp/iss/jti itself.
type IssueInput struct {
	Subject  string   // internal uid, e.g. "u_100234"
	Audience []string // e.g. ["boss"]
	Scene    string
	TenantID int64
	StoreID  int64
	Role     string
	OpenID   string
	Version  int64
	JTI      string // optional; auto-generated (8 random bytes hex) when empty
}

// Config bundles the primitives needed to construct a Manager.
type Config struct {
	PrivateKey *ecdsa.PrivateKey // required; use DevKey or LoadPrivateKeyPEM to obtain
	KeyID      string            // kid header; identifies the signing key for rotation
	Issuer     string            // iss claim
	AccessTTL  time.Duration     // exp = iat + AccessTTL
}

// Signer issues ES256 access tokens.
type Signer struct {
	key       *ecdsa.PrivateKey
	keyID     string
	issuer    string
	accessTTL time.Duration
}

// Verifier validates ES256 access tokens and returns the parsed Claims.
type Verifier struct {
	pub      *ecdsa.PublicKey
	issuer   string
	audience []string
}

// Manager bundles a Signer and a Verifier backed by the same key pair. The
// modular monolith both signs and verifies (no separate gateway yet), so this
// is the type wiring layers construct; a dedicated Verifier is exported for the
// day a gateway needs signature-only checks with the public key alone.
type Manager struct {
	*Signer
	*Verifier
}

// New constructs a Manager (signer + verifier) from one key pair.
func New(cfg Config) (*Manager, error) {
	if cfg.PrivateKey == nil {
		return nil, errors.New("jwt: PrivateKey is required")
	}
	if cfg.AccessTTL <= 0 {
		return nil, errors.New("jwt: AccessTTL must be positive")
	}
	return &Manager{
		Signer: &Signer{
			key:       cfg.PrivateKey,
			keyID:     cfg.KeyID,
			issuer:    cfg.Issuer,
			accessTTL: cfg.AccessTTL,
		},
		Verifier: &Verifier{
			pub:    &cfg.PrivateKey.PublicKey,
			issuer: cfg.Issuer,
		},
	}, nil
}

// NewVerifier constructs a Verifier from a public key alone (gateway use case).
func NewVerifier(pub *ecdsa.PublicKey, keyID, issuer string, audience []string) (*Verifier, error) {
	if pub == nil {
		return nil, errors.New("jwt: public key is required")
	}
	return &Verifier{pub: pub, issuer: issuer, audience: audience}, nil
}

// Sign issues a signed access token for in. The returned Claims are the exact
// claims embedded in the token (useful for logging jti/exp without re-parsing).
func (s *Signer) Sign(_ context.Context, in IssueInput) (string, *Claims, error) {
	now := time.Now()
	jti := in.JTI
	if jti == "" {
		var err error
		jti, err = randomID(8)
		if err != nil {
			return "", nil, fmt.Errorf("jwt: generate jti: %w", err)
		}
	}
	claims := &Claims{
		Scene:    in.Scene,
		TenantID: in.TenantID,
		StoreID:  in.StoreID,
		Role:     in.Role,
		OpenID:   in.OpenID,
		Version:  in.Version,
		RegisteredClaims: jwtv5.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   in.Subject,
			Audience:  jwtv5.ClaimStrings(in.Audience),
			IssuedAt:  jwtv5.NewNumericDate(now),
			ExpiresAt: jwtv5.NewNumericDate(now.Add(s.accessTTL)),
			ID:        jti,
		},
	}
	tok := jwtv5.NewWithClaims(jwtv5.SigningMethodES256, claims)
	if s.keyID != "" {
		tok.Header["kid"] = s.keyID
	}
	signed, err := tok.SignedString(s.key)
	if err != nil {
		return "", nil, fmt.Errorf("jwt: sign: %w", err)
	}
	return signed, claims, nil
}

// Verify validates the token signature, expiry and issuer, optionally the
// audience. It returns the parsed Claims on success, or a wrapped sentinel
// error on failure.
func (v *Verifier) Verify(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	opts := []jwtv5.ParserOption{
		jwtv5.WithValidMethods([]string{jwtv5.SigningMethodES256.Alg()}),
	}
	if v.issuer != "" {
		opts = append(opts, jwtv5.WithIssuer(v.issuer))
	}
	if len(v.audience) > 0 {
		opts = append(opts, jwtv5.WithAudience(v.audience[0]))
	}

	_, err := jwtv5.ParseWithClaims(tokenStr, claims, func(t *jwtv5.Token) (any, error) {
		return v.pub, nil
	}, opts...)
	if err != nil {
		return nil, translateErr(err)
	}
	return claims, nil
}

// KeyID returns the kid this verifier expects (empty = accept any/none).
func (v *Verifier) KeyID() string { return "" } // reserved for rotation; currently single-key

// translateErr maps golang-jwt's error tree onto our sentinels so middleware
// can branch on errors.Is without importing jwtv5 errors widely.
func translateErr(err error) error {
	switch {
	case errors.Is(err, jwtv5.ErrTokenExpired):
		return fmt.Errorf("%w: %v", ErrExpired, err)
	case errors.Is(err, jwtv5.ErrTokenNotValidYet):
		return fmt.Errorf("%w: %v", ErrNotYetValid, err)
	case errors.Is(err, jwtv5.ErrTokenSignatureInvalid):
		return fmt.Errorf("%w: %v", ErrBadSignature, err)
	case errors.Is(err, jwtv5.ErrTokenInvalidIssuer):
		return fmt.Errorf("%w: %v", ErrWrongIssuer, err)
	case errors.Is(err, jwtv5.ErrTokenInvalidAudience):
		return fmt.Errorf("%w: %v", ErrWrongAudience, err)
	default:
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
}

// randomID returns n random bytes hex-encoded (2n chars).
func randomID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
