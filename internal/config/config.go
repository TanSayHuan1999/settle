// Package config loads settle configuration from environment variables.
package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
)

// Config holds all runtime configuration for the service.
type Config struct {
	DatabaseURL string
	Port        int
}

// Load reads configuration from the environment, applying defaults.
// DATABASE_URL is required; PORT defaults to 8080.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		Port:        envInt("PORT", 8080),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return Config{}, fmt.Errorf("invalid PORT %d", cfg.Port)
	}
	return cfg, nil
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("WARNING: invalid %s=%q, using default %d", key, v, def)
		return def
	}
	return n
}
