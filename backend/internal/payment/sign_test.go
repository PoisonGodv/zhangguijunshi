package payment

// sign_test.go verifies the V3 signing primitives (CONTRACT §15.3.1 / §15.5.8):
// SignV3Authorization builds a deterministic SHA256withRSA Authorization header
// from a fixed injected private key, and VerifyV3Notify accepts a genuine
// signature but rejects a nil key, a missing header, a stale timestamp and a
// tampered body (each → ErrSignInvalid). The private/public key below is fixed
// (PEM-embedded) so the signature bytes are byte-deterministic across runs;
// rsa.SignPKCS1v15 is itself deterministic (no random salt).

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"
)

// testPrivPEM / testPubPEM are a fixed 2048-bit RSA key pair generated once and
// embedded so signing tests are byte-reproducible.
const testPrivPEM = `-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEA4JLomnD81VeQ0PRwm2qe6ow1TezUDdLTk81fHek3Rwo9xxaN
IUCiZ0jGTVh3tCgLHNXiX+m8Ih3kAcusHGW2MNfKcI86qrQrDl7Bn+kKi9zJPVk6
agw4Y9K1Hw8e4S7JpJl96AThYcl+jPvn+nuEBG5f3yr7m+rD1JSmkcjfuH6jFk9u
SlCBuY5ysdNRJvGYnoCYD8gr4sdxmqL7PZvjErfTMwSfcd8k1vFx+p3fjI7+7D8J
is3ZVA1k4Mpr24tK40XgPLKczzrO0Qh6peUwf+zCGuzwHV3g3Ft6VB/XhVuPeJBF
xJBm5cA5i7EJ8r9J3yfnnDTzYbIW9BVZL5+iLwIDAQABAoIBAGXGHjTztJlo1D/m
L2w3nypzfSrCA/35yBnpDkyrZvpCaWnizfW+aXs+T2Q5DhKuvQ6F3dASyMh7XFv1
9htueulauitVtcQr34FlLwGt5rIozmiHmgH6ZMnUcXHQc/GgNxDp4NX39D5RRxFU
jjaFqHgrnzYvsBRiifo6ndxeuQjOPZU6OlUw+cPyvbfPtp76CbdY9WX7oAmcb6X8
la1ycyTY8mg8jLHZV3SEjcgqte5TkRbwnbXH3CSh3lZrjFH/qYyke9M3mr6QuGDi
FnS4dck+ZrnGF5qrYFa1A8RnsY9QpU36aQwj0EGvTp+bQYlHVelMSPcnKGNMRnZm
Fl7JXEUCgYEA/vNTzZNoPohcSquUU93LHcJcqioHBqDISV4AZ+UsrDJi5FwFy5JI
GI2CtBxhppG5GfaTJjtPwTk5VjfNfVneBRkC9W90KJ4dpsNny+I6xR0PZYD7Bpcn
RWUs7Cn/Cn7D1wCNfR/ylxXv1KTmSv1nTR2MdiIE9zap+ocLYD5WQ0MCgYEA4X+R
1TzFsvkqYnEa/tTPDmCZ/b2LeygS+sHhTle7GT+4wZPuxBizphejaYIfz+9hYNZe
TbagjzY5luz+9Z8XNFVM5hKAViJ0+bxwC30ucUjOgsMc2uLktcwLljLSNJDm3fZI
cgWosu2gwI1buQTyJc4QmH05hr4HkNZ71z3oGKUCgYAK9meKFWaMGczuIQaVvJm5
Q9w8LOZ0cIFsNjCKRx5uoRQDgbl2EWosNBmEqRvBz85HwYR+RlqdklpWzLt3Ql/7
lCG+b/ZssK/RteFGsQmZ5C//S1Z/6Av2Pe16kiu3RdAh9JIg6TjDKlxPpm+IlfDV
LgoWIkxYXO9eKGRi1QtchQKBgQDOIxYnPmTj1Q6/6vFonxWg6QIciRdmwB2JiY7z
VRO+Yxf8LcWJEaO4Y8eIfeGi7guwwIZ4uEhSYpmGtKPl2UcXGhsv/x2+n8UbPv2W
Ixs/h/TwaHiKR/wXjK3S4iZy0zuZELjtMusilkPrgc2ldzX9BUD6hQ7/c9vqtYeE
Hh172QKBgD5NgJAWhlbWYfDi9KYZBtp4GV/a4DQyaBYahsy2kfmRPbHyk7XEiyOL
LndvdlnPwqHQtg0TlHs8yr4FdwwSUc41bLjE7FylzevrmtCaroBR8Hgwyyq1/FzQ
4A3AKS8IBiff0GIeYpzAVL2jNGA/0F5Th39cney7Hi9WjU2a1m/0
-----END RSA PRIVATE KEY-----`

const testPubPEM = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA4JLomnD81VeQ0PRwm2qe
6ow1TezUDdLTk81fHek3Rwo9xxaNIUCiZ0jGTVh3tCgLHNXiX+m8Ih3kAcusHGW2
MNfKcI86qrQrDl7Bn+kKi9zJPVk6agw4Y9K1Hw8e4S7JpJl96AThYcl+jPvn+nuE
BG5f3yr7m+rD1JSmkcjfuH6jFk9uSlCBuY5ysdNRJvGYnoCYD8gr4sdxmqL7PZvj
ErfTMwSfcd8k1vFx+p3fjI7+7D8Jis3ZVA1k4Mpr24tK40XgPLKczzrO0Qh6peUw
f+zCGuzwHV3g3Ft6VB/XhVuPeJBFxJBm5cA5i7EJ8r9J3yfnnDTzYbIW9BVZL5+i
LwIDAQAB
-----END PUBLIC KEY-----`

// mustTestKeys decodes the fixed PEM pair, failing the test on any error.
func mustTestKeys(t *testing.T) (*rsa.PrivateKey, *rsa.PublicKey) {
	t.Helper()
	privBlock, _ := pem.Decode([]byte(testPrivPEM))
	if privBlock == nil {
		t.Fatal("decode private pem: nil block")
	}
	priv, err := x509.ParsePKCS1PrivateKey(privBlock.Bytes)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	pubBlock, _ := pem.Decode([]byte(testPubPEM))
	if pubBlock == nil {
		t.Fatal("decode public pem: nil block")
	}
	pubAny, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	if err != nil {
		t.Fatalf("parse public key: %v", err)
	}
	pub, ok := pubAny.(*rsa.PublicKey)
	if !ok {
		t.Fatal("public key is not RSA")
	}
	return priv, pub
}

func TestSignV3Authorization_DeterministicAndVerifies(t *testing.T) {
	priv, pub := mustTestKeys(t)
	const (
		method = "POST"
		uri    = "/v3/pay/transactions/jsapi"
		body   = `{"out_trade_no":"o1"}`
		nonce  = "abc123"
		mchID  = "1900000109"
		serial = "SERIALXYZ"
	)
	ts := time.Now().Unix()

	auth1, err := SignV3Authorization(method, uri, body, nonce, ts, mchID, serial, priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// Determinism: same inputs → identical bytes (rsa.SignPKCS1v15 has no salt).
	auth2, _ := SignV3Authorization(method, uri, body, nonce, ts, mchID, serial, priv)
	if auth1 != auth2 {
		t.Fatalf("sign not deterministic:\n%s\n%s", auth1, auth2)
	}
	// Shape: scheme + required fields.
	if !strings.HasPrefix(auth1, v3SignAlgorithm+" ") {
		t.Fatalf("auth missing scheme: %s", auth1)
	}
	for _, want := range []string{`mchid="` + mchID, `nonce_str="` + nonce, `timestamp="`, `serial_no="` + serial, `signature="`} {
		if !strings.Contains(auth1, want) {
			t.Fatalf("auth missing %q in %s", want, auth1)
		}
	}

	// A notify carrying the same timestamp/body verifies under the matching pub.
	hdrs := map[string]string{
		v3HeaderTimestamp: itoa(ts),
		v3HeaderNonce:     nonce,
		v3HeaderSerial:    serial,
	}
	// Extract signature from the request auth to reuse for the notify-style
	// message (timestamp\nnonce\nbody\n). The notify uses a DIFFERENT message
	// shape than the request, so we sign the notify message directly here.
	notifyMsg := itoa(ts) + "\n" + nonce + "\n" + body + "\n"
	sig := signRaw(t, priv, notifyMsg)
	hdrs[v3HeaderSignature] = sig
	if err := VerifyV3Notify(hdrs, []byte(body), pub); err != nil {
		t.Fatalf("verify genuine signature: %v", err)
	}
}

func TestSignV3Authorization_NilPrivateKey(t *testing.T) {
	out, err := SignV3Authorization("POST", "/x", "", "n", 1, "m", "s", nil)
	if err == nil {
		t.Fatal("expected error for nil private key")
	}
	if out != "" {
		t.Fatalf("expected empty auth, got %q", out)
	}
}

func TestVerifyV3Notify_TamperedBodyRejected(t *testing.T) {
	priv, pub := mustTestKeys(t)
	ts := time.Now().Unix()
	body := `{"out_trade_no":"o1"}`
	hdrs := signedHeaders(t, priv, ts, "n1", body)
	// Tamper with the body after signing → verification must fail.
	if err := VerifyV3Notify(hdrs, []byte(`{"out_trade_no":"evil"}`), pub); !errors.Is(err, ErrSignInvalid) {
		t.Fatalf("expected ErrSignInvalid for tampered body, got %v", err)
	}
}

func TestVerifyV3Notify_StaleTimestampRejected(t *testing.T) {
	priv, pub := mustTestKeys(t)
	stale := time.Now().Unix() - 600 // -10min, beyond ±5min skew
	body := `{"x":1}`
	hdrs := signedHeadersAt(t, priv, stale, "n1", body)
	if err := VerifyV3Notify(hdrs, []byte(body), pub); !errors.Is(err, ErrSignInvalid) {
		t.Fatalf("expected ErrSignInvalid for stale timestamp, got %v", err)
	}
}

func TestVerifyV3Notify_MissingHeaderRejected(t *testing.T) {
	_, pub := mustTestKeys(t)
	ts := time.Now().Unix()
	body := `{"x":1}`
	// No signature header at all.
	hdrs := map[string]string{
		v3HeaderTimestamp: itoa(ts),
		v3HeaderNonce:     "n1",
		v3HeaderSerial:    "s1",
	}
	if err := VerifyV3Notify(hdrs, []byte(body), pub); !errors.Is(err, ErrSignInvalid) {
		t.Fatalf("expected ErrSignInvalid for missing signature, got %v", err)
	}
}

func TestVerifyV3Notify_NilPublicKeyRejected(t *testing.T) {
	ts := time.Now().Unix()
	hdrs := map[string]string{
		v3HeaderTimestamp: itoa(ts),
		v3HeaderNonce:     "n1",
		v3HeaderSerial:    "s1",
		v3HeaderSignature: "sig",
	}
	if err := VerifyV3Notify(hdrs, []byte("{}"), nil); !errors.Is(err, ErrSignInvalid) {
		t.Fatalf("expected ErrSignInvalid for nil public key, got %v", err)
	}
}

func TestVerifyV3Notify_HeaderCaseInsensitive(t *testing.T) {
	priv, pub := mustTestKeys(t)
	ts := time.Now().Unix()
	body := `{"x":1}`
	hdrs := signedHeaders(t, priv, ts, "n1", body)
	// Re-key headers in lowercase to confirm case-insensitive lookup.
	lower := make(map[string]string, len(hdrs))
	for k, v := range hdrs {
		lower[""+strings.ToLower(k)] = v
	}
	if err := VerifyV3Notify(lower, []byte(body), pub); err != nil {
		t.Fatalf("expected verify to pass with lowercased headers, got %v", err)
	}
}

// itoa formats an int64 timestamp (avoids strconv import noise at call sites).
func itoa(ts int64) string {
	const digits = "0123456789"
	if ts == 0 {
		return "0"
	}
	neg := ts < 0
	if neg {
		ts = -ts
	}
	var buf [20]byte
	i := len(buf)
	for ts > 0 {
		i--
		buf[i] = digits[ts%10]
		ts /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// signRaw signs message with SHA256withRSA and returns base64.
func signRaw(t *testing.T, priv *rsa.PrivateKey, message string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(message))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign raw: %v", err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

// signedHeaders builds the Wechatpay-* headers carrying a valid V3 signature
// over timestamp\nnonce\nbody\n at the given timestamp.
func signedHeaders(t *testing.T, priv *rsa.PrivateKey, ts int64, nonce, body string) map[string]string {
	t.Helper()
	return signedHeadersAt(t, priv, ts, nonce, body)
}

func signedHeadersAt(t *testing.T, priv *rsa.PrivateKey, ts int64, nonce, body string) map[string]string {
	t.Helper()
	msg := itoa(ts) + "\n" + nonce + "\n" + body + "\n"
	return map[string]string{
		v3HeaderTimestamp: itoa(ts),
		v3HeaderNonce:     nonce,
		v3HeaderSerial:    "SERIALXYZ",
		v3HeaderSignature: signRaw(t, priv, msg),
	}
}
