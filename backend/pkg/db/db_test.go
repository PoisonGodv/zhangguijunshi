package db

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// These tests never touch a real database: the InTx behaviour is exercised
// through a pgxmock pool mock, and DSN construction is verified by calling the
// internal buildDSN/maskDSN helpers directly.

func TestBuildDSN(t *testing.T) {
	cfg := config.PostgresConfig{
		Host:     "db.internal",
		Port:     5432,
		User:     "zg_app",
		Password: "s3cr3t!@#",
		Database: "zhanggui",
		SSLMode:  "disable",
	}

	dsn := buildDSN(cfg)

	// The DSN must be a well-formed URL.
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Equal(t, "postgres", u.Scheme)
	require.Equal(t, "db.internal:5432", u.Host)

	// Key fields round-trip through percent-encoding.
	require.Equal(t, "zg_app", u.User.Username())
	pw, ok := u.User.Password()
	require.True(t, ok)
	require.Equal(t, "s3cr3t!@#", pw)
	require.Equal(t, "/zhanggui", u.Path)

	// sslmode is always emitted.
	require.Equal(t, "disable", u.Query().Get("sslmode"))
	require.Contains(t, dsn, "sslmode=disable")

	// pgxpool must accept the produced DSN (pure parsing, no connection).
	_, err = pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
}

func TestBuildDSN_DefaultSSLMode(t *testing.T) {
	// An unset SSLMode must still yield an explicit sslmode query param.
	dsn := buildDSN(config.PostgresConfig{
		Host: "localhost", Port: 5432, User: "u", Password: "p", Database: "d",
	})
	require.Contains(t, dsn, "sslmode=prefer")
}

func TestMaskDSN_HidesPassword(t *testing.T) {
	cfg := config.PostgresConfig{
		Host: "db.internal", Port: 5432, User: "zg_app",
		Password: "super-secret", Database: "zhanggui", SSLMode: "disable",
	}
	masked := maskDSN(buildDSN(cfg))

	require.NotContains(t, masked, "super-secret", "real password must not leak into the masked DSN")
	require.Contains(t, masked, "***")
	require.Contains(t, masked, "zg_app", "username is preserved")

	u, err := url.Parse(masked)
	require.NoError(t, err)
	gotPW, ok := u.User.Password()
	require.True(t, ok)
	require.Equal(t, "***", gotPW)
}

func TestInTx_Commit(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectCommit()

	calls := 0
	err = execInTx(context.Background(), mock, pgx.TxOptions{}, func(tx pgx.Tx) error {
		calls++
		require.NotNil(t, tx, "fn must receive a non-nil transaction")
		return nil
	})

	require.NoError(t, err)
	require.Equal(t, 1, calls, "fn must run exactly once")
	require.NoError(t, mock.ExpectationsWereMet(), "Begin then Commit must have been called")
}

func TestInTx_RollbackOnError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectRollback()

	fnErr := errors.New("business rule violated")
	err = execInTx(context.Background(), mock, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return fnErr
	})

	require.ErrorIs(t, err, fnErr, "the original fn error must be preserved for errors.Is")
	require.NoError(t, mock.ExpectationsWereMet(), "Begin then Rollback must have been called")
}

func TestInTx_RollbackOnPanic(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectRollback()

	panicked := false
	var retErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		retErr = execInTx(context.Background(), mock, pgx.TxOptions{}, func(tx pgx.Tx) error {
			panic("boom")
		})
	}()

	require.False(t, panicked, "InTx must NOT re-panic; it must convert the panic into an error")
	require.Error(t, retErr, "InTx must return an error when fn panics")
	require.Contains(t, retErr.Error(), "boom")
	require.NoError(t, mock.ExpectationsWereMet(), "Begin then Rollback must have been called even on panic")
}
