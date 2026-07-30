package payment

// v3client_test.go verifies the real WeChat Pay V3 client (CONTRACT §15.5.8):
// NewV3Client config/key loading, Apply/Query/Prepay against an httptest fake
// upstream (asserting the signed Authorization header, method, path and key
// request fields, and parsing canned responses), the AES-256-GCM resource
// decryptor with a known vector, platform-cert PEM parsing, and the full notify
// verify→decrypt chain (genuine signature accepted; tampered body / stale
// timestamp → ErrSignInvalid).
//
// The fixed merchant key pair (testPrivPEM/testPubPEM) and mustTestKeys come
// from sign_test.go (same package). The platform key pair is generated fresh
// per test; its self-signed cert is served encrypted from the fake
// /v3/certificates endpoint exactly as the real gateway serves it.

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhangguijunshi/backend/pkg/config"
)

// testAPIv3Key is a fixed 32-byte APIv3 key (AES-256) used for resource / cert
// encryption in the notify + cert tests.
const testAPIv3Key = "0123456789abcdef0123456789abcdef"

// newTestV3Client builds a V3Client whose baseURL points at the given (fake)
// upstream, using the fixed merchant private key + a 32-byte APIv3 key.
func newTestV3Client(t *testing.T, url string) *V3Client {
	t.Helper()
	cfg := config.PaymentConfig{
		MchID:         "1900000109",
		APiv3Key:      testAPIv3Key,
		CertSerial:    "MCHSERIAL1",
		PrivateKeyPEM: testPrivPEM,
		BaseURL:       url,
		NotifyURL:     "https://example.com/notify",
	}
	c, err := NewV3Client(cfg, &http.Client{Timeout: 5 * time.Second})
	require.NoError(t, err)
	c.AppID = "wxTESTAPPID"
	return c
}

// recordedRequest captures what the fake upstream received.
type recordedRequest struct {
	method string
	path   string
	auth   string
	body   []byte
}

// fakeUpstream returns an httptest server that records the last request into rc
// and dispatches to handlers keyed by "METHOD path".
func fakeUpstream(t *testing.T, rc *recordedRequest, handlers map[string]func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rc.method = r.Method
		rc.path = r.URL.Path
		rc.auth = r.Header.Get("Authorization")
		rc.body = body
		key := r.Method + " " + r.URL.Path
		h, ok := handlers[key]
		if !ok {
			// fallback: match by path prefix for templated routes (e.g. applyment id).
			for k, hh := range handlers {
				if strings.HasSuffix(k, "*") && strings.HasPrefix(key, strings.TrimSuffix(k, "*")) {
					h = hh
					ok = true
					break
				}
			}
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h(w, r)
	}))
}

func TestNewV3Client_RejectsIncompleteConfig(t *testing.T) {
	// Missing APiv3Key → not Configured → error, no network.
	_, err := NewV3Client(config.PaymentConfig{MchID: "x", CertSerial: "s", PrivateKeyPEM: testPrivPEM}, nil)
	if err == nil {
		t.Fatal("expected error for incomplete config")
	}
}

func TestNewV3Client_RejectsBadPEM(t *testing.T) {
	_, err := NewV3Client(config.PaymentConfig{
		MchID: "x", APiv3Key: testAPIv3Key, CertSerial: "s", PrivateKeyPEM: "not a pem",
	}, nil)
	if err == nil {
		t.Fatal("expected error for bad PEM")
	}
}

func TestNewV3Client_LoadsKeyAndDefaults(t *testing.T) {
	c, err := NewV3Client(config.PaymentConfig{
		MchID: "x", APiv3Key: testAPIv3Key, CertSerial: "s", PrivateKeyPEM: testPrivPEM,
	}, nil)
	require.NoError(t, err)
	if c.priv == nil {
		t.Fatal("private key not loaded")
	}
	if c.baseURL != v3DefaultBaseURL {
		t.Fatalf("baseURL = %s, want default %s", c.baseURL, v3DefaultBaseURL)
	}
	if c.hc == nil {
		t.Fatal("http client not defaulted")
	}
}

func TestV3Client_ApplySignsAndParses(t *testing.T) {
	var rc recordedRequest
	srv := fakeUpstream(t, &rc, map[string]func(http.ResponseWriter, *http.Request){
		"POST " + v3PathApplyments: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"applyment_id":"APPLYMENT_20261"}`)
		},
	})
	defer srv.Close()

	c := newTestV3Client(t, srv.URL)
	id, err := c.Apply(context.Background(), ApplySubMerchantInput{
		TenantID: 7, StoreID: 3, Provider: ProviderWxPay,
		BusinessName: "李记杂货", ContactName: "李四", ContactPhone: "13800001111",
		LicenseNo: "91110000MA00ABCDEF", LicenseType: "enterprise",
	})
	require.NoError(t, err)
	if id != "APPLYMENT_20261" {
		t.Fatalf("applyment_id = %s", id)
	}

	// Outbound request: POST to the applyments path with a signed Authorization.
	if rc.method != http.MethodPost || rc.path != v3PathApplyments {
		t.Fatalf("request = %s %s, want POST %s", rc.method, rc.path, v3PathApplyments)
	}
	if !strings.HasPrefix(rc.auth, v3SignAlgorithm+" ") {
		t.Fatalf("Authorization not signed with V3 scheme: %q", rc.auth)
	}
	// Key fields carried into the applyment body.
	var got v3ApplyBody
	require.NoError(t, json.Unmarshal(rc.body, &got))
	assert.Contains(t, got.OutRequestNo, "zg_apply_7_3_")
	assert.Equal(t, "91110000MA00ABCDEF", got.BusinessCode)
	assert.Equal(t, "李四", got.ContactInfo.ContactName)
	assert.Equal(t, "13800001111", got.ContactInfo.ContactPhone)
	assert.Equal(t, "李记杂货", got.BusinessInfo.MerchantShortname)
}

func TestV3Client_QueryMapsState(t *testing.T) {
	var rc recordedRequest
	srv := fakeUpstream(t, &rc, map[string]func(http.ResponseWriter, *http.Request){
		"GET *": func(w http.ResponseWriter, r *http.Request) {
			// Echo a FINISH state + sub_mch_id.
			fmt.Fprint(w, `{"applyment_state":"FINISH","sub_mch_id":"1234000001"}`)
		},
	})
	defer srv.Close()

	c := newTestV3Client(t, srv.URL)
	status, subMchID, err := c.Query(context.Background(), 7, "APPLYMENT_20261")
	require.NoError(t, err)
	assert.Equal(t, SubMerchantActive, status)
	assert.Equal(t, "1234000001", subMchID)
	// GET hits /v3/ecommerce/applyments/{id}.
	wantPath := v3PathApplyments + "APPLYMENT_20261"
	if rc.path != wantPath {
		t.Fatalf("query path = %s, want %s", rc.path, wantPath)
	}
	if !strings.HasPrefix(rc.auth, v3SignAlgorithm+" ") {
		t.Fatalf("query Authorization not signed: %q", rc.auth)
	}
}

func TestV3Client_QueryRejectedState(t *testing.T) {
	srv := fakeUpstream(t, &recordedRequest{}, map[string]func(http.ResponseWriter, *http.Request){
		"GET *": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"applyment_state":"REJECTED","sub_mch_id":""}`)
		},
	})
	defer srv.Close()
	c := newTestV3Client(t, srv.URL)
	status, _, err := c.Query(context.Background(), 1, "X")
	require.NoError(t, err)
	assert.Equal(t, SubMerchantRejected, status)
}

func TestV3Client_PrepaySignsAndBuildsJSAPI(t *testing.T) {
	var rc recordedRequest
	srv := fakeUpstream(t, &rc, map[string]func(http.ResponseWriter, *http.Request){
		"POST " + v3PathPayJSAPI: func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"prepay_id":"wx_prepay_001"}`)
		},
	})
	defer srv.Close()

	c := newTestV3Client(t, srv.URL)
	res, err := c.Prepay(context.Background(), PrepayInput{
		SubMerchantID: 1234000001,
		OutTradeNo:    "o-trade-1",
		AmountCents:   9900,
		Description:   "test order",
		OpenID:        "oOpenIdABC",
	})
	require.NoError(t, err)

	// JSAPI lift-off params.
	assert.Equal(t, "wx_prepay_001", res.PrepayID)
	assert.Equal(t, "prepay_id=wx_prepay_001", res.Package)
	assert.Equal(t, "RSA", res.SignType)
	assert.NotEmpty(t, res.TimeStamp)
	assert.NotEmpty(t, res.NonceStr)
	assert.NotEmpty(t, res.PaySign, "paySign must be a fresh RSA signature")

	// Outbound body: appid + sub_mchid (int64 formatted) + amount + payer.openid.
	var got v3PrepayBody
	require.NoError(t, json.Unmarshal(rc.body, &got))
	assert.Equal(t, "wxTESTAPPID", got.AppID)
	assert.Equal(t, "1234000001", got.SubMchID)
	assert.Equal(t, "o-trade-1", got.OutTradeNo)
	assert.Equal(t, int64(9900), got.Amount.Total)
	require.NotNil(t, got.Payer)
	assert.Equal(t, "oOpenIdABC", got.Payer.OpenID)
	// Signed request to the JSAPI path.
	if rc.method != http.MethodPost || rc.path != v3PathPayJSAPI {
		t.Fatalf("request = %s %s", rc.method, rc.path)
	}
	if !strings.HasPrefix(rc.auth, v3SignAlgorithm+" ") {
		t.Fatalf("Authorization not signed: %q", rc.auth)
	}

	// paySign verifies against the matching public key over the JSAPI message.
	_, pub := mustTestKeys(t)
	msg := "wxTESTAPPID\n" + res.TimeStamp + "\n" + res.NonceStr + "\n" + res.Package + "\n"
	sig, err := base64.StdEncoding.DecodeString(res.PaySign)
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(msg))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("paySign does not verify: %v", err)
	}
}

func TestV3Client_PrepayNativeOmitsPayer(t *testing.T) {
	var rc recordedRequest
	srv := fakeUpstream(t, &rc, map[string]func(http.ResponseWriter, *http.Request){
		"POST " + v3PathPayJSAPI: func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"prepay_id":"wx_prepay_002"}`)
		},
	})
	defer srv.Close()
	c := newTestV3Client(t, srv.URL)
	_, err := c.Prepay(context.Background(), PrepayInput{SubMerchantID: 1, OutTradeNo: "n", AmountCents: 1})
	require.NoError(t, err)
	// No openid → payer field must be absent (Native 扫码 shape).
	if strings.Contains(string(rc.body), "\"payer\"") {
		t.Fatalf("payer should be omitted for Native prepay, body=%s", rc.body)
	}
}

func TestV3Client_Non2xxIsV3Error(t *testing.T) {
	srv := fakeUpstream(t, &recordedRequest{}, map[string]func(http.ResponseWriter, *http.Request){
		"POST " + v3PathApplyments: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"code":"SIGN_ERROR","message":"signature error"}`)
		},
	})
	defer srv.Close()
	c := newTestV3Client(t, srv.URL)
	_, err := c.Apply(context.Background(), ApplySubMerchantInput{TenantID: 1, StoreID: 1})
	if err == nil {
		t.Fatal("expected V3 error")
	}
	if !strings.Contains(err.Error(), "SIGN_ERROR") {
		t.Fatalf("error should carry V3 code, got %v", err)
	}
}

func TestAESGCMDecrypt_KnownVector(t *testing.T) {
	// Encrypt a known plaintext with a known key/nonce/ad, then assert our
	// decryptor recovers it verbatim.
	key := []byte(testAPIv3Key)
	nonce := "abcdefghijkl" // 12 bytes (GCM standard nonce length)
	ad := "transaction"
	plain := []byte(`{"out_trade_no":"o1","transaction_id":"t1"}`)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	ciphertext := gcm.Seal(nil, []byte(nonce), plain, []byte(ad))

	got, err := aesGCMDecrypt(key, nonce, ad, ciphertext)
	require.NoError(t, err)
	assert.Equal(t, plain, got)
}

func TestAESGCMDecrypt_WrongKeyLength(t *testing.T) {
	// A 16-byte key (AES-128) is rejected — APIv3 mandates 32 bytes.
	_, err := aesGCMDecrypt([]byte("0123456789abcdef"), "n", "ad", []byte("x"))
	if err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestDecryptPlatformCert_ParsesSelfSigned(t *testing.T) {
	// Generate a platform key pair, self-sign a cert, PEM-encode it, then
	// AES-GCM-encrypt the PEM exactly as /v3/certificates serves it. The decryptor
	// must recover the cert's RSA public key.
	platPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	cert, certPEM := selfSignedCert(t, platPriv)

	nonce := "certnonce012" // 12 bytes
	ad := "certificate"
	ciphertext := gcmEncrypt(t, []byte(testAPIv3Key), nonce, ad, certPEM)

	pub, err := decryptPlatformCert(testAPIv3Key, nonce, ad, base64.StdEncoding.EncodeToString(ciphertext))
	require.NoError(t, err)
	// Recovered public key matches the cert's key.
	assert.Equal(t, cert.PublicKey.(*rsa.PublicKey).N, pub.N)
	assert.Equal(t, cert.PublicKey.(*rsa.PublicKey).E, pub.E)
}

// notifyFixture bundles everything ParseNotify needs: the platform cert served
// from /v3/certificates plus helpers to build a signed notify envelope.
type notifyFixture struct {
	platPriv *rsa.PrivateKey
	serial   string
	certSrv  *httptest.Server
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	platPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	cert, certPEM := selfSignedCert(t, platPriv)

	// Serve /v3/certificates with the cert PEM, AES-GCM encrypted.
	nonce := "certnonce012" // 12 bytes
	ad := "certificate"
	ciphertext := gcmEncrypt(t, []byte(testAPIv3Key), nonce, ad, certPEM)
	certsBody, _ := json.Marshal(map[string]any{
		"data": []map[string]any{
			{
				"serial_no": cert.SerialNumber.String(),
				"encrypt_certificate": map[string]any{
					"nonce":           nonce,
					"associated_data": ad,
					"ciphertext":      base64.StdEncoding.EncodeToString(ciphertext),
				},
			},
		},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != v3PathCerts {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(certsBody)
	}))
	t.Cleanup(srv.Close)
	return &notifyFixture{platPriv: platPriv, serial: cert.SerialNumber.String(), certSrv: srv}
}

// buildSignedNotify encrypts tx as the notify resource and signs the envelope
// with the platform private key. Returns the headers + body to feed ParseNotify.
func (f *notifyFixture) buildSignedNotify(t *testing.T, tx map[string]any, ts int64) (map[string]string, []byte) {
	t.Helper()
	plain, _ := json.Marshal(tx)
	rNonce := "resnonce1234" // 12 bytes
	rAD := "transaction"
	rCiphertext := gcmEncrypt(t, []byte(testAPIv3Key), rNonce, rAD, plain)
	envelope := map[string]any{
		"id":         "EV-TEST",
		"event_type": "TRANSACTION.SUCCESS",
		"resource": map[string]any{
			"algorithm":       "AEAD_AES_256_GCM",
			"ciphertext":      base64.StdEncoding.EncodeToString(rCiphertext),
			"associated_data": rAD,
			"nonce":           rNonce,
		},
	}
	body, _ := json.Marshal(envelope)
	nonce := "notifynonce"
	msg := fmt.Sprintf("%d\n%s\n%s\n", ts, nonce, string(body))
	digest := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.platPriv, crypto.SHA256, digest[:])
	require.NoError(t, err)
	headers := map[string]string{
		v3HeaderTimestamp: fmt.Sprintf("%d", ts),
		v3HeaderNonce:     nonce,
		v3HeaderSerial:    f.serial,
		v3HeaderSignature: base64.StdEncoding.EncodeToString(sig),
	}
	return headers, body
}

func TestV3Client_ParseNotify_FullChain(t *testing.T) {
	f := newNotifyFixture(t)
	c := newTestV3Client(t, f.certSrv.URL)

	successTime := time.Now().UTC().Format(time.RFC3339)
	tx := map[string]any{
		"transaction_id": "wx-tx-full-1",
		"out_trade_no":   "trade-full-1",
		"trade_state":    "SUCCESS",
		"success_time":   successTime,
		"amount":         map[string]any{"payer_total": 9900, "total": 10000},
	}
	headers, body := f.buildSignedNotify(t, tx, time.Now().Unix())

	res, err := c.ParseNotify(context.Background(), headers, body)
	require.NoError(t, err)
	assert.Equal(t, "trade-full-1", res.OutTradeNo)
	assert.Equal(t, "wx-tx-full-1", res.ProviderTxID)
	assert.Equal(t, int64(9900), res.AmountCents, "payer_total (实付) preferred over total")
	assert.Equal(t, PaymentStatusSuccess, res.Status)
	assert.False(t, res.PaidAt.IsZero(), "PaidAt parsed from success_time")
	// RawPayload echoes the signed envelope (audit ledger).
	assert.Equal(t, body, res.RawPayload)
}

func TestV3Client_ParseNotify_TamperedBody(t *testing.T) {
	f := newNotifyFixture(t)
	c := newTestV3Client(t, f.certSrv.URL)
	tx := map[string]any{
		"transaction_id": "wx-tx-2", "out_trade_no": "o2",
		"trade_state": "SUCCESS", "amount": map[string]any{"payer_total": 1},
	}
	headers, body := f.buildSignedNotify(t, tx, time.Now().Unix())
	// Tamper with the body AFTER signing → signature must not verify. The
	// envelope "id" field is plaintext in the body (the out_trade_no lives inside
	// the encrypted resource), so mutate a byte that is guaranteed present.
	tampered := []byte(strings.Replace(string(body), "EV-TEST", "EV-TESX", 1))
	_, err := c.ParseNotify(context.Background(), headers, tampered)
	if !errors.Is(err, ErrSignInvalid) {
		t.Fatalf("tampered body must yield ErrSignInvalid, got %v", err)
	}
}

func TestV3Client_ParseNotify_StaleTimestamp(t *testing.T) {
	f := newNotifyFixture(t)
	c := newTestV3Client(t, f.certSrv.URL)
	tx := map[string]any{
		"transaction_id": "wx-tx-3", "out_trade_no": "o3",
		"trade_state": "SUCCESS", "amount": map[string]any{"payer_total": 1},
	}
	stale := time.Now().Unix() - 600 // -10min, beyond ±5min skew
	headers, body := f.buildSignedNotify(t, tx, stale)
	_, err := c.ParseNotify(context.Background(), headers, body)
	if !errors.Is(err, ErrSignInvalid) {
		t.Fatalf("stale timestamp must yield ErrSignInvalid, got %v", err)
	}
}

func TestV3Client_ParseNotify_FailedTradeState(t *testing.T) {
	f := newNotifyFixture(t)
	c := newTestV3Client(t, f.certSrv.URL)
	tx := map[string]any{
		"transaction_id": "wx-tx-4", "out_trade_no": "o4",
		"trade_state": "PAYERROR", "amount": map[string]any{"total": 1},
	}
	headers, body := f.buildSignedNotify(t, tx, time.Now().Unix())
	res, err := c.ParseNotify(context.Background(), headers, body)
	require.NoError(t, err)
	assert.Equal(t, PaymentStatusFailed, res.Status)
	// No payer_total → falls back to total.
	assert.Equal(t, int64(1), res.AmountCents)
}

func TestV3Client_SatisfiesPorts(t *testing.T) {
	// Runtime mirror of the compile-time assertions in v3wire.go.
	c, err := NewV3Client(config.PaymentConfig{
		MchID: "x", APiv3Key: testAPIv3Key, CertSerial: "s", PrivateKeyPEM: testPrivPEM,
	}, nil)
	require.NoError(t, err)
	var (
		_ SubMerchantApplier = c
		_ Preparer           = c
		_ NotifyParser       = c
	)
}

// selfSignedCert generates a self-signed X.509 cert for priv and returns the
// parsed cert + its PEM encoding. Its SerialNumber is used as the
// Wechatpay-Serial in the notify test.
func selfSignedCert(t *testing.T, priv *rsa.PrivateKey) (*x509.Certificate, []byte) {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(0x2026072801),
		Subject:      pkix.Name{CommonName: "wechatpay-platform-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return cert, pemBytes
}

// gcmEncrypt is the test-side AES-256-GCM encryptor (mirror of aesGCMDecrypt).
func gcmEncrypt(t *testing.T, key []byte, nonce, ad string, plain []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	return gcm.Seal(nil, []byte(nonce), plain, []byte(ad))
}
