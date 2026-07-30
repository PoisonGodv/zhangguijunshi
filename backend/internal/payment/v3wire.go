package payment

// v3wire.go implements the real WeChat Pay V3 HTTP client constructor and key
// loading (CONTRACT §15.5.8). The *V3Client built here is the production
// implementation of all three anti-corruption ports — SubMerchantApplier +
// Preparer + NotifyParser — wiring the already-tested pure signing primitives
// (sign.go) to a real *http.Client. dev/test never builds a V3Client: the main
// thread keeps FakeWxPay when cfg is not Configured().
//
// All V3 HTTP work is plain stdlib (net/http + crypto + encoding); no
// third-party WeChat SDK is imported (§15.5.8).

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// v3DefaultBaseURL is the official WeChat Pay V3 gateway used when
// cfg.BaseURL is empty.
const v3DefaultBaseURL = "https://api.mch.weixin.qq.com"

// v3DefaultTimeout is the outbound HTTP timeout used when cfg.Timeout <= 0.
const v3DefaultTimeout = 10 * time.Second

// V3Client is the real WeChat Pay V3 service-provider client. It signs every
// outbound request with the merchant private key (SignV3Authorization), verifies
// notify signatures with the platform certificate (fetched + cached on first
// use) and AES-256-GCM-decrypts notify resource bodies with the APIv3 key.
//
// A single *V3Client satisfies SubMerchantApplier, Preparer and NotifyParser;
// the main thread wires one instance into all three ports of the payment
// Service. The client owns no funds and never persists state — it is a pure
// transport anti-corruption layer (反二清, CONTRACT §15.5.3).
type V3Client struct {
	cfg     config.PaymentConfig
	priv    *rsa.PrivateKey
	hc      *http.Client
	baseURL string

	// certCache lazily holds platform certificates (public keys) keyed by the
	// Wechatpay-Serial the notify carries. See v3cert.go.
	certCache *platformCertCache

	// AppID is the service-provider appid (mini-program / 公众号). PaymentConfig
	// does not carry it today, so the wiring layer sets this field after
	// construction; Prepay needs it both for the outbound prepay body and to
	// build the JSAPI lift-off params (appId / paySign). Empty AppID yields an
	// invalid JSAPI payload — the caller MUST populate it in production.
	AppID string
}

// Compile-time assertions that *V3Client satisfies all three V3 ports. This
// guards signature drift in PrepayInput / ApplySubMerchantInput / NotifyResult
// (the ports are declared in ports.go).
var (
	_ SubMerchantApplier = (*V3Client)(nil)
	_ Preparer           = (*V3Client)(nil)
	_ NotifyParser       = (*V3Client)(nil)
)

// NewV3Client constructs a real V3 client from cfg.
//
//   - cfg MUST be Configured() (MchID + APiv3Key + CertSerial + a private key);
//     otherwise the wiring must keep FakeWxPay (dev/test never networks).
//   - The merchant private key is loaded from PrivateKeyPEM (inline) first,
//     otherwise from PrivateKeyFile (path); PEM wins when both are set. Both
//     PKCS#1 ("BEGIN RSA PRIVATE KEY") and PKCS#8 ("BEGIN PRIVATE KEY") encodings
//     are accepted.
//   - hc nil → a fresh *http.Client with Timeout=cfg.Timeout (default 10s).
//   - baseURL empty → the official V3 gateway.
//
// The returned client is safe for concurrent use: the only mutable state is the
// platform-cert cache, which is guarded by a sync.Mutex.
func NewV3Client(cfg config.PaymentConfig, hc *http.Client) (*V3Client, error) {
	if !cfg.Configured() {
		return nil, errors.New("payment: v3 client config incomplete (mch_id/api_v3_key/cert_serial/private key required)")
	}
	priv, err := loadPrivateKey(cfg)
	if err != nil {
		return nil, err
	}
	if hc == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = v3DefaultTimeout
		}
		hc = &http.Client{Timeout: timeout}
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = v3DefaultBaseURL
	}
	return &V3Client{
		cfg:       cfg,
		priv:      priv,
		hc:        hc,
		baseURL:   baseURL,
		certCache: newPlatformCertCache(),
	}, nil
}

// loadPrivateKey decodes the merchant API private key from cfg. PEM (inline)
// wins over file path; both PKCS#1 and PKCS#8 are tried so a key exported by
// either common toolchain loads.
func loadPrivateKey(cfg config.PaymentConfig) (*rsa.PrivateKey, error) {
	pemStr := cfg.PrivateKeyPEM
	if pemStr == "" {
		if cfg.PrivateKeyFile == "" {
			// Unreachable when cfg.Configured() held, but guard defensively.
			return nil, errors.New("payment: no private key source")
		}
		b, err := os.ReadFile(cfg.PrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("payment: read private key file %q: %w", cfg.PrivateKeyFile, err)
		}
		pemStr = string(b)
	}
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("payment: private key PEM decode failed")
	}
	// PKCS#1 ("RSA PRIVATE KEY") is the common WeChat-issued shape; fall back to
	// PKCS#8 for keys re-encoded by openssl/kmstool.
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	kAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("payment: parse private key (tried PKCS1/PKCS8): %w", err)
	}
	k, ok := kAny.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("payment: private key is not RSA")
	}
	return k, nil
}
