// Package logger provides a zap-based structured logger with context-aware
// field propagation.
//
// Field names are fixed across the codebase so that the log shipper (CLS) can
// index them consistently (CONTRACT §4.3):
//
//	trace_id, tenant_id, merchant_id, store_id, user_id
//
// Typical use:
//
//	log := logger.FromCtx(ctx)            // picks up trace_id/tenant_id/...
//	log.Info("strategy.generated",
//	    zap.String("action_id", s.ID),
//	    zap.Int64("tenant_id", s.TenantID))
//
// Handlers seed the context once per request:
//
//	ctx = logger.WithFields(ctx,
//	    zap.String(logger.FieldTraceID, traceID),
//	    zap.Int64(logger.FieldTenantID, tenantID))
package logger

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/zhangguijunshi/backend/pkg/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Canonical field names (CONTRACT §4.3). Use these constants instead of raw
// strings so renames are caught by the compiler.
const (
	FieldTraceID    = "trace_id"
	FieldTenantID   = "tenant_id"
	FieldMerchantID = "merchant_id"
	FieldStoreID    = "store_id"
	FieldUserID     = "user_id"
)

// ctxKey is the unexported context key under which WithFields stores zap
// fields. Unexported so callers cannot tamper with it directly; they must go
// through WithFields/FromCtx (CONTRACT §1.3.4 forbids bare context.WithValue
// for log/tenant plumbing).
type ctxKey struct{}

// ctxFields is the value stored under ctxKey.
type ctxFields struct {
	fields []zap.Field
}

// New builds a zap.Logger from the LogConfig layer of the shared Config.
//
// cfg.Level selects the minimum level (debug|info|warn|error; empty→info).
// cfg.Encoding selects the wire format ("json" or "console"; empty→json).
// cfg.OutputPaths selects sinks (defaults to ["stdout"]).
//
// Returns an error for an unknown level, an unsupported encoding, or a sink
// that zap cannot open — surfacing misconfiguration loudly at startup.
func New(cfg config.LogConfig) (*zap.Logger, error) {
	encoding := strings.ToLower(strings.TrimSpace(cfg.Encoding))
	switch encoding {
	case "":
		encoding = "json"
	case "json", "console":
		// ok
	default:
		return nil, fmt.Errorf("logger: unsupported encoding %q (want json|console)", cfg.Encoding)
	}

	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	outputs := cfg.OutputPaths
	if len(outputs) == 0 {
		outputs = []string{"stdout"}
	}

	zcfg := zap.Config{
		Level:             zap.NewAtomicLevelAt(level),
		Encoding:          encoding,
		OutputPaths:       outputs,
		ErrorOutputPaths:  []string{"stderr"},
		DisableStacktrace: true,
	}
	if encoding == "console" {
		zcfg.EncoderConfig = zap.NewDevelopmentEncoderConfig()
	} else {
		zcfg.EncoderConfig = zap.NewProductionEncoderConfig()
	}

	l, err := zcfg.Build()
	if err != nil {
		return nil, fmt.Errorf("logger: build: %w", err)
	}
	return l, nil
}

// parseLevel maps the config string to a zapcore.Level. An empty string is
// treated as the safe default "info"; any other unrecognised value is an error
// so typos surface at startup.
func parseLevel(s string) (zapcore.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return zapcore.InfoLevel, nil
	case "debug":
		return zapcore.DebugLevel, nil
	case "warn", "warning":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	case "dpanic":
		return zapcore.DPanicLevel, nil
	case "panic":
		return zapcore.PanicLevel, nil
	case "fatal":
		return zapcore.FatalLevel, nil
	default:
		return 0, fmt.Errorf("logger: unknown level %q", s)
	}
}

// WithFields returns a derived context that carries the given zap fields so a
// later FromCtx(ctx) attaches them to every log line. Repeated calls merge with
// fields already present in ctx (later fields win on key collision at log time,
// matching zap's With semantics).
//
// Passing no fields returns ctx unchanged.
func WithFields(ctx context.Context, fields ...zap.Field) context.Context {
	if len(fields) == 0 {
		return ctx
	}
	merged := make([]zap.Field, 0, len(fields))
	if existing, ok := ctx.Value(ctxKey{}).(*ctxFields); ok && existing != nil {
		merged = append(merged, existing.fields...)
	}
	merged = append(merged, fields...)
	return context.WithValue(ctx, ctxKey{}, &ctxFields{fields: merged})
}

// fieldsFromCtx returns the zap fields stored under ctxKey, or nil.
func fieldsFromCtx(ctx context.Context) []zap.Field {
	if ctx == nil {
		return nil
	}
	if cf, ok := ctx.Value(ctxKey{}).(*ctxFields); ok && cf != nil {
		return cf.fields
	}
	return nil
}

// FromCtx returns a logger enriched with the fields stored in ctx (via
// WithFields). When ctx has no fields, or the context is nil, it returns the
// package default logger unchanged. The package default is set with SetDefault
// (typically once at startup from the loaded config); if never set it falls
// back to a console logger on stderr so logs are never silently lost.
func FromCtx(ctx context.Context) *zap.Logger {
	l := Default()
	fs := fieldsFromCtx(ctx)
	if len(fs) == 0 {
		return l
	}
	return l.With(fs...)
}

// Default returns the package default logger, honouring an explicit SetDefault.
func Default() *zap.Logger {
	defaultMu.RLock()
	l := defaultLoggerV
	defaultMu.RUnlock()
	if l != nil {
		return l
	}
	return fallbackLogger()
}

// SetDefault overrides the package default logger used by FromCtx when the
// context carries no embedded logger. main calls this once after building the
// logger from the loaded config.
func SetDefault(l *zap.Logger) {
	defaultMu.Lock()
	defaultLoggerV = l
	defaultMu.Unlock()
}

var (
	defaultMu      sync.RWMutex
	defaultLoggerV *zap.Logger

	fallbackOnce sync.Once
	fallbackLog  *zap.Logger
)

// fallbackLogger lazily builds a console logger to stderr; used only if
// SetDefault is never called. Errors fall back to a Nop logger so the package
// is always safe to use.
func fallbackLogger() *zap.Logger {
	fallbackOnce.Do(func() {
		l, err := New(config.LogConfig{
			Level:       "info",
			Encoding:    "console",
			OutputPaths: []string{"stderr"},
		})
		if err != nil || l == nil {
			l = zap.NewNop()
		}
		fallbackLog = l
	})
	return fallbackLog
}
