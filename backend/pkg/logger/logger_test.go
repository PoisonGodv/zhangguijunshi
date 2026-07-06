package logger

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/zhangguijunshi/backend/pkg/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// setDefaultForTest swaps the package default logger and returns a cleanup that
// restores the previous value.
func setDefaultForTest(l *zap.Logger) func() {
	prev := defaultLoggerV
	defaultLoggerV = l
	return func() { defaultLoggerV = prev }
}

// tempSink creates a temp file logger target and returns its path plus a
// reader+cleanup. The file is created and closed so zap can (re)open it; the
// returned readFn re-reads the current contents on demand.
func tempSink(t *testing.T) (path string, readFn func() string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "zap-*.log")
	if err != nil {
		t.Fatalf("create temp log: %v", err)
	}
	path = f.Name()
	if err := f.Close(); err != nil {
		t.Fatalf("close temp log: %v", err)
	}
	return path, func() string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read temp log: %v", err)
		}
		return string(b)
	}
}

func TestNew_DefaultsNoError(t *testing.T) {
	// Empty config → sensible defaults (json / info / stdout) and no error.
	l, err := New(config.LogConfig{})
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	if l == nil {
		t.Fatal("New: nil logger")
	}
	_ = l.Sync()
}

func TestNew_InvalidLevel(t *testing.T) {
	if _, err := New(config.LogConfig{Level: "bogus"}); err == nil {
		t.Fatal("New with bogus level should error")
	}
}

func TestNew_InvalidEncoding(t *testing.T) {
	if _, err := New(config.LogConfig{Encoding: "xml"}); err == nil {
		t.Fatal("New with unsupported encoding should error")
	}
}

func TestNew_LevelFilters(t *testing.T) {
	// level=warn must drop debug and info, keep warn and error.
	path, read := tempSink(t)
	l, err := New(config.LogConfig{
		Level:       "warn",
		Encoding:    "json",
		OutputPaths: []string{path},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Debug("dbg-msg")
	l.Info("info-msg")
	l.Warn("warn-msg")
	l.Error("err-msg")
	_ = l.Sync()

	out := read()
	if strings.Contains(out, "dbg-msg") {
		t.Error("debug entry should be filtered at warn level")
	}
	if strings.Contains(out, "info-msg") {
		t.Error("info entry should be filtered at warn level")
	}
	if !strings.Contains(out, "warn-msg") {
		t.Error("warn entry should be present")
	}
	if !strings.Contains(out, "err-msg") {
		t.Error("error entry should be present")
	}
}

func TestNew_JSONEncoding(t *testing.T) {
	path, read := tempSink(t)
	l, err := New(config.LogConfig{
		Level:       "debug",
		Encoding:    "json",
		OutputPaths: []string{path},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info("hello", zap.String(FieldTraceID, "t-1"), zap.Int64(FieldTenantID, 42))
	_ = l.Sync()

	out := strings.TrimSpace(read())
	if out == "" {
		t.Fatal("expected at least one log line")
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(out), &entry); err != nil {
		t.Fatalf("json encoding should produce valid JSON, got %q: %v", out, err)
	}
	if got := entry["msg"]; got != "hello" {
		t.Errorf("msg: got %v, want hello", got)
	}
	if got := entry["level"]; got != "info" {
		t.Errorf("level: got %v, want info", got)
	}
	if got := entry[FieldTraceID]; got != "t-1" {
		t.Errorf("trace_id: got %v, want t-1", got)
	}
	if got := entry[FieldTenantID]; got != float64(42) {
		t.Errorf("tenant_id: got %v, want 42", got)
	}
}

func TestNew_ConsoleEncoding(t *testing.T) {
	path, read := tempSink(t)
	l, err := New(config.LogConfig{
		Level:       "info",
		Encoding:    "console",
		OutputPaths: []string{path},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info("hi-console")
	_ = l.Sync()

	out := strings.TrimSpace(read())
	if out == "" {
		t.Fatal("expected console output")
	}
	// console encoding is NOT a JSON object line; it contains the level token.
	if strings.HasPrefix(out, "{") {
		t.Errorf("console encoding should not start with '{', got %q", out)
	}
	if !strings.Contains(out, "INFO") || !strings.Contains(out, "hi-console") {
		t.Errorf("console output missing INFO/message: %q", out)
	}
}

func TestFromCtx_PicksUpFields(t *testing.T) {
	// Wire an observer as the package default so FromCtx's output is captured.
	core, recorded := observer.New(zapcore.DebugLevel)
	cleanup := setDefaultForTest(zap.New(core))
	defer cleanup()

	ctx := WithFields(context.Background(),
		zap.String(FieldTraceID, "trace-xyz"),
		zap.Int64(FieldTenantID, 7),
		zap.Int64(FieldUserID, 99),
	)
	FromCtx(ctx).Info("strategy.generated")

	if recorded.Len() != 1 {
		t.Fatalf("recorded entries: got %d, want 1", recorded.Len())
	}
	entry := recorded.All()[0]
	if entry.Message != "strategy.generated" {
		t.Errorf("message: got %q, want strategy.generated", entry.Message)
	}
	fields := entry.ContextMap()
	if fields[FieldTraceID] != "trace-xyz" {
		t.Errorf("trace_id: got %v, want trace-xyz", fields[FieldTraceID])
	}
	if fields[FieldTenantID] != int64(7) {
		t.Errorf("tenant_id: got %v (%T), want 7", fields[FieldTenantID], fields[FieldTenantID])
	}
	if fields[FieldUserID] != int64(99) {
		t.Errorf("user_id: got %v, want 99", fields[FieldUserID])
	}
}

func TestFromCtx_NoFieldsUsesDefault(t *testing.T) {
	// No WithFields → FromCtx returns the default logger; logging still works.
	core, recorded := observer.New(zapcore.InfoLevel)
	cleanup := setDefaultForTest(zap.New(core))
	defer cleanup()

	FromCtx(context.Background()).Info("bare")
	if recorded.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", recorded.Len())
	}
	if recorded.All()[0].Message != "bare" {
		t.Errorf("message: got %q, want bare", recorded.All()[0].Message)
	}
	if len(recorded.All()[0].Context) != 0 {
		t.Errorf("expected no context fields, got %v", recorded.All()[0].Context)
	}
}

func TestFromCtx_NilContextSafe(t *testing.T) {
	// FromCtx must not panic on a nil context (returns default logger).
	core, recorded := observer.New(zapcore.InfoLevel)
	cleanup := setDefaultForTest(zap.New(core))
	defer cleanup()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("FromCtx(nil) panicked: %v", r)
		}
	}()
	FromCtx(nil).Info("nil-ctx-ok")
	if recorded.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", recorded.Len())
	}
}

func TestWithFields_MergesAcrossCalls(t *testing.T) {
	core, recorded := observer.New(zapcore.DebugLevel)
	cleanup := setDefaultForTest(zap.New(core))
	defer cleanup()

	ctx := WithFields(context.Background(), zap.String(FieldTraceID, "t1"))
	ctx = WithFields(ctx, zap.Int64(FieldTenantID, 3))
	FromCtx(ctx).Info("merged")

	if recorded.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", recorded.Len())
	}
	fields := recorded.All()[0].ContextMap()
	if fields[FieldTraceID] != "t1" {
		t.Errorf("trace_id: got %v, want t1", fields[FieldTraceID])
	}
	if fields[FieldTenantID] != int64(3) {
		t.Errorf("tenant_id: got %v, want 3", fields[FieldTenantID])
	}
}

func TestWithFields_EmptyReturnsSameContext(t *testing.T) {
	ctx := context.Background()
	if got := WithFields(ctx); got != ctx {
		t.Error("WithFields with no fields should return the same context")
	}
}

func TestParseLevel_Table(t *testing.T) {
	cases := []struct {
		in      string
		want    zapcore.Level
		wantErr bool
	}{
		{"", zapcore.InfoLevel, false},
		{"info", zapcore.InfoLevel, false},
		{"DEBUG", zapcore.DebugLevel, false},
		{"warn", zapcore.WarnLevel, false},
		{"warning", zapcore.WarnLevel, false},
		{"error", zapcore.ErrorLevel, false},
		{"bogus", 0, true},
	}
	for _, c := range cases {
		got, err := parseLevel(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseLevel(%q): want error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseLevel(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseLevel(%q): got %v, want %v", c.in, got, c.want)
		}
	}
}
