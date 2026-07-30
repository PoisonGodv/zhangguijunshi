package payment

// v3cert.go implements the WeChat Pay V3 platform-certificate cache
// (CONTRACT §15.5.8). Notify verification needs the platform public key
// matching the Wechatpay-Serial the notify carries; that key is itself served
// AES-256-GCM-encrypted (with the APIv3 key) from GET /v3/certificates. This
// file lazily fetches, decrypts and caches platform certs by serial.
//
// The same AES-256-GCM primitive decrypts both the platform cert and the notify
// resource body (aesGCMDecrypt); both use the 32-byte APIv3 key as the symmetric
// key. WeChat rotates platform certs periodically, so the cache is keyed by
// serial and refreshed lazily on a miss.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"sync"
)

// platformCertCache holds platform RSA public keys keyed by certificate serial
// number. A zero-value map is not usable; construct via newPlatformCertCache.
type platformCertCache struct {
	mu       sync.Mutex
	bySerial map[string]*rsa.PublicKey
}

func newPlatformCertCache() *platformCertCache {
	return &platformCertCache{bySerial: make(map[string]*rsa.PublicKey)}
}

// get returns the platform public key for serial, fetching + decrypting all
// platform certificates from /v3/certificates on cache miss. The HTTP fetch
// happens WITHOUT holding the mutex (it would risk blocking the whole client on
// a slow upstream); concurrent misses may fetch redundantly and last-writer-wins
// into the map — an acceptable trade-off, not a correctness risk. Returns an
// error if the serial is absent even after a fresh fetch (e.g. WeChat signed
// with a cert the gateway no longer serves — treat as verification failure).
func (cc *platformCertCache) get(ctx context.Context, c *V3Client, serial string) (*rsa.PublicKey, error) {
	cc.mu.Lock()
	pub, ok := cc.bySerial[serial]
	cc.mu.Unlock()
	if ok {
		return pub, nil
	}
	if err := cc.fetchAndCache(ctx, c); err != nil {
		return nil, err
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if pub, ok := cc.bySerial[serial]; ok {
		return pub, nil
	}
	return nil, fmt.Errorf("payment: platform cert serial %q not found after fetch", serial)
}

// fetchAndCache GETs /v3/certificates (signed like every other request via do),
// AES-256-GCM-decrypts each encrypted_certificate with the APIv3 key and stores
// the parsed RSA public key under its serial_no.
func (cc *platformCertCache) fetchAndCache(ctx context.Context, c *V3Client) error {
	data, err := c.do(ctx, http.MethodGet, v3PathCerts, nil)
	if err != nil {
		return fmt.Errorf("payment: fetch platform certificates: %w", err)
	}
	var resp struct {
		Data []struct {
			SerialNo           string `json:"serial_no"`
			EncryptCertificate struct {
				Nonce          string `json:"nonce"`
				AssociatedData string `json:"associated_data"`
				Ciphertext     string `json:"ciphertext"`
			} `json:"encrypt_certificate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("payment: parse platform certificates: %w", err)
	}
	if len(resp.Data) == 0 {
		return fmt.Errorf("payment: platform certificates endpoint returned no certs")
	}
	// Parse outside the lock (CPU work); only the map writes need the mutex.
	decoded := make(map[string]*rsa.PublicKey, len(resp.Data))
	for _, cert := range resp.Data {
		pub, err := decryptPlatformCert(c.cfg.APiv3Key,
			cert.EncryptCertificate.Nonce,
			cert.EncryptCertificate.AssociatedData,
			cert.EncryptCertificate.Ciphertext)
		if err != nil {
			return fmt.Errorf("payment: decrypt platform cert %s: %w", cert.SerialNo, err)
		}
		decoded[cert.SerialNo] = pub
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	for serial, pub := range decoded {
		cc.bySerial[serial] = pub
	}
	return nil
}

// decryptPlatformCert base64-decodes the ciphertext, AES-256-GCM-decrypts it
// with the APIv3 key (key=apiV3Key, nonce, associated_data) to recover the cert
// PEM, then parses the X.509 certificate and extracts its RSA public key.
func decryptPlatformCert(apiV3Key, nonce, associatedData, ciphertextB64 string) (*rsa.PublicKey, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, fmt.Errorf("base64 decode ciphertext: %w", err)
	}
	plain, err := aesGCMDecrypt([]byte(apiV3Key), nonce, associatedData, ciphertext)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(plain)
	if block == nil {
		return nil, fmt.Errorf("decrypted platform cert PEM decode failed")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse platform cert X.509: %w", err)
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("platform cert public key is not RSA")
	}
	return pub, nil
}

// aesGCMDecrypt is the shared AEAD_AES_256_GCM decryptor used for both platform
// certificates and notify resource bodies. key MUST be exactly 32 bytes
// (AES-256, the APIv3 key length); nonce/associatedData come from the envelope.
// ciphertext is the raw (already base64-decoded) GCM ciphertext+tag.
func aesGCMDecrypt(key []byte, nonce, associatedData string, ciphertext []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("payment: api v3 key must be 32 bytes (got %d)", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("payment: aes new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("payment: aes gcm: %w", err)
	}
	plain, err := gcm.Open(nil, []byte(nonce), ciphertext, []byte(associatedData))
	if err != nil {
		return nil, fmt.Errorf("payment: aes gcm open: %w", err)
	}
	return plain, nil
}
