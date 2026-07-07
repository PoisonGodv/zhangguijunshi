// Package jwt issues and verifies the backend's self-signed access tokens
// (tech plan §6.2.4 / ARCH-02). Tokens use ES256 (ECDSA P-256 + SHA-256):
// signing is fast and tokens are small, the public key can be published to an
// upstream gateway for signature-only checks, and the private key never leaves
// the server. The header carries a `kid` for future key rotation.
//
// This package is a pure leaf: it takes primitives (a *ecdsa.PrivateKey, a
// key id, an issuer, a TTL) rather than *config.Config, so it stays unit-
// testable without the config stack. The wiring layer (cmd/api) bridges
// config → jwt and decides whether to load a PEM key or fall back to a
// deterministic dev key (see DevKey).
package jwt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
)

// DevSeed is the default seed for DevKey. It MUST NEVER be used outside local
// dev: tokens signed by a dev key are accepted only because the same process
// verifies them with the same derived public key.
const DevSeed = "zhanggui-junshi-dev-key-DO-NOT-USE-IN-PROD"

// ErrInvalidKey is returned when a PEM block does not contain a usable
// ECDSA P-256 key.
var ErrInvalidKey = errors.New("jwt: not a valid ECDSA P-256 key")

// DevKey deterministically derives an ECDSA P-256 private key from seed. The
// same seed always yields the same key, so a dev server emits a stable key
// pair across restarts (and tests are reproducible).
//
// Note: we deliberately do NOT call ecdsa.GenerateKey — the stdlib documents
// that its output "does not depend deterministically on the bytes read from
// rand" (it blinds with extra entropy for side-channel resistance). For a dev
// key the scalar D is derived directly from the seed via a SHA-256 counter PRG
// and reduced below the curve order, then the public point is computed. This
// is acceptable precisely because dev keys never protect anything — never use
// DevKey in staging or production.
func DevKey(seed string) (*ecdsa.PrivateKey, error) {
	if seed == "" {
		return nil, errors.New("jwt: dev seed must be non-empty")
	}
	curve := elliptic.P256()
	params := curve.Params()
	r := newSHA256Reader([]byte(seed))

	var d *big.Int
	for {
		buf := make([]byte, params.BitSize/8) // 32 bytes for P-256
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("jwt: derive dev key: %w", err)
		}
		d = new(big.Int).SetBytes(buf)
		if d.Sign() != 0 && d.Cmp(params.N) < 0 {
			break
		}
	}
	x, y := curve.ScalarBaseMult(d.Bytes())
	return &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y},
		D:         d,
	}, nil
}

// LoadPrivateKeyPEM parses a PEM-encoded ECDSA private key (SEC1 "EC PRIVATE
// KEY" or PKCS#8 "PRIVATE KEY") on the P-256 curve. It returns ErrInvalidKey
// for non-ECDSA keys or non-P-256 curves.
func LoadPrivateKeyPEM(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("jwt: %w: no PEM block", ErrInvalidKey)
	}

	var (
		key *ecdsa.PrivateKey
		err error
	)
	switch block.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		parsed, perr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if perr != nil {
			err = perr
			break
		}
		ek, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("jwt: %w: PKCS#8 key is not ECDSA", ErrInvalidKey)
		}
		key = ek
	default:
		return nil, fmt.Errorf("jwt: %w: unexpected PEM type %q", ErrInvalidKey, block.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("jwt: parse private key: %w", err)
	}
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("jwt: %w: curve is not P-256", ErrInvalidKey)
	}
	return key, nil
}

// LoadPublicKeyPEM parses a PEM-encoded ECDSA public key in SubjectPublicKeyInfo
// form (PEM type "PUBLIC KEY", as produced by `openssl ec -pubout`) on the
// P-256 curve. Other PEM types return ErrInvalidKey.
func LoadPublicKeyPEM(pemBytes []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("jwt: %w: no PEM block", ErrInvalidKey)
	}
	if block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("jwt: %w: unsupported PEM type %q (want PUBLIC KEY)", ErrInvalidKey, block.Type)
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("jwt: parse public key: %w", err)
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("jwt: %w: PKIX key is not ECDSA", ErrInvalidKey)
	}
	if pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("jwt: %w: curve is not P-256", ErrInvalidKey)
	}
	return pub, nil
}

// LoadPrivateKeyFile is a convenience wrapper around LoadPrivateKeyPEM that
// reads the key material from path.
func LoadPrivateKeyFile(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("jwt: read key file %q: %w", path, err)
	}
	return LoadPrivateKeyPEM(b)
}

// PublicKeyOf returns the ecdsa.PublicKey counterpart of a private key.
func PublicKeyOf(priv *ecdsa.PrivateKey) *ecdsa.PublicKey {
	return &priv.PublicKey
}

// sha256Reader is a deterministic io.Reader: the i-th 32-byte block it serves
// is SHA-256(seed || uint64(i)). It lets ecdsa.GenerateKey run with fully
// reproducible randomness.
type sha256Reader struct {
	seed []byte
	ctr  uint64
	buf  []byte
}

func newSHA256Reader(seed []byte) *sha256Reader {
	// Copy seed so callers cannot mutate the PRG state after construction.
	cp := make([]byte, len(seed))
	copy(cp, seed)
	return &sha256Reader{seed: cp}
}

func (r *sha256Reader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(r.buf) == 0 {
			var ctrBytes [8]byte
			binary.BigEndian.PutUint64(ctrBytes[:], r.ctr)
			r.ctr++
			h := sha256.New()
			h.Write(r.seed)
			h.Write(ctrBytes[:])
			r.buf = h.Sum(nil)
		}
		c := copy(p[n:], r.buf)
		r.buf = r.buf[c:]
		n += c
	}
	return n, nil
}
