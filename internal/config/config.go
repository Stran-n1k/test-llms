// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"net"
	"os"
	"strings"
	"time"
)

// Config is the process configuration.
type Config struct {
	// DatabaseURL is a PostgreSQL connection string (DATABASE_URL).
	DatabaseURL string
	// HTTPAddr is the address the HTTP server listens on (HTTP_ADDR).
	HTTPAddr string
	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout time.Duration
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:     strings.TrimSpace(os.Getenv("DATABASE_URL")),
		HTTPAddr:        strings.TrimSpace(os.Getenv("HTTP_ADDR")),
		ShutdownTimeout: 10 * time.Second,
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":8080"
	}
	if _, _, err := net.SplitHostPort(cfg.HTTPAddr); err != nil {
		return Config{}, errors.New("HTTP_ADDR must be in host:port form: " + err.Error())
	}
	return cfg, nil
}
