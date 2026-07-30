package auth

// password_test.go verifies the bcrypt round-trip and the empty-password guard
// for the staff password-login path (CONTRACT §19 / BE-A-01).

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHashPassword_VerifyRoundTrip(t *testing.T) {
	hash, err := HashPassword("boss12345")
	require.NoError(t, err)
	require.NotEmpty(t, hash)
	// bcrypt hashes carry the cost prefix "$2a$10$" (or "$2b$").
	require.Contains(t, hash, "$2")

	require.NoError(t, VerifyPassword(hash, "boss12345"))
}

func TestHashPassword_DifferentInputsProduceDifferentHashes(t *testing.T) {
	h1, _ := HashPassword("boss12345")
	h2, _ := HashPassword("ops12345")
	require.NotEqual(t, h1, h2)
	// Same input produces a different hash (bcrypt salt) but both verify.
	h1b, _ := HashPassword("boss12345")
	require.NotEqual(t, h1, h1b)
}

func TestVerifyPassword_WrongPasswordFails(t *testing.T) {
	hash, err := HashPassword("boss12345")
	require.NoError(t, err)
	require.Error(t, VerifyPassword(hash, "wrong"))
	require.Error(t, VerifyPassword(hash, ""))
	// Malformed hash must also error (caller collapses to ErrLoginFailed).
	require.Error(t, VerifyPassword("not-a-bcrypt-hash", "boss12345"))
}

func TestHashPassword_RejectsEmpty(t *testing.T) {
	_, err := HashPassword("")
	require.ErrorIs(t, err, errEmptyPassword)
}

func TestHashPassword_UsesConfiguredCost(t *testing.T) {
	h, err := HashPassword("ops12345")
	require.NoError(t, err)
	// cost 10 -> "$2a$10$" prefix.
	require.Contains(t, h, "$2a$10$")
}
