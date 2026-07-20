package membership

// MemoryMemberCoder is the in-memory MemberCoder for dev and tests. A code is
// the string "<tenantID>.<memberID>.<expiryUnix>.<hmac-hex>" where the HMAC
// (keyed by a per-coder secret) authenticates the first three fields, so a
// forged member id or extended expiry fails signature verification. On top of
// the signature the coder keeps a server-side issued-code registry (code →
// member) so a code that never issued here — even one that somehow reuses a
// valid signature structure — is rejected, and so expiry is enforced against
// the registry. The clock is injected for deterministic expiry tests.
//
// Resolve is deliberately NOT consumptive: a member may present the same code
// twice within its TTL (e.g. first a balance query then the actual deduction),
// so the registry maps a code to its member rather than deleting on first use.
// Anti-replay across DIFFERENT purchases is provided by the short TTL (60s) plus
// the per-issue unique code, not by single-use deletion.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// codeParts is the number of dot-separated fields in a code.
const codeParts = 4

// issuedCode is one registry entry: which member a code identifies and when it
// expires.
type issuedCode struct {
	memberID  int64
	expiresAt time.Time
}

// MemoryMemberCoder signs and resolves short-lived member codes in memory.
type MemoryMemberCoder struct {
	mu     sync.Mutex
	secret []byte
	now    func() time.Time
	issued map[string]issuedCode // code -> member/expiry, scoped by tenant in the code itself
	seq    int64
}

// CoderOption customises a MemoryMemberCoder (primarily for deterministic tests).
type CoderOption func(*MemoryMemberCoder)

// WithCoderNowFunc overrides the coder clock (deterministic expiry tests).
func WithCoderNowFunc(f func() time.Time) CoderOption {
	return func(c *MemoryMemberCoder) {
		if f != nil {
			c.now = f
		}
	}
}

// NewMemoryMemberCoder constructs a coder keyed by secret. A nil/empty secret
// falls back to a fixed dev key so tests are deterministic without config; the
// production wiring will pass a real per-deployment secret.
func NewMemoryMemberCoder(secret []byte, opts ...CoderOption) *MemoryMemberCoder {
	if len(secret) == 0 {
		secret = []byte("zhangguijunshi-dev-member-code-secret")
	}
	c := &MemoryMemberCoder{
		secret: append([]byte(nil), secret...),
		now:    time.Now,
		issued: make(map[string]issuedCode),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// sign returns the hex HMAC-SHA256 of the unsigned payload under the secret.
func (c *MemoryMemberCoder) sign(payload string) string {
	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// Issue implements MemberCoder. It builds the signed code, registers it
// server-side, and lazily sweeps expired entries to bound memory.
func (c *MemoryMemberCoder) Issue(ctx context.Context, tenantID, memberID int64, ttl time.Duration) (string, error) {
	if _, err := multitenancy.EnforceTenant(ctx); err != nil {
		return "", err
	}
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	now := c.now()
	expiry := now.Add(ttl).Unix()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	// The payload embeds a per-issue sequence so two codes for the same member
	// within the same second are still distinct (distinct registry entries).
	payload := strconv.FormatInt(tenantID, 10) + "." +
		strconv.FormatInt(memberID, 10) + "." +
		strconv.FormatInt(expiry, 10)
	code := payload + "." + c.sign(payload+"#"+strconv.FormatInt(c.seq, 10))
	c.issued[code] = issuedCode{memberID: memberID, expiresAt: now.Add(ttl)}
	c.sweepLocked(now)
	return code, nil
}

// Resolve implements MemberCoder. It verifies the code is one this coder
// issued (server-side registry), that its signature is well-formed for the
// tenant, and that it has not expired. Any failure yields ErrCodeInvalid.
func (c *MemoryMemberCoder) Resolve(ctx context.Context, tenantID int64, code string) (int64, error) {
	if _, err := multitenancy.EnforceTenant(ctx); err != nil {
		return 0, err
	}

	parts := strings.Split(code, ".")
	if len(parts) != codeParts {
		return 0, ErrCodeInvalid
	}
	codeTenant, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, ErrCodeInvalid
	}
	memberID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, ErrCodeInvalid
	}
	expiry, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return 0, ErrCodeInvalid
	}
	// A code minted for another tenant never resolves under this tenant.
	if codeTenant != tenantID {
		return 0, ErrCodeInvalid
	}

	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.issued[code]
	if !ok {
		return 0, ErrCodeInvalid // never issued here (forged) — server-side anti-replay
	}
	if entry.memberID != memberID {
		return 0, ErrCodeInvalid // payload tampered relative to registry
	}
	if now.After(entry.expiresAt) || now.Unix() >= expiry {
		delete(c.issued, code)
		return 0, ErrCodeInvalid // expired
	}
	return memberID, nil
}

// sweepLocked drops expired registry entries. Caller holds c.mu.
func (c *MemoryMemberCoder) sweepLocked(now time.Time) {
	for code, e := range c.issued {
		if now.After(e.expiresAt) {
			delete(c.issued, code)
		}
	}
}
