package auth

// password.go wraps bcrypt for the boss/admin password-login flow (CONTRACT §19
// / BE-A-01). cost 10 balances ~60ms hash time on commodity hardware against
// offline-cracking resistance; the same cost is used by every (future) account
// seeding path so Verify always matches Hash.
//
// bcrypt is an indirect dependency (golang.org/x/crypto) already in go.mod; it
// is imported directly here without a go.mod edit (indirect deps are importable).

import (
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is the bcrypt cost used for all staff password hashes.
const bcryptCost = 10

// errEmptyPassword guards the seed path against hashing an empty password
// (bcrypt would otherwise happily hash "" and create a login-with-no-password
// account). HashPassword is internal-only for M5 Phase 1 (memory seed / future
// seeding job), so a plain error suffices.
var errEmptyPassword = errors.New("auth: empty password")

// dummyPasswordHash is a valid bcrypt hash compared against when a login lookup
// finds no usable credential (account not found / role mismatch / empty stored
// hash). Running one real bcrypt comparison on these paths keeps the response
// time statistically indistinguishable from the "account exists, wrong password"
// path, closing the user-enumeration timing side channel. It is a timing pad,
// never a real credential; the comparison always fails.
var dummyPasswordHash = func() string {
	h, err := HashPassword("timing-equalization-dummy-not-a-real-credential")
	if err != nil {
		// bcrypt broken at init: fall back to a static valid hash so Verify still
		// runs a full comparison rather than erroring out fast.
		return "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	}
	return h
}()

// HashPassword returns a bcrypt hash of pwd at cost 10. Empty passwords are
// rejected. Callers store the result in users.password_hash.
func HashPassword(pwd string) (string, error) {
	if pwd == "" {
		return "", errEmptyPassword
	}
	b, err := bcrypt.GenerateFromPassword([]byte(pwd), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// VerifyPassword returns nil iff pwd matches the stored bcrypt hash. Any
// mismatch (or malformed/empty hash) yields a non-nil error; callers MUST
// collapse every non-nil outcome into ErrLoginFailed so the response is
// constant and free of timing/user-enumeration side channels.
func VerifyPassword(hash, pwd string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pwd))
}
