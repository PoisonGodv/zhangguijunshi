// Package config defines the layered configuration model for the backend.
//
// The struct surface below is the SHARED CONTRACT consumed by every package
// (logger, db, http, multitenancy). Only the type definitions live here; the
// loading logic (viper) lives in config.go, authored separately. Do NOT change
// these struct shapes without updating CONTRACT.md and downstream packages.
package config

import "time"

// Config is the root configuration loaded from layered sources
// (defaults → configs/<env>.yaml → env vars prefixed ZG_, underscore-nested).
type Config struct {
	App          AppConfig          `mapstructure:"app"`
	HTTP         HTTPConfig         `mapstructure:"http"`
	Postgres     PostgresConfig     `mapstructure:"postgres"`
	Redis        RedisConfig        `mapstructure:"redis"`
	Log          LogConfig          `mapstructure:"log"`
	JWT          JWTConfig          `mapstructure:"jwt"`
	OTel         OTelConfig         `mapstructure:"otel"`
	COS          COSConfig          `mapstructure:"cos"`
	Multitenancy MultitenancyConfig `mapstructure:"multitenancy"`
}

type AppConfig struct {
	Name    string `mapstructure:"name"`
	Env     string `mapstructure:"env"` // dev | staging | prod
	Version string `mapstructure:"version"`
}

type HTTPConfig struct {
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

type PostgresConfig struct {
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	User            string        `mapstructure:"user"`
	Password        string        `mapstructure:"password"`
	Database        string        `mapstructure:"database"`
	SSLMode         string        `mapstructure:"ssl_mode"`
	MaxConns        int32         `mapstructure:"max_conns"`
	MinConns        int32         `mapstructure:"min_conns"`
	MaxConnLifetime time.Duration `mapstructure:"max_conn_lifetime"`
	MaxConnIdleTime time.Duration `mapstructure:"max_conn_idle_time"`
}

type RedisConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
	PoolSize int    `mapstructure:"pool_size"`
}

type LogConfig struct {
	Level       string   `mapstructure:"level"`    // debug | info | warn | error
	Encoding    string   `mapstructure:"encoding"` // json | console
	OutputPaths []string `mapstructure:"output_paths"`
}

type JWTConfig struct {
	Secret     string        `mapstructure:"secret"`
	AccessTTL  time.Duration `mapstructure:"access_ttl"`
	RefreshTTL time.Duration `mapstructure:"refresh_ttl"`
	Issuer     string        `mapstructure:"issuer"`
}

type OTelConfig struct {
	Enable      bool    `mapstructure:"enable"`
	Endpoint    string  `mapstructure:"endpoint"`
	ServiceName string  `mapstructure:"service_name"`
	SampleRatio float64 `mapstructure:"sample_ratio"`
	Insecure    bool    `mapstructure:"insecure"`
}

type COSConfig struct {
	SecretID  string `mapstructure:"secret_id"`
	SecretKey string `mapstructure:"secret_key"`
	Region    string `mapstructure:"region"`
	Bucket    string `mapstructure:"bucket"`
	Endpoint  string `mapstructure:"endpoint"`
}

// MultitenancyConfig governs the row-level isolation guard (ARCH-01).
type MultitenancyConfig struct {
	Strict bool `mapstructure:"strict"` // hard-enforce tenant on every repo call
}
