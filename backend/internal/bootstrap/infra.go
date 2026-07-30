package bootstrap

// infra.go holds the auth-infrastructure builders shared by every binary
// (tech plan §6.2): the ES256 JWT manager and the session store. Both degrade
// gracefully — a dev box with no Redis still issues verifiable tokens.

import (
	"crypto/ecdsa"

	"go.uber.org/zap"

	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/jwt"
	"github.com/zhangguijunshi/backend/pkg/redisx"
)

// buildJWTManager constructs the ES256 signer/verifier. It loads a PEM private
// key from cfg.JWT.KeyFile when set; otherwise it derives a deterministic dev
// key (logged loudly) so a dev server still issues verifiable tokens with no
// configuration.
func buildJWTManager(cfg *config.Config, log *zap.Logger) (*jwt.Manager, error) {
	var priv *ecdsa.PrivateKey
	if cfg.JWT.KeyFile != "" {
		p, err := jwt.LoadPrivateKeyFile(cfg.JWT.KeyFile)
		if err != nil {
			return nil, err
		}
		priv = p
		log.Info("jwt key loaded from file", zap.String("key_file", cfg.JWT.KeyFile))
	} else {
		p, err := jwt.DevKey(jwt.DevSeed)
		if err != nil {
			return nil, err
		}
		priv = p
		log.Warn("using derived dev JWT key — DO NOT USE IN PRODUCTION",
			zap.String("kid", cfg.JWT.KeyID),
			zap.String("hint", "set jwt.key_file to a PEM ECDSA P-256 private key in staging/prod"),
		)
	}
	return jwt.New(jwt.Config{
		PrivateKey: priv,
		KeyID:      cfg.JWT.KeyID,
		Issuer:     cfg.JWT.Issuer,
		AccessTTL:  cfg.JWT.AccessTTL,
	})
}

// buildSessionStore returns a Redis-backed SessionStore when a Redis client is
// available, otherwise an in-memory store. The caller (BuildInfra) has already
// verified reachability; a nil rdb means Redis is unavailable.
func buildSessionStore(rdb *redisx.Client) jwt.SessionStore {
	if rdb == nil {
		return jwt.NewMemorySessionStore()
	}
	return jwt.NewRedisSessionStore(rdb.Client)
}
