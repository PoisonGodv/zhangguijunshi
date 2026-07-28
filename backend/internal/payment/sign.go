package payment

// sign.go implements WeChat Pay V3 request signing and notify verification as
// pure stdlib-crypto functions (CONTRACT §15.5.8). No third-party WeChat SDK is
// imported. Both functions take the RSA key as an argument so they are fully
// deterministic under test (inject a fixed *rsa.PrivateKey / *rsa.PublicKey,
// nonce, timestamp, body) — no global state, no network, no time.Now inside
// the signing path.
//
// FakeWxPay.ParseNotify does NOT call VerifyV3Notify (it JSON-decodes the body
// directly); a real NotifyParser implementation calls VerifyV3Notify. Both
// live in this package.

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// V3 header / parameter constants (CONTRACT §15.3.1 sign note).
const (
	// v3SignAlgorithm is the WeChat Pay V3 Authorization scheme.
	v3SignAlgorithm = "WECHATPAY2-SHA256-RSA2048"

	// v3MaxSkewSeconds is the maximum tolerated clock skew for a notify
	// timestamp (±5 minutes, WeChat Pay V3 contract).
	v3MaxSkewSeconds = 5 * 60
)

// WeChat Pay V3 notify header names (case-insensitive in practice; we normalise
// to the canonical capitalisation WeChat sends).
const (
	v3HeaderTimestamp = "Wechatpay-Timestamp"
	v3HeaderNonce     = "Wechatpay-Nonce"
	v3HeaderSerial    = "Wechatpay-Serial"
	v3HeaderSignature = "Wechatpay-Signature"
)

// SignV3Authorization builds the WeChat Pay V3 request Authorization header
// (APIv3). The signed message is:
//
//	METHOD\nURL\ntimestamp\nnonce\nbody\n
//
// signed with SHA256withRSA using priv, then base64-encoded and packed into:
//
//	WECHATPAY2-SHA256-RSA2048 mchid="...",nonce_str="...",timestamp="...",
//	  serial_no="...",signature="..."
//
// A nil priv yields ("", error) — never a panic. The function is deterministic
// for fixed inputs (method/url/body/nonce/ts/mchID/serial/priv): there is no
// randomness in the message and the RSA signature is deterministic under PKCS#1
// v1.5 (which Go's rsa.SignPKCS1v15 implements deterministically).
func SignV3Authorization(method, url, body, nonce string, ts int64, mchID, serial string, priv *rsa.PrivateKey) (string, error) {
	if priv == nil {
		return "", errors.New("payment: sign requires a private key")
	}
	msg := strings.Join([]string{method, url, strconv.FormatInt(ts, 10), nonce, body}, "\n") + "\n"
	digest := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("payment: v3 sign: %w", err)
	}
	sigB64 := base64.StdEncoding.EncodeToString(sig)
	return fmt.Sprintf(
		`%s mchid="%s",nonce_str="%s",timestamp="%d",serial_no="%s",signature="%s"`,
		v3SignAlgorithm, mchID, nonce, ts, serial, sigB64,
	), nil
}

// VerifyV3Notify verifies a WeChat Pay V3 notify against the platform
// certificate public key pub. The signed message is:
//
//	timestamp\nnonce\nbody\n
//
// drawn from the Wechatpay-* headers. A notify is accepted only when all hold:
//   - pub is non-nil;
//   - all four Wechatpay-* headers are present;
//   - the timestamp is within ±v3MaxSkewSeconds of now (replay protection);
//   - the RSA-SHA256 signature over the message verifies under pub.
//
// Any failure returns ErrSignInvalid (no other error identity leaks). The
// signature-math itself is deterministic for fixed headers/body/pub; only the
// ±5min freshness check consults the wall clock (a runtime security check, not
// part of the signing path).
func VerifyV3Notify(headers map[string]string, body []byte, pub *rsa.PublicKey) error {
	if pub == nil {
		return ErrSignInvalid
	}
	tsStr := headerGet(headers, v3HeaderTimestamp)
	nonce := headerGet(headers, v3HeaderNonce)
	sigB64 := headerGet(headers, v3HeaderSignature)
	if headerGet(headers, v3HeaderSerial) == "" || tsStr == "" || nonce == "" || sigB64 == "" {
		return ErrSignInvalid
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return ErrSignInvalid
	}
	now := time.Now().Unix()
	if diff := now - ts; diff > v3MaxSkewSeconds || diff < -v3MaxSkewSeconds {
		return ErrSignInvalid
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return ErrSignInvalid
	}
	msg := tsStr + "\n" + nonce + "\n" + string(body) + "\n"
	digest := sha256.Sum256([]byte(msg))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return ErrSignInvalid
	}
	return nil
}

// headerGet returns the value for key, matching case-insensitively so a notify
// whose headers arrive with different capitalisation still verifies. The
// comparison is constant-time per byte to avoid header-name oracles.
func headerGet(headers map[string]string, key string) string {
	if v, ok := headers[key]; ok {
		return v
	}
	kl := strings.ToLower(key)
	for k, v := range headers {
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(k)), []byte(kl)) == 1 {
			return v
		}
	}
	return ""
}
